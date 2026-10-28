package runtime_test

import (
	"context"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/sandbox/sandboxtest"
)

// c4PDF is a PDF whose Info dictionary carries a C4 label.
const c4PDF = "%PDF-1.4\n%\xe2\xe3\n1 0 obj\n<< /MSIP_Label_x_Name(C4 - Secret) >>\nendobj\n"

func TestProcessViewFileRefusesADisallowedSource(t *testing.T) {
	// view_file and read_file refuse a C4 document (their page images and text would lose its label): tool errors, no page rendered, no render recorded (review round 1 P2 #2).
	// Arrange
	r := newV2Rig(t, true,
		llmtest.Turn{Chunks: streamedCall("v1", "view_file", `{"path":"secret.pdf"}`)},
		llmtest.Turn{Chunks: streamedCall("r1", "read_file", `{"path":"secret.pdf"}`)},
		llmtest.Turn{Chunks: llmtest.Text("I cannot open it.")},
	)
	r.agent.Harness.Vision = func(string) bool { return true }
	r.backend.OnCreate = func(sb *sandboxtest.Sandbox) {
		sb.SetExec(sandboxtest.FindExec(sb, nil))
		sb.PutDoc("/workspace/secret.pdf", sandboxtest.Doc{Parts: []string{"Top secret page"}})
		sb.Put("/workspace/secret.pdf", c4PDF)
	}
	_, run := r.queued("look at the secret pdf")

	// Act
	r.process(run)

	// Assert
	results := find(r.events(run.ID), "TOOL_CALL_RESULT")
	if len(results) != 2 {
		t.Fatalf("results %v", types(r.events(run.ID)))
	}
	for _, res := range results {
		if content, _ := res["content"].(string); !strings.Contains(content, "C4 or C3-Amundi-Only") || strings.Contains(content, "Top secret page") {
			t.Fatalf("result %q", content)
		}
	}
	if renders := r.backend.Created()[0].Renders(); len(renders) != 0 {
		t.Fatalf("rendered %v", renders)
	}
}

func TestProcessViewFileRecordsTheRenderWithItsSourceLabel(t *testing.T) {
	// view_file of an allowed document returns its renderId (the folder of its page images) and records the render (source path, label, pages) for the renders endpoint.
	// Arrange
	r := newV2Rig(t, true,
		llmtest.Turn{Chunks: streamedCall("v1", "view_file", `{"path":"outputs/deck.pptx","pages":"2"}`)},
		llmtest.Turn{Chunks: llmtest.Text("The slide looks right.")},
	)
	r.agent.Harness.Vision = func(string) bool { return true }
	r.backend.OnCreate = func(sb *sandboxtest.Sandbox) {
		sb.SetExec(sandboxtest.FindExec(sb, nil))
		sb.PutDoc("/workspace/outputs/deck.pptx", sandboxtest.Doc{Parts: []string{"Title", "Body"}})
	}
	th, run := r.queued("check the deck")

	// Act
	r.process(run)

	// Assert
	pages := r.backend.Created()[0].Renders()
	if len(pages) != 1 {
		t.Fatalf("rendered %v", pages)
	}
	renderID := path.Base(path.Dir(pages[0]))
	got, err := r.st.GetRender(context.Background(), th.ID, renderID)
	if err != nil || got.SourcePath != "/workspace/outputs/deck.pptx" || !got.Allowed || !slices.Equal(got.Pages, []int{2}) {
		t.Fatalf("render %+v %v", got, err)
	}
	result := toolResult(t, r.events(run.ID), 0)
	if result["renderId"] != renderID || pages[0] != "/workspace/.work/renders/"+renderID+"/p2.png" {
		t.Fatalf("result %v, page %s", result, pages[0])
	}
}
