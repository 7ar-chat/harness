// Package renders guards the files derived from sandbox documents (harness v3
// "Review round 1", P2 #2): view_file page images, read_file text extraction
// and Office previews drop the source's confidentiality label, so a source
// labelled C4 / C3-Amundi-Only is refused (CheckSource), and the page images
// view_file writes are recorded with their source's label (RecordRender) so
// GET /threads/{id}/renders/{renderId}/{page}.png only serves allowed ones.
package renders

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"regexp"

	"github.com/GhaziBenDahmane/harness/classification"
	"github.com/GhaziBenDahmane/harness/sandbox"
	"github.com/GhaziBenDahmane/harness/store"
)

// MaxSourceBytes bounds the source read to check its label (the attachment
// limit); a larger source is refused (its label cannot be checked).
const MaxSourceBytes = 100 << 20

// NotAllowedMessage is the tool error of a refused source.
const NotAllowedMessage = "this document is labelled C4 or C3-Amundi-Only (or its label cannot be read): it cannot be rendered, converted or extracted"

// ValidID matches a render id: the render directory name under sandbox.RendersDir.
var ValidID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// CheckSource reads a sandbox document and returns its most restrictive label,
// or classification.ErrSourceNotAllowed (wrapped) when no derived file may be
// produced from it. sandbox.ErrNotFound and sandbox errors are returned as is.
func CheckSource(ctx context.Context, sb sandbox.Sandbox, p string) (label string, err error) {
	data, size, err := sb.ReadFile(ctx, p, MaxSourceBytes)
	if err != nil {
		return "", err
	}
	if size > int64(len(data)) {
		return "", fmt.Errorf("%w: %s is larger than %d MiB, its label cannot be checked", classification.ErrSourceNotAllowed, p, MaxSourceBytes>>20)
	}
	if !classification.SourceAllowed(data) {
		return classification.ConfidentialityLabel(p, data), classification.ErrSourceNotAllowed
	}
	return classification.ConfidentialityLabel(p, data), nil
}

// IsNotAllowed reports a CheckSource refusal.
func IsNotAllowed(err error) bool { return errors.Is(err, classification.ErrSourceNotAllowed) }

// Service records renders (the sandbox tools' sandboxtools.RenderRecorder).
type Service struct {
	Store     store.RenderStore
	Sandboxes sandbox.Manager
}

// New returns the render recorder (nil without a store or sandboxes).
func New(st store.RenderStore, mgr sandbox.Manager) *Service {
	if st == nil || mgr == nil || !mgr.Enabled() {
		return nil
	}
	return &Service{Store: st, Sandboxes: mgr}
}

// RecordRender records that view_file rendered pages of sourcePath into
// sandbox.RendersDir/renderID (contract: harness v3 "Review round 1"). The
// source is read again from the thread's sandbox to store its label; a source
// that cannot be read or checked is recorded as not allowed (fail closed).
func (s *Service) RecordRender(ctx context.Context, threadID, sourcePath, renderID string, pages []int) error {
	if s == nil {
		return nil
	}
	if !ValidID.MatchString(renderID) || renderID == "." || renderID == ".." {
		return fmt.Errorf("renders: invalid render id %q", renderID)
	}
	r := &store.Render{ThreadID: threadID, RenderID: renderID, SourcePath: path.Clean(sourcePath), Pages: pages}
	sb, err := s.Sandboxes.Get(ctx, threadID)
	if err == nil {
		r.Label, err = CheckSource(ctx, sb, r.SourcePath)
	}
	r.Allowed = err == nil
	if err != nil && !IsNotAllowed(err) {
		slog.Warn("renders: the source label could not be checked; render recorded as not allowed", "thread_id", threadID, "error", err)
	}
	return s.Store.PutRender(ctx, r)
}
