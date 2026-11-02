package runtime_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/tools"
	"github.com/7ar-chat/harness/features/artifacts"
)

// viewTool attaches n renders of size bytes each and reports whether the model sees them.
func viewTool(n, size int) *fakeTool {
	return &fakeTool{name: "view", run: func(ctx context.Context, _ string) (string, error) {
		shown := 0
		for i := 1; i <= n; i++ {
			if tools.AttachImage(ctx, tools.Image{Path: fmt.Sprintf("/workspace/.work/renders/p%d.png", i), MIMEType: "image/png", Data: bytes.Repeat([]byte{1}, size)}) {
				shown++
			}
		}
		b, _ := json.Marshal(map[string]any{"shown": shown, "vision": tools.CanViewImages(ctx)})
		return string(b), nil
	}}
}

// imageParts lists the image parts of a call and the text of its image stubs.
func imageParts(c llmtest.Call) (images int, stubs []string) {
	for _, m := range c.Messages {
		for _, p := range m.Parts {
			switch {
			case p.Type == ai.PartImage:
				images++
			case strings.HasPrefix(p.Text, "[image "):
				stubs = append(stubs, p.Text)
			}
		}
	}
	return images, stubs
}

func TestProcessSendsAToolImageOnceThenAStub(t *testing.T) {
	// An image a tool attaches reaches a vision model in the next step only; later steps get "[image <path> shown earlier]" (A-13).
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: []*ai.Message{llmtest.ToolCall(0, "v1", "view", `{}`)}},
		llmtest.Turn{Chunks: []*ai.Message{llmtest.ToolCall(0, "l1", "list_files", `{}`)}},
		llmtest.Turn{Chunks: llmtest.Text("The chart looks right.")},
	)
	r.withTools(viewTool(1, 100))
	r.agent.Harness.Vision = func(string) bool { return true }
	_, run := r.queued("check the render")

	// Act
	got := r.process(run)

	// Assert
	checkStrictOrder(t, r.events(run.ID))
	calls := r.model.Calls()
	if got.Status != store.StatusCompleted || len(calls) != 3 {
		t.Fatalf("status %s calls %d", got.Status, len(calls))
	}
	if n, _ := imageParts(calls[0]); n != 0 {
		t.Fatalf("first call has %d images", n)
	}
	if n, _ := imageParts(calls[1]); n != 1 {
		t.Fatalf("the step after the tool has %d images", n)
	}
	if n, stubs := imageParts(calls[2]); n != 0 || len(stubs) != 1 || stubs[0] != "[image /workspace/.work/renders/p1.png shown earlier]" {
		t.Fatalf("later step: %d images, stubs %q", n, stubs)
	}
}

func TestProcessNeverSendsImagesToModelsWithoutVision(t *testing.T) {
	// A model outside LLM_VISION_MODELS gets no image part; the tool learns it cannot show images.
	// Arrange
	r := newRig(t, llmtest.Turn{Chunks: []*ai.Message{llmtest.ToolCall(0, "v1", "view", `{}`)}}, llmtest.Turn{Chunks: llmtest.Text("ok")})
	r.withTools(viewTool(1, 100))
	_, run := r.queued("check the render")

	// Act
	r.process(run)

	// Assert
	checkStrictOrder(t, r.events(run.ID))
	calls := r.model.Calls()
	res := find(r.events(run.ID), "TOOL_CALL_RESULT")[0]["content"]
	if n, _ := imageParts(calls[1]); n != 0 || res != `{"shown":0,"vision":false}` {
		t.Fatalf("images %d result %v", n, res)
	}
}

func TestProcessCapsImagesPerRequest(t *testing.T) {
	// One request carries at most 6 images and 6 MiB of them, the newest first; the others are stubbed.
	// Arrange
	byCount := newRig(t, llmtest.Turn{Chunks: []*ai.Message{llmtest.ToolCall(0, "v1", "view", `{}`)}}, llmtest.Turn{Chunks: llmtest.Text("ok")})
	byCount.withTools(viewTool(8, 1000))
	bySize := newRig(t, llmtest.Turn{Chunks: []*ai.Message{llmtest.ToolCall(0, "v1", "view", `{}`)}}, llmtest.Turn{Chunks: llmtest.Text("ok")})
	bySize.withTools(viewTool(3, 5<<19)) // 2.5 MiB each
	for _, r := range []*rig{byCount, bySize} {
		r.agent.Harness.Vision = func(string) bool { return true }
	}
	_, countRun := byCount.queued("view")
	_, sizeRun := bySize.queued("view")

	// Act
	byCount.process(countRun)
	bySize.process(sizeRun)

	// Assert
	checkStrictOrder(t, byCount.events(countRun.ID))
	if n, stubs := imageParts(byCount.model.Calls()[1]); n != 6 || len(stubs) != 2 || !strings.Contains(stubs[0], "p1.png") {
		t.Fatalf("by count: %d images, stubs %q", n, stubs)
	}
	if n, stubs := imageParts(bySize.model.Calls()[1]); n != 2 || len(stubs) != 1 || !strings.Contains(stubs[0], "p1.png") {
		t.Fatalf("by size: %d images, stubs %q", n, stubs)
	}
}

