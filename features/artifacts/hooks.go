package artifacts

import (
	"context"
	"encoding/base64"
	"log/slog"
	"strings"

	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/agent"
)

// FileReferencePrompt is the §2.3 instruction (workspace-file: links).
const FileReferencePrompt = `File references: whenever your answer mentions a file you created, changed or read under /workspace, write it as a markdown link with the workspace-file scheme and the absolute path, e.g. [Quarterly report](workspace-file:///workspace/reports/q3.docx). The user's interface turns these links into file chips that open the document. Files the user attached are saved at the root of /workspace (each message lists their paths). Put final deliverables in /workspace/outputs/ (or where the user asked). Only link files that exist: check them with a tool first. Never link a file under /workspace/.work/ (scratch space, never saved): copy or save it under /workspace/outputs/ first. Files larger than 50 MiB are not saved either (they are lost when the sandbox stops): keep each deliverable under 50 MiB, or split or compress it.`

// PromptSection adds the file-reference instruction when the run has a sandbox
// (an agent.PromptSection).
func PromptSection(_ context.Context, in agent.Input) string {
	if !in.Sandbox {
		return ""
	}
	return FileReferencePrompt
}

const (
	maxImageBytes    = 5 << 20
	maxImagesPerRun  = agent.MaxRequestImages // the agent's per-request image cap
	maxImageMessages = 2                      // the most recent user messages whose images are sent
)

var visionMIME = map[string]bool{"image/png": true, "image/jpeg": true, "image/webp": true, "image/gif": true}

// Images sends attached images (png/jpeg/webp/gif ≤ 5 MiB) to vision models as
// image parts: the images of the two most recent user messages that have
// some, at most agent.MaxRequestImages (6) per model call.
type Images struct {
	Svc *Service
	// Vision reports whether a model accepts image input (LLM_VISION_MODELS).
	Vision func(model string) bool
}

// AttachImages implements agent.ImageAttacher.
func (im Images) AttachImages(ctx context.Context, run *store.Run, history []*ai.Message) []*ai.Message {
	if im.Svc == nil || im.Vision == nil || !im.Vision(run.Model) {
		return history
	}
	budget, messages := maxImagesPerRun, 0
	for i := len(history) - 1; i >= 0 && budget > 0 && messages < maxImageMessages; i-- {
		m := history[i]
		atts, _ := m.Extra[store.ExtraAttachments].([]store.Attachment)
		if m.Role != ai.RoleUser || len(atts) == 0 {
			continue
		}
		var parts []ai.Part
		for _, a := range atts {
			if budget == 0 || !visionMIME[a.MimeType] || a.SizeBytes > maxImageBytes {
				continue
			}
			data, mime := im.load(ctx, run, a)
			if data == nil {
				continue
			}
			b64 := base64.StdEncoding.EncodeToString(data)
			parts = append(parts, ai.Part{Type: ai.PartImage,
				Image: &ai.Image{Base64Data: b64, MIMEType: mime},
				Extra: map[string]any{agent.ImageAttachmentKey: a.ArtifactID}}) // its stub names this file
			budget--
		}
		if len(parts) == 0 {
			continue
		}
		messages++
		m.Parts = append([]ai.Part{{Type: ai.PartText, Text: m.Content}}, parts...)
		m.Content = ""
	}
	return history
}

func (im Images) load(ctx context.Context, run *store.Run, att store.Attachment) ([]byte, string) {
	a, err := im.Svc.Store.GetArtifact(ctx, att.ArtifactID)
	if err != nil || a.ThreadID != run.ThreadID {
		return nil, ""
	}
	data, _, err := im.Svc.Content(ctx, a, att.Version)
	if err != nil || len(data) > maxImageBytes {
		if err != nil {
			slog.Warn("artifacts: loading attached image failed", "artifact_id", a.ID, "error", err)
		}
		return nil, ""
	}
	mime := a.MimeType
	if !strings.HasPrefix(mime, "image/") {
		return nil, ""
	}
	return data, mime
}
