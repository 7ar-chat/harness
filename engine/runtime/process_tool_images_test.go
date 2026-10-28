package runtime_test

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/sandbox/sandboxtest"
)

// toolImageParts counts the image parts of a call's messages and returns the user message holding them.
func toolImageParts(msgs []*schema.Message) (int, *schema.Message) {
	n := 0
	var holder *schema.Message
	for _, m := range msgs {
		for _, p := range m.UserInputMultiContent {
			if p.Type == schema.ChatMessagePartTypeImageURL {
				n++
				holder = m
			}
		}
	}
	return n, holder
}

func TestImagesShownByToolsReachTheNextCallOnly(t *testing.T) {
	// A page rendered by view_file is sent as an image part with the next model call of a vision model, then replaced by a text stub naming it (§3).
	// Arrange
	r := newV2Rig(t, true,
		llmtest.Turn{Chunks: streamedCall("v1", "view_file", `{"path":"outputs/deck.pptx","pages":"1"}`)},
		llmtest.Turn{Chunks: streamedCall("l1", "list_files", `{}`)},
		llmtest.Turn{Chunks: llmtest.Text("The slide looks right.")},
	)
	r.agent.Harness.Vision = func(string) bool { return true }
	r.backend.OnCreate = func(sb *sandboxtest.Sandbox) {
		sb.SetExec(sandboxtest.FindExec(sb, nil))
		sb.PutDoc("/workspace/outputs/deck.pptx", sandboxtest.Doc{Parts: []string{"Title slide"}})
	}
	_, run := r.queued("check the deck")

	// Act
	r.process(run)

	// Assert
	calls := r.model.Calls()
	if len(calls) != 3 {
		t.Fatalf("calls %d", len(calls))
	}
	if n, _ := toolImageParts(calls[0].Messages); n != 0 {
		t.Fatalf("image before the tool ran")
	}
	n, holder := toolImageParts(calls[1].Messages)
	if n != 1 || holder.Role != schema.User || !strings.Contains(holder.UserInputMultiContent[0].Text, "/workspace/.work/renders/deck-") {
		t.Fatalf("second call images %d holder %+v", n, holder)
	}
	if n, _ := toolImageParts(calls[2].Messages); n != 0 {
		t.Fatalf("image re-sent in a later step")
	}
	stub := false
	for _, m := range calls[2].Messages { // the image part became a text stub (images.go)
		for _, p := range m.UserInputMultiContent {
			if m.Role == schema.User && strings.HasPrefix(p.Text, "[image /workspace/.work/renders/deck-") && strings.HasSuffix(p.Text, " shown earlier]") {
				stub = true
			}
		}
	}
	if !stub {
		t.Fatalf("no stub in the third call: %+v", calls[2].Messages)
	}
}

func TestNonVisionModelsGetNoImageParts(t *testing.T) {
	// A model outside LLM_VISION_MODELS never receives image parts: read_file of an image says it cannot view images.
	// Arrange
	r := newV2Rig(t, false,
		llmtest.Turn{Chunks: streamedCall("r1", "read_file", `{"path":"chart.png"}`)},
		llmtest.Turn{Chunks: llmtest.Text("ok")},
	)
	r.agent.Harness.Vision = func(string) bool { return false }
	r.backend.OnCreate = func(sb *sandboxtest.Sandbox) {
		sb.SetExec(sandboxtest.FindExec(sb, nil))
		sb.Put("/workspace/chart.png", "\x89PNG\r\n\x1a\n\x00")
	}
	_, run := r.queued("look")

	// Act
	r.process(run)

	// Assert
	calls := r.model.Calls()
	res := find(r.events(run.ID), "TOOL_CALL_RESULT")[0]["content"].(string)
	if n, _ := toolImageParts(calls[1].Messages); n != 0 || !strings.Contains(res, "cannot view images") {
		t.Fatalf("images %d result %s", n, res)
	}
}