func TestProcessSendsAnAttachedImageInTheFirstStepOnly(t *testing.T) {
	// An attached image goes to the run's first model call; later steps name it in a stub instead of re-sending it (A-13).
	// Arrange
	r := newScriptedRig(t, true,
		llmtest.Turn{Chunks: []*ai.Message{llmtest.ToolCall(0, "l1", "list_files", `{}`)}},
		llmtest.Turn{Chunks: llmtest.Text("A chart")},
	)
	r.agent.Harness.Vision = func(string) bool { return true }
	run := r.attachedRun(map[string][]byte{"chart.png": pngBytes})

	// Act
	r.process(run)

	// Assert
	checkStrictOrder(t, r.events(run.ID))
	calls := r.model.Calls()
	first, _ := imageParts(calls[0])
	second, stubs := imageParts(calls[1])
	if first != 1 || second != 0 || len(stubs) != 1 || stubs[0] != "[image /workspace/chart.png shown earlier]" {
		t.Fatalf("first %d second %d stubs %q", first, second, stubs)
	}
}

func TestAnAttachedImageStubNamesItsOwnFile(t *testing.T) {
	// When an attachment that is not sent as an image (an SVG) precedes an attached photo, the later stub names the photo, not the SVG.
	// Arrange
	r := newScriptedRig(t, true,
		llmtest.Turn{Chunks: []*ai.Message{llmtest.ToolCall(0, "l1", "list_files", `{}`)}},
		llmtest.Turn{Chunks: llmtest.Text("A logo and a photo")},
	)
	r.agent.Harness.Vision = func(string) bool { return true }
	ctx := context.Background()
	th, _ := r.st.CreateThread(ctx, "alice", "t")
	var items []any
	for _, f := range []struct {
		name string
		data []byte
	}{{"logo.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="4" height="4"><rect width="4" height="4"/></svg>`)}, {"photo.png", pngBytes}} {
		a, err := r.arts.Upload(ctx, artifacts.UploadInput{ThreadID: th.ID, Login: "alice", Filename: f.name, Data: f.data})
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, map[string]any{"artifactId": a.ID, "name": a.Name, "path": a.Path, "sizeBytes": a.SizeBytes, "mimeType": a.MimeType, "version": a.Version})
	}
	run := r.create(th, store.NewRun{Message: store.Message{Role: store.RoleUser, Content: "Look at these", Metadata: map[string]any{"attachments": items}}})

	// Act
	r.process(run)

	// Assert
	checkStrictOrder(t, r.events(run.ID))
	calls := r.model.Calls()
	first, _ := imageParts(calls[0])
	_, stubs := imageParts(calls[1])
	if first != 1 || len(stubs) != 1 || stubs[0] != "[image /workspace/photo.png shown earlier]" {
		t.Fatalf("first call %d images, later stubs %q", first, stubs)
	}
}

func TestImagesLeftOutByTheRequestCapAreNotCalledShown(t *testing.T) {
	// Images the per-request cap left out are stubbed "not shown" in later steps, the ones that were sent "shown earlier".
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: []*ai.Message{llmtest.ToolCall(0, "v1", "view", `{}`)}},
		llmtest.Turn{Chunks: []*ai.Message{llmtest.ToolCall(0, "l1", "list_files", `{}`)}},
		llmtest.Turn{Chunks: llmtest.Text("ok")},
	)
	r.withTools(viewTool(8, 1000))
	r.agent.Harness.Vision = func(string) bool { return true }
	_, run := r.queued("view")

	// Act
	r.process(run)

	// Assert
	checkStrictOrder(t, r.events(run.ID))
	_, stubs := imageParts(r.model.Calls()[2])
	var shown, notShown []string
	for _, s := range stubs {
		if strings.HasSuffix(s, "shown earlier]") {
			shown = append(shown, s)
		} else if strings.Contains(s, "not shown") {
			notShown = append(notShown, s)
		}
	}
	if len(shown) != 6 || len(notShown) != 2 || !strings.Contains(notShown[0], "p1.png") || !strings.Contains(notShown[1], "p2.png") {
		t.Fatalf("stubs %q", stubs)
	}
}
