package harness

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/GhaziBenDahmane/harness/engine/agent"
	"github.com/GhaziBenDahmane/harness/features/artifacts"
	"github.com/GhaziBenDahmane/harness/store"
)

// RunMode is RunOptions.Mode.
type RunMode string

const (
	// ModeBuild (the default): the run has all the tools its profile allows.
	ModeBuild RunMode = "build"
	// ModePlan: the run is read-only. The engine offers it only tools without
	// side effects (reads, searches, read-only MCP tools) and no command
	// execution, and its prompt asks for an investigation and a plan. A resumed
	// run keeps the mode of the thread's previous run.
	ModePlan RunMode = "plan"
)

// runMode validates a run's mode and returns the stored value.
func runMode(m RunMode) (string, error) {
	switch m {
	case "", ModeBuild:
		return store.ModeBuild, nil
	case ModePlan:
		return store.ModePlan, nil
	}
	return "", fmt.Errorf("%w: mode must be build or plan, not %q", ErrInvalidRequest, m)
}

// Limits of RunOptions.Attachments.
const (
	// MaxAttachments is the most attachments one message carries (the number of
	// images one model request carries).
	MaxAttachments = agent.MaxRequestImages
	// MaxAttachmentBytes is the largest attachment (the largest image a vision
	// model is sent).
	MaxAttachmentBytes = 5 << 20
)

// Attachment is an image sent with a message. It is stored in the thread's
// workspace like an uploaded file and shown to vision models; a model without
// vision sees the file's path only.
type Attachment struct {
	// Name is the file name (its extension is corrected to MIME's).
	Name string
	// MIME is image/png, image/jpeg, image/gif or image/webp.
	MIME string
	Data []byte
}

var attachmentExt = map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp"}

// checkAttachments validates the attachments' count, type and size.
func checkAttachments(atts []Attachment) error {
	if len(atts) > MaxAttachments {
		return fmt.Errorf("%w: at most %d attachments per message", ErrInvalidRequest, MaxAttachments)
	}
	for _, a := range atts {
		if _, ok := attachmentExt[strings.ToLower(strings.TrimSpace(a.MIME))]; !ok {
			return fmt.Errorf("%w: attachment %q: only image/png, image/jpeg, image/gif and image/webp are accepted", ErrInvalidRequest, a.Name)
		}
		if len(a.Data) == 0 {
			return fmt.Errorf("%w: attachment %q is empty", ErrInvalidRequest, a.Name)
		}
		if len(a.Data) > MaxAttachmentBytes {
			return fmt.Errorf("%w: attachment %q exceeds %d MiB", ErrInvalidRequest, a.Name, MaxAttachmentBytes>>20)
		}
	}
	return nil
}

// storeAttachments stores the (validated) attachments as uploads of the thread
// and returns the message metadata that references them.
func (h *Harness) storeAttachments(ctx context.Context, user, threadID string, atts []Attachment) (map[string]any, error) {
	if len(atts) == 0 {
		return nil, nil
	}
	if h.Artifacts == nil {
		return nil, fmt.Errorf("%w: this harness stores no attachments", ErrInvalidRequest)
	}
	list := make([]store.Attachment, 0, len(atts))
	for _, a := range atts {
		mime := strings.ToLower(strings.TrimSpace(a.MIME))
		name := artifacts.SanitizeFilename(a.Name)
		if ext := path.Ext(name); !strings.EqualFold(ext, attachmentExt[mime]) && !(mime == "image/jpeg" && strings.EqualFold(ext, ".jpeg")) {
			name = strings.TrimSuffix(name, ext) + attachmentExt[mime]
		}
		art, err := h.Artifacts.Upload(ctx, artifacts.UploadInput{ThreadID: threadID, Login: user, Filename: name, DeclaredMIME: mime, Data: a.Data})
		if err != nil {
			if errors.Is(err, artifacts.ErrTypeMismatch) || errors.Is(err, artifacts.ErrTooLarge) {
				return nil, fmt.Errorf("%w: attachment %q: %v", ErrInvalidRequest, a.Name, err)
			}
			return nil, err
		}
		list = append(list, store.Attachment{ArtifactID: art.ID, Name: art.Name, Path: art.Path, SizeBytes: art.SizeBytes, MimeType: art.MimeType, Version: art.Version})
	}
	return map[string]any{"attachments": list}, nil
}
