package runtime_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	goopenai "github.com/meguminnnnnnnnn/go-openai"

	"github.com/GhaziBenDahmane/harness/core/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/agent"
	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/features/verify"
)

func TestProcessRunsTheLoopAndToolStreamsTogether(t *testing.T) {
	// One run through both v3 streams (C1 loop & context, C2 tools & verification), wired like cmd/work: it reads a document,
	// writes a deliverable under /workspace/outputs/ after a broken stream was retried, gets the post-write validation in the
	// result, has the old document result masked, is compacted (the context message lists the deliverable with the verifier's
	// status) and is refused once by the verification StopGuard; the AG-UI order stays valid and the system prompt identical.
	// Arrange
	brief := "Quarterly brief. " + strings.Repeat("Revenue grew in every region; the report needs one section per region. ", 230)
	// A long deliverable: the compaction keeps it in the tail and folds the document read before it.
	writeReport := `{"path":"outputs/report.docx","content":"` + strings.Repeat("BROKEN draft line. ", 1700) + `"}`
	r := verifyRig(t, false,
		llmtest.Turn{Chunks: streamedCall("r1", "read_file", `{"path":"brief.docx"}`)},
		llmtest.Turn{Chunks: []*schema.Message{{Role: schema.Assistant, Content: "Writing"}, llmtest.ToolCall(0, "w0", "write_file", `{"path":"outputs/rep`)},
			FailAfter: &goopenai.APIError{HTTPStatusCode: 502, Message: "bad gateway"}},
		llmtest.Turn{Chunks: streamedCall("w1", "write_file", writeReport)},
		llmtest.Turn{Chunks: []*schema.Message{llmtest.WithUsage(llmtest.ToolCall(0, "l1", "list_files", `{"path":"outputs"}`), 19500, 10)}},
		llmtest.Turn{Chunks: llmtest.Text("## Goal\nWrite the quarterly report.")}, // the compaction summary
		llmtest.Turn{Chunks: llmtest.Text("Done.")},
		llmtest.Turn{Chunks: llmtest.Text("Done, but the heading levels could not be fixed.")},
	)
	r.window = 20000
	r.agent.MaxSteps = 8
	r.agent.Harness.KeepRecentToolResults = 1
	recordSleeps(r.rig)
	r.backend.OnCreate = func(sb *sandboxtest.Sandbox) {
		sb.SetExec(sandboxtest.FindExec(sb, sandboxtest.ShellExec(sb)))
		sb.PutDoc("/workspace/brief.docx", sandboxtest.Doc{Parts: []string{brief}})
	}
	// Like cmd/work: one verifier is the post-write checker and the rehydration's deliverable source.
	v := verify.New(verify.Options{Sandbox: r.mgr, Messages: r.st, Checkers: map[string]verify.Checker{
		".docx": {Name: "inspect_docx", Command: "bash " + fixtureChecker(t) + " {file}"}}})
	r.agent.Harness.Checker, r.agent.Harness.Deliverables = v, v
	th, run := r.queued("write the quarterly report from brief.docx")

	// Act
	got := r.process(run)

	// Assert
	evs := r.events(run.ID)
	checkStrictOrder(t, evs)
	tys := types(evs)
	calls := r.model.Calls()
	if got.Status != store.StatusCompleted || len(calls) != 7 || !isSummaryCall(calls[4]) {
		t.Fatalf("status %s calls %d types %v", got.Status, len(calls), tys)
	}
	// The document was read through markitdown.
	results := find(evs, "TOOL_CALL_RESULT")
	if results[0]["toolCallId"] != "r1" || !strings.Contains(results[0]["content"].(string), "Quarterly brief.") {
		t.Fatalf("read_file result %v", results[0])
	}
	// The broken stream was retried: work.retry, a retried result for w0, nothing of it persisted.
	retries := find(evs, "CUSTOM:work.retry")
	if len(retries) != 1 || results[1]["toolCallId"] != "w0" || results[1]["content"] != agent.StreamRetriedResult {
		t.Fatalf("retries %v second result %v", retries, results[1])
	}
	for _, m := range r.messages(th.ID) {
		if m.ToolCallID == "w0" || m.Content == "Writing" {
			t.Fatalf("the failed attempt was persisted: %+v", m)
		}
	}
	// Post-write validation in the result, work.verification after it and before its step ends.
	res := results[2]["content"].(string)
	if results[2]["toolCallId"] != "w1" || !strings.Contains(res, `"verification":[{"checker":"inspect_docx"`) || !strings.Contains(res, `"summary":"inspect_docx: NOT OK`) {
		t.Fatalf("write result %q", res)
	}
	vi := index(tys, "CUSTOM:work.verification")
	if vi < 0 || vi < indexOfResult(evs, "w1") || vi > nextAfter(tys, vi, "STEP_FINISHED") {
		t.Fatalf("work.verification misplaced: %v", tys)
	}
	// Masking: in step 4 the document result is a stub (only the newest result is kept), the stored result is whole.
	var masked string
	for _, m := range calls[3].Messages {
		if m.Role == schema.Tool && m.ToolCallID == "r1" {
			masked = m.Content
		}
	}
	if !strings.HasPrefix(masked, "[read_file result, ") || !strings.HasSuffix(masked, "— hidden to save context]") {
		t.Fatalf("document result in step 4: %.200q", masked)
	}
	// Compaction and rehydration: the context message after the system prompt lists the deliverable with the verifier's status.
	ce := compactionEvents(evs)
	if len(ce) != 2 || ce[1]["phase"] != "completed" {
		t.Fatalf("compaction %v", ce)
	}
	after := calls[5].Messages
	ctxMsg := after[1]
	if ctxMsg.Role != schema.User || !strings.Contains(ctxMsg.Content, "<"+agent.RehydrationTag+">") ||
		!strings.Contains(ctxMsg.Content, "/workspace/outputs/report.docx: inspect_docx failed") {
		t.Fatalf("context message after the compaction: %+v", ctxMsg)
	}
	// The StopGuard refused the first stop once, naming the failing deliverable; the answer lists it as failed.
	reminder := calls[6].Messages[len(calls[6].Messages)-1].Content
	if !strings.Contains(reminder, "/workspace/outputs/report.docx: its checker failed") {
		t.Fatalf("stop reminder %q", reminder)
	}
	verdict, _ := json.Marshal(lastAnswer(t, r, th.ID).Metadata["verification"])
	if !strings.Contains(string(verdict), `"failed":[{"checker":"inspect_docx","path":"/workspace/outputs/report.docx"`) {
		t.Fatalf("answer verification %s", verdict)
	}
	// The system prompt is byte-identical on every step (cache-stable layout); the compaction only appends its summary
	// at the end of the dynamic context, after the static sections.
	before, compacted := calls[0].Messages[0].Content, calls[5].Messages[0].Content
	for i, c := range calls {
		if want := map[bool]string{true: before, false: compacted}[i < 4]; i != 4 && c.Messages[0].Content != want {
			t.Fatalf("call %d changed the system prompt", i)
		}
	}
	static := strings.LastIndex(before, "</context>")
	if static < 0 || compacted == before || !strings.HasPrefix(compacted, before[:static]) || !strings.Contains(compacted[static:], "Write the quarterly report.") {
		t.Fatalf("the compaction rewrote the static prompt:\n%s", compacted)
	}
}

// indexOfResult is the index of the TOOL_CALL_RESULT of a call (-1: none).
func indexOfResult(evs []event, toolCallID string) int {
	for i, e := range evs {
		if e["type"] == "TOOL_CALL_RESULT" && e["toolCallId"] == toolCallID {
			return i
		}
	}
	return -1
}

// nextAfter is the index of the first typ after from (len: none).
func nextAfter(tys []string, from int, typ string) int {
	for i := from + 1; i < len(tys); i++ {
		if tys[i] == typ {
			return i
		}
	}
	return len(tys)
}
