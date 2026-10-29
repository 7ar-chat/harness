package artifacts

import (
	"context"
	"errors"
	"path"
	"strings"
	"time"

	"github.com/GhaziBenDahmane/harness/core/sandbox"
	"github.com/GhaziBenDahmane/harness/core/store"
)

// RecordPath records the current state of one workspace file of the thread's
// sandbox after it was changed outside a run (a checkpoint restore): a new
// version when it exists (created or updated), a deletion of its artifact
// when it no longer does. It returns the change (nil when there is none, e.g.
// a file above the snapshot limits or under /workspace/.work).
func (s *Service) RecordPath(ctx context.Context, sb sandbox.Sandbox, threadID, login, p string) (*Change, error) {
	p = path.Clean(p)
	if !strings.HasPrefix(p, sandbox.Workspace+"/") || InSkippedDir(p) {
		return nil, nil // never an artifact (the snapshot listing skips it; C-22)
	}
	unlock, err := s.lockThread(ctx, threadID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	existing, err := s.Store.ListArtifacts(ctx, threadID)
	if err != nil {
		return nil, err
	}
	var current *store.Artifact
	for _, a := range existing {
		if a.Path == p {
			current = a
		}
	}
	info, err := sb.Stat(ctx, p)
	if errors.Is(err, sandbox.ErrNotFound) {
		if current == nil || current.Deleted {
			return nil, nil
		}
		// The file is known to be gone from this sandbox: under the thread lock
		// nothing else changes the artifact meanwhile.
		if err := s.Store.MarkArtifactsMaterialized(ctx, sb.ID(), []string{current.ID}); err != nil {
			return nil, err
		}
		clock, err := s.Store.Clock(ctx)
		if err != nil {
			return nil, err
		}
		got, err := s.Store.MarkArtifactDeleted(ctx, current.ID, "", sb.ID(), clock.Add(time.Second))
		if err != nil {
			return nil, err
		}
		return &Change{Artifact: got, Change: store.ChangeDeleted}, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Type == "directory" || info.Size > s.Opts.MaxFileBytes || (s.Blobs.MaxBytes() > 0 && info.Size > s.Blobs.MaxBytes()) {
		return nil, nil
	}
	data, size, err := sb.ReadFile(ctx, p, s.Opts.MaxFileBytes)
	if err != nil {
		return nil, err
	}
	if size > int64(len(data)) {
		return nil, nil
	}
	sum := sha(data)
	if err := s.putBlob(ctx, sum, data); err != nil {
		return nil, err
	}
	name := path.Base(p)
	var mtime float64
	if !info.ModifiedAt.IsZero() {
		mtime = float64(info.ModifiedAt.UnixNano()) / 1e9
	}
	got, change, err := s.put(ctx, store.PutArtifact{ThreadID: threadID, UserLogin: login, Path: p, Name: name,
		MimeType: ContentMime(name, data), Kind: store.ArtifactOutput, SizeBytes: int64(len(data)), SHA256: sum,
		Mtime: mtime, SandboxID: sb.ID()})
	if err != nil || change == "" {
		return nil, err
	}
	return &Change{Artifact: got, Change: change}, nil
}
