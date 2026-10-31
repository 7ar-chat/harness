package runtime_test

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/ai/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/features/artifacts"
)

var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDRfake")

// attachedRun queues a run whose user message attaches the given uploads.
func (r *scriptedRig) attachedRun(files map[string][]byte) *store.Run {
	return r.attachedRunWith("Look at this", files)
}

func (r *scriptedRig) attachedRunWith(content string, files map[string][]byte) *store.Run {
	r.t.Helper()
	ctx := context.Background()
	th, _ := r.st.CreateThread(ctx, "alice", "t")
	var items []any
	for name, data := range files {
		a, err := r.arts.Upload(ctx, artifacts.UploadInput{ThreadID: th.ID, Login: "alice", Filename: name, Data: data})
		if err != nil {
			r.t.Fatal(err)
		}
		items = append(items, map[string]any{"artifactId": a.ID, "name": a.Name, "path": a.Path, "sizeBytes": a.SizeBytes, "mimeType": a.MimeType, "version": a.Version})
	}
	run, _, err := r.st.CreateRun(ctx, store.NewRun{RunID: newID(), ThreadID: th.ID, UserLogin: "alice", Model: "gpt-test",
		Message: store.Message{Role: store.RoleUser, Content: content, Metadata: map[string]any{"attachments": items}}})
	if err != nil {
		r.t.Fatal(err)
	}
	return run
}

func TestAttachedFilesAreListedAfterTheUserText(t *testing.T) {
	// The model receives "[Attached files: …]" after the text; without vision no image part is sent.
	// Arrange
	r := newScriptedRig(t, false, llmtest.Turn{Chunks: llmtest.Text("Seen")})
	run := r.attachedRun(map[string][]byte{"data.csv": []byte("a,b")})

	// Act
	r.process(run)

	// Assert
	msgs := r.model.Calls()[0].Messages
	user := msgs[len(msgs)-1]
	if user.Role != ai.RoleUser || user.Content != "Look at this\n\n[Attached files: /workspace/data.csv]" || len(user.Parts) != 0 {
		t.Fatalf("user message %+v", user)
	}
}

func TestVisionModelsReceiveAttachedImagesAsImageParts(t *testing.T) {
	// For LLM_VISION_MODELS an attached image is sent as a base64 image part next to the text.
	// Arrange
	r := newScriptedRig(t, true, llmtest.Turn{Chunks: llmtest.Text("A chart")})
	run := r.attachedRun(map[string][]byte{"chart.png": pngBytes})

	// Act
	r.process(run)

	// Assert
	msgs := r.model.Calls()[0].Messages
	parts := msgs[len(msgs)-1].Parts
	if len(parts) != 2 || parts[0].Type != ai.PartText || !strings.HasSuffix(parts[0].Text, "[Attached files: /workspace/chart.png]") {
		t.Fatalf("parts %+v", parts)
	}
	img := parts[1].Image
	if parts[1].Type != ai.PartImage || img == nil || img.MIMEType != "image/png" ||
		img.Base64Data != base64.StdEncoding.EncodeToString(pngBytes) {
		t.Fatalf("image part %+v", parts[1])
	}
}

func TestAttachmentsWithoutTextSendOnlyTheAttachedFilesLine(t *testing.T) {
	// A message made only of attachments reaches the model as the attached-files line.
	// Arrange
	r := newScriptedRig(t, false, llmtest.Turn{Chunks: llmtest.Text("Seen")})
	run := r.attachedRunWith("", map[string][]byte{"data.csv": []byte("a,b")})

	// Act
	r.process(run)

	// Assert
	sent := r.model.Calls()[0].Messages
	if user := sent[len(sent)-1]; user.Content != "[Attached files: /workspace/data.csv]" {
		t.Fatalf("user message %q", user.Content)
	}
}
