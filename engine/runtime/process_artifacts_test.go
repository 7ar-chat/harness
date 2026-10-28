package runtime_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/sandbox"
	"github.com/GhaziBenDahmane/harness/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/store"
)

func indexOf(evs []event, typ string) int {
	for i, e := range evs {
		if types([]event{e})[0] == typ {
			return i
		}
	}
	return -1
}

func TestWriteFileEmitsAnArtifactEventRightAfterItsResult(t *testing.T) {
	// A file written by the agent becomes an artifact; work.artifact follows the tool's TOOL_CALL_RESULT.
	// Arrange
	r := newV2Rig(t, false,
		llmtest.Turn{Chunks: streamedCall("call_1", "write_file", `{"path":"report.md","content":"# Q3"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
	)
	th, run := r.queued("write the report")

	// Act
	got := r.process(run)

	// Assert
	evs := r.events(run.ID)
	res := indexOf(evs, "TOOL_CALL_RESULT")
	if got.Status != store.StatusCompleted || res < 0 || types(evs)[res+1] != "CUSTOM:work.artifact" {
		t.Fatalf("status %s types %v", got.Status, types(evs))
	}
	value := evs[res+1]["value"].(map[string]any)
	list, _ := r.st.ListArtifacts(context.Background(), th.ID)
	if value["path"] != "/workspace/report.md" || value["change"] != "created" || value["version"] != float64(1) ||
		value["kind"] != "output" || value["mimeType"] != "text/markdown" || value["artifactId"] != list[0].ID {
		t.Fatalf("event %v", value)
	}
	if len(list) != 1 || list[0].RunID != run.ID || list[0].SizeBytes != 4 {
		t.Fatalf("artifacts %+v", list)
	}
	if n := len(find(evs, "CUSTOM:work.artifact")); n != 1 {
		t.Fatalf("%d artifact events, want 1 (the end-of-run snapshot finds nothing new)", n)
	}
}

func TestEndOfRunSnapshotReportsChangesBeforeRunFinished(t *testing.T) {
	// Files changed outside file tools (e.g. by a background process) are reported right before RUN_FINISHED.
	// Arrange
	var sb *sandboxtest.Sandbox
	r := newV2Rig(t, false,
		llmtest.Turn{Chunks: streamedCall("call_1", "list_files", `{}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done"), OnCall: func() { sb.Put("/workspace/late.csv", "a,b") }},
	)
	r.backend.OnCreate = func(s *sandboxtest.Sandbox) { sb = s; s.SetExec(sandboxtest.FindExec(s, nil)) }
	_, run := r.queued("list")

	// Act
	r.process(run)

	// Assert
	evs := r.events(run.ID)
	ts := types(evs)
	if len(ts) < 2 || ts[len(ts)-2] != "CUSTOM:work.artifact" || ts[len(ts)-1] != "RUN_FINISHED" {
		t.Fatalf("types %v", ts)
	}
	if v := evs[len(evs)-2]["value"].(map[string]any); v["path"] != "/workspace/late.csv" || v["change"] != "created" {
		t.Fatalf("event %v", v)
	}
}

func TestCommandChangesAndDeletionsAreVersioned(t *testing.T) {
	// execute_command edits create new versions and removals are reported as deleted.
	// Arrange
	r := newV2Rig(t, false,
		llmtest.Turn{Chunks: streamedCall("call_1", "write_file", `{"path":"a.txt","content":"one"}`)},
		llmtest.Turn{Chunks: streamedCall("call_2", "execute_command", `{"command":"edit a.txt; rm b.txt"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
	)
	r.backend.OnCreate = func(s *sandboxtest.Sandbox) {
		s.Put("/workspace/b.txt", "bee")
		s.SetExec(sandboxtest.FindExec(s, func(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
			if strings.HasPrefix(req.Command, "edit") {
				s.Put("/workspace/a.txt", "one, two")
				s.Remove("/workspace/b.txt")
			}
			return sandbox.ExecResult{}, nil
		}))
	}
	th, run := r.queued("go")

	// Act
	r.process(run)

	// Assert
	var changes []string
	for _, e := range find(r.events(run.ID), "CUSTOM:work.artifact") {
		v := e["value"].(map[string]any)
		changes = append(changes, v["name"].(string)+":"+v["change"].(string))
	}
	sort.Strings(changes) // one snapshot reports its files in mtime order
	if !equal(changes, []string{"a.txt:created", "a.txt:updated", "b.txt:created", "b.txt:deleted"}) {
		t.Fatalf("changes %v", changes)
	}
	list, _ := r.st.ListArtifacts(context.Background(), th.ID)
	for _, a := range list {
		if a.Name == "a.txt" && (a.Version != 2 || a.SizeBytes != 8) || a.Name == "b.txt" && !a.Deleted {
			t.Fatalf("artifact %+v", a)
		}
	}
}

func TestNewSandboxRestoresTheWorkspaceAndTellsTheModel(t *testing.T) {
	// When a thread's sandbox is gone, the next sandbox gets the saved artifacts back and the tool result says so.
	// Arrange
	r := newV2Rig(t, false,
		llmtest.Turn{Chunks: streamedCall("call_1", "write_file", `{"path":"keep.txt","content":"saved"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
		llmtest.Turn{Chunks: streamedCall("call_2", "read_file", `{"path":"keep.txt"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Read it")},
	)
	th, first := r.queued("save")
	r.process(first)
	r.backend.Forget(r.sandbox().ID()) // expired at the provider
	r.wire()                           // a new runtime process: no cached sandbox handle
	second := r.queuedOn(th, "read it back")

	// Act
	r.process(second)

	// Assert
	res := find(r.events(second.ID), "TOOL_CALL_RESULT")[0]["content"].(string)
	if !strings.Contains(res, "1 saved file(s) were restored into /workspace") || !strings.Contains(res, "saved") {
		t.Fatalf("result %q", res)
	}
	if got, _ := r.sandbox().File("/workspace/keep.txt"); got != "saved" || len(r.backend.Created()) != 2 {
		t.Fatalf("restored %q sandboxes %d", got, len(r.backend.Created()))
	}
}

func TestArtifactChangesAreLinkedToTheirToolCallAndPersistedForReload(t *testing.T) {
	// work.artifact carries the producing toolCallId (none at end of run); the changes are stored on the tool
	// message of that call and, for end-of-run changes, on the run's last assistant message.
	// Arrange
	var sb *sandboxtest.Sandbox
	r := newV2Rig(t, false,
		llmtest.Turn{Chunks: streamedCall("call_1", "write_file", `{"path":"a.txt","content":"one"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done"), OnCall: func() { sb.Put("/workspace/late.csv", "x") }},
	)
	r.backend.OnCreate = func(s *sandboxtest.Sandbox) { sb = s; s.SetExec(sandboxtest.FindExec(s, nil)) }
	th, run := r.queued("go")

	// Act
	r.process(run)

	// Assert
	evs := find(r.events(run.ID), "CUSTOM:work.artifact")
	first, last := evs[0]["value"].(map[string]any), evs[1]["value"].(map[string]any)
	if first["toolCallId"] != "call_1" || first["path"] != "/workspace/a.txt" {
		t.Fatalf("tool event %v", first)
	}
	if _, ok := last["toolCallId"]; ok || last["path"] != "/workspace/late.csv" {
		t.Fatalf("end-of-run event %v", last)
	}
	var toolMeta, assistantMeta []any
	for _, m := range r.messages(th.ID) {
		switch {
		case m.Role == store.RoleTool && m.ToolCallID == "call_1":
			toolMeta, _ = m.Metadata["artifacts"].([]any)
		case m.Role == store.RoleAssistant && m.Content == "Done":
			assistantMeta, _ = m.Metadata["artifacts"].([]any)
		}
	}
	if len(toolMeta) != 1 || toolMeta[0].(map[string]any)["path"] != "/workspace/a.txt" || toolMeta[0].(map[string]any)["change"] != "created" ||
		toolMeta[0].(map[string]any)["artifactId"] == "" || toolMeta[0].(map[string]any)["name"] != "a.txt" {
		t.Fatalf("tool message metadata %v", toolMeta)
	}
	if len(assistantMeta) != 1 || assistantMeta[0].(map[string]any)["path"] != "/workspace/late.csv" {
		t.Fatalf("assistant message metadata %v", assistantMeta)
	}
}

func TestEndOfRunChangesOfARunWithoutAnswerAreKeptOnAnEmptyAssistantMessage(t *testing.T) {
	// When a run produced no assistant message (the model failed), its end-of-run artifact changes are stored on an empty assistant message of that run.
	// Arrange
	r := newV2Rig(t, false,
		llmtest.Turn{Chunks: streamedCall("call_1", "write_file", `{"path":"a.txt","content":"a"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
		llmtest.Turn{Err: errors.New("bad request: invalid model input")},
	)
	th, first := r.queued("write")
	r.process(first)
	r.sandbox().Put("/workspace/late.csv", "x,y") // e.g. written by a background process
	second := r.queuedOn(th, "go on")

	// Act
	got := r.process(second)

	// Assert
	var carrier *store.Message
	for _, m := range r.messages(th.ID) {
		if m.RunID == second.ID && m.Role == store.RoleAssistant {
			carrier = m
		}
	}
	if got.Status != store.StatusFailed || carrier == nil || carrier.Content != "" || len(store.ArtifactPaths(carrier.Metadata)) != 1 ||
		store.ArtifactPaths(carrier.Metadata)[0] != "/workspace/late.csv" {
		t.Fatalf("status %s carrier %+v", got.Status, carrier)
	}
}

func TestCompactRunsDoNotSnapshotTheWorkspace(t *testing.T) {
	// A compact run only compacts: the end-of-run snapshot is skipped (no work.artifact events, no versions).
	// Arrange
	r := newV2Rig(t, false,
		llmtest.Turn{Chunks: streamedCall("call_1", "write_file", `{"path":"a.txt","content":"a"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
		llmtest.Turn{Chunks: llmtest.Text("## Summary")},
	)
	th, first := r.queued("write")
	r.process(first)
	r.sandbox().Put("/workspace/late.csv", "x,y")
	compact := r.create(th, store.NewRun{Kind: store.KindCompact})

	// Act
	r.process(compact)

	// Assert
	if n := len(find(r.events(compact.ID), "CUSTOM:work.artifact")); n != 0 {
		t.Fatalf("%d artifact events in the compact run", n)
	}
	if list, _ := r.st.ListArtifacts(context.Background(), th.ID); len(list) != 1 {
		t.Fatalf("artifacts %d", len(list))
	}
}
