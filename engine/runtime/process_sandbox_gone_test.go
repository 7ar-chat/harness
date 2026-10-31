package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/ai/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/core/store"
)

// killed removes the thread's sandbox at the provider while this runtime still
// holds its handle, checked healthy less than 30 s ago (no new process).
func (v *scriptedRig) killed() string {
	v.t.Helper()
	id := v.sandbox().ID()
	if err := v.backend.Kill(context.Background(), id); err != nil {
		v.t.Fatal(err)
	}
	return id
}

func TestASandboxKilledRightAfterARunIsReplacedOnTheNextRun(t *testing.T) {
	// A sandbox that died within the health-check window is detected on its first failing call: the next run gets a new sandbox with the workspace restored, and no tool fails.
	// Arrange
	r := newScriptedRig(t, false,
		llmtest.Turn{Chunks: streamedCall("call_1", "write_file", `{"path":"keep.txt","content":"saved"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
		llmtest.Turn{Chunks: streamedCall("call_2", "execute_command", `{"command":"ls"}`)},
		llmtest.Turn{Chunks: streamedCall("call_3", "read_file", `{"path":"keep.txt"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Read it")},
	)
	th, first := r.queued("save")
	r.process(first)
	old := r.killed()
	second := r.queuedOn(th, "read it back")

	// Act
	got := r.process(second)

	// Assert
	results := find(r.events(second.ID), "TOOL_CALL_RESULT")
	if got.Status != store.StatusCompleted || len(results) != 2 {
		t.Fatalf("status %s results %d", got.Status, len(results))
	}
	for _, res := range results {
		if res["metadata"] != nil || strings.Contains(res["content"].(string), "SANDBOX_NOT_FOUND") {
			t.Fatalf("tool failed: %v", res)
		}
	}
	if exec := results[0]["content"].(string); !strings.Contains(exec, "1 saved file(s) were restored into /workspace") {
		t.Fatalf("no restore notice in %q", exec)
	}
	if read := results[1]["content"].(string); !strings.Contains(read, "saved") {
		t.Fatalf("read %q", read)
	}
	current, _ := r.st.ThreadSandbox(context.Background(), th.ID)
	if len(r.backend.Created()) != 2 || current == old || current != r.sandbox().ID() {
		t.Fatalf("sandboxes %d current %q old %q", len(r.backend.Created()), current, old)
	}
}

func TestEndOfRunSnapshotOfAKilledSandboxForgetsIt(t *testing.T) {
	// When only the end-of-run snapshot meets the dead sandbox, the thread forgets it (the next tool call creates a new one) without creating a sandbox for nothing.
	// Arrange
	r := newScriptedRig(t, false,
		llmtest.Turn{Chunks: streamedCall("call_1", "write_file", `{"path":"keep.txt","content":"saved"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
		llmtest.Turn{Chunks: llmtest.Text("Nothing to do")},
	)
	th, first := r.queued("save")
	r.process(first)
	r.killed()
	second := r.queuedOn(th, "hello")

	// Act
	got := r.process(second)

	// Assert
	current, _ := r.st.ThreadSandbox(context.Background(), th.ID)
	if got.Status != store.StatusCompleted || current != "" || len(r.backend.Created()) != 1 {
		t.Fatalf("status %s current %q sandboxes %d", got.Status, current, len(r.backend.Created()))
	}
	list, _ := r.st.ListArtifacts(context.Background(), th.ID)
	if len(list) != 1 || list[0].Deleted {
		t.Fatalf("artifacts %+v", list)
	}
}

func TestLoadSkillOnAKilledSandboxIsInstalledInTheNextOne(t *testing.T) {
	// load_skill after the thread's sandbox died starts no sandbox; the next command's new sandbox gets the skill installed and the model is told about the restore.
	// Arrange
	r := newScriptedRig(t, false,
		llmtest.Turn{Chunks: streamedCall("call_1", "write_file", `{"path":"keep.txt","content":"saved"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
		llmtest.Turn{Chunks: streamedCall("call_2", "load_skill", `{"name":"fixture-doc"}`)},
		llmtest.Turn{Chunks: streamedCall("call_3", "execute_command", `{"command":"ls"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Loaded")},
	)
	th, first := r.queued("save")
	r.process(first)
	r.killed()
	second := r.queuedOn(th, "make a fixture document")

	// Act
	r.process(second)

	// Assert
	res := find(r.events(second.ID), "TOOL_CALL_RESULT")
	if len(res) != 2 || res[0]["metadata"] != nil || !strings.Contains(res[0]["content"].(string), "# Fixture documents") ||
		!strings.Contains(res[0]["content"].(string), "from your first command") || !strings.Contains(res[1]["content"].(string), "restored into /workspace") {
		t.Fatalf("results %v", res)
	}
	if md, ok := r.sandbox().File("/skills/fixture-doc/SKILL.md"); !ok || md == "" || len(r.backend.Created()) != 2 {
		t.Fatalf("skill not installed in the new sandbox (sandboxes %d)", len(r.backend.Created()))
	}
}
