//go:build unix

package persistence_test

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
)

// startFileProcess is a process over the durable local store of dir: what a CLI, a desktop app or a single-user web
// server is. Stopping it closes the store, which releases the directory lock.
func startFileProcess(t *testing.T, dir string, model *llmtest.Model) *process {
	t.Helper()
	t.Setenv("LOCAL_STATE_DIR", dir)
	st, err := harness.OpenLocalStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return startProcess(t, processOpts{store: st, model: model, closeStore: func() {
		if err := st.Close(); err != nil {
			t.Errorf("closing the store: %v", err)
		}
	}})
}

// history reduces the messages a model call received to (role, content, tool call ids) without the system prompt.
type turnMsg struct {
	Role       ai.Role
	Content    string
	CallIDs    []string
	ToolCallID string
}

func conversation(c llmtest.Call) []turnMsg {
	var out []turnMsg
	for _, m := range c.Messages {
		if m.Role == ai.RoleSystem {
			continue
		}
		tm := turnMsg{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			tm.CallIDs = append(tm.CallIDs, tc.ID)
		}
		out = append(out, tm)
	}
	return out
}

func equalTurns(a, b []turnMsg) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Role != b[i].Role || a[i].Content != b[i].Content || a[i].ToolCallID != b[i].ToolCallID ||
			len(a[i].CallIDs) != len(b[i].CallIDs) {
			return false
		}
		for j := range a[i].CallIDs {
			if a[i].CallIDs[j] != b[i].CallIDs[j] {
				return false
			}
		}
	}
	return true
}

func TestAConversationSurvivesARestartOverTheSameStateDirectory(t *testing.T) {
	// Arrange: process 1 runs two turns, the first with a write_file tool call, and captures what the API shows.
	dir := t.TempDir()
	p1 := startFileProcess(t, dir, scriptedModel())
	thread := p1.newThread("alice")
	run1 := p1.send("alice", thread, "Create the notes [write v1]")
	events1 := p1.finishedRun("alice", run1)
	run2 := p1.send("alice", thread, "What did you do?")
	events2 := p1.finishedRun("alice", run2)
	before := p1.thread("alice", thread)
	listBefore := p1.threads("alice")
	p1.stop()

	// Act: process 2 starts over the same directory with a fresh model.
	model2 := scriptedModel()
	p2 := startFileProcess(t, dir, model2)
	after := p2.thread("alice", thread)
	listAfter := p2.threads("alice")
	replay1 := p2.stream("alice", run1)
	replay2 := p2.stream("alice", run2)

	// Assert: the thread list, the title and the message list are those of before the restart.
	if len(listAfter) != 1 || listAfter[0].ID != thread || listAfter[0].Title != "A title" || listAfter[0].Title != listBefore[0].Title {
		t.Fatalf("threads after the restart = %+v (before %+v)", listAfter, listBefore)
	}
	if want := []string{"user", "assistant", "tool", "assistant", "user", "assistant"}; len(after.Messages) != len(want) {
		t.Fatalf("%d messages after the restart, want %d: %v", len(after.Messages), len(want), after.Messages)
	}
	if !equalJSON(before.Messages, after.Messages) {
		t.Fatalf("messages changed over the restart:\nbefore %v\nafter  %v", before.Messages, after.Messages)
	}
	if calls, _ := after.Messages[1]["toolCalls"].([]any); len(calls) != 1 || after.Messages[2]["role"] != "tool" || after.Messages[2]["toolCallId"] != "call-v1" {
		t.Fatalf("the tool call and its result were not kept: %v / %v", after.Messages[1], after.Messages[2])
	}
	if after.ActiveRun != nil {
		t.Fatalf("activeRun after the restart = %+v", after.ActiveRun)
	}

	// Assert: the finished runs replay byte for byte, whole and from a Last-Event-ID in the middle.
	if !equalSSE(replay1, events1) || !equalSSE(replay2, events2) {
		t.Fatalf("replayed events differ:\nbefore %v\nafter  %v", types(events1), types(replay1))
	}
	cut := events1[len(events1)/2].ID
	n, _ := strconv.Atoi(cut)
	if resumed := p2.stream("alice", run1, "Last-Event-ID", cut); !equalSSE(resumed, events1[n:]) {
		t.Fatalf("resume after %s: got %v, want the %d events after it", cut, types(resumed), len(events1)-n)
	}
	if r := p2.run("alice", run1); r.Status != "completed" || r.ThreadID != thread {
		t.Fatalf("run after the restart = %+v", r)
	}

	// Act: a third turn in the resumed thread.
	run3 := p2.send("alice", thread, "Thanks, and now?")
	p2.finishedRun("alice", run3)

	// Assert: the model received the whole previous history, in order, then the new message.
	calls := mainCalls(model2)
	if len(calls) != 1 {
		t.Fatalf("%d agent calls in process 2, want 1", len(calls))
	}
	want := []turnMsg{
		{Role: ai.RoleUser, Content: "Create the notes [write v1]"},
		{Role: ai.RoleAssistant, CallIDs: []string{"call-v1"}},
		{Role: ai.RoleTool, ToolCallID: "call-v1", Content: toolResult(t, before.Messages[2])},
		{Role: ai.RoleAssistant, Content: "Tool finished"},
		{Role: ai.RoleUser, Content: "What did you do?"},
		{Role: ai.RoleAssistant, Content: "Reply to: What did you do?"},
		{Role: ai.RoleUser, Content: "Thanks, and now?"},
	}
	if got := conversation(calls[0]); !equalTurns(got, want) {
		t.Fatalf("history sent to the model:\ngot  %+v\nwant %+v", got, want)
	}
	if th := p2.thread("alice", thread); len(th.Messages) != 8 || th.Messages[7]["content"] != "Reply to: Thanks, and now?" {
		t.Fatalf("thread after the third turn: %v", th.Messages)
	}
}

func toolResult(t *testing.T, m map[string]any) string {
	t.Helper()
	s, ok := m["content"].(string)
	if !ok || m["role"] != "tool" {
		t.Fatalf("not a tool message: %v", m)
	}
	return s
}

func equalSSE(a, b []sseEvent) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestThreadOwnershipIsStillEnforcedAfterARestart(t *testing.T) {
	// Arrange: alice's thread and run exist before the restart.
	dir := t.TempDir()
	p1 := startFileProcess(t, dir, scriptedModel())
	thread := p1.newThread("alice")
	run := p1.send("alice", thread, "Private question")
	p1.finishedRun("alice", run)
	p1.stop()
	p2 := startFileProcess(t, dir, scriptedModel())

	// Act: bob and an anonymous caller try to reach alice's data.
	bobThreads := p2.threads("bob")
	requests := []struct{ method, path, body string }{
		{"GET", "/api/threads/" + thread, ""},
		{"DELETE", "/api/threads/" + thread, ""},
		{"POST", "/api/threads/" + thread + "/runs", `{"message":"hijack"}`},
		{"GET", "/api/runs/" + run, ""},
		{"GET", "/api/runs/" + run + "/events", ""},
		{"POST", "/api/runs/" + run + "/cancel", ""},
	}
	var bobCodes []int
	for _, r := range requests {
		code, _ := p2.do(r.method, r.path, "bob", r.body)
		bobCodes = append(bobCodes, code)
	}
	anonymous, _ := p2.do("GET", "/api/threads/"+thread, "", "")

	// Assert
	if len(bobThreads) != 0 {
		t.Fatalf("bob sees %d threads", len(bobThreads))
	}
	for i, code := range bobCodes {
		if code != 404 {
			t.Errorf("%s %s as bob: status %d, want 404", requests[i].method, requests[i].path, code)
		}
	}
	if anonymous != 401 {
		t.Errorf("anonymous: status %d, want 401", anonymous)
	}
	if th := p2.thread("alice", thread); len(th.Messages) != 2 {
		t.Errorf("alice's thread was altered: %v", th.Messages)
	}
}

func TestARunInFlightWhenTheProcessIsKilledIsInterruptedAndTheThreadStaysUsable(t *testing.T) {
	// Arrange: the model call of the first run is held, so the run is mid-flight in process 1.
	dir := t.TempDir()
	gate := newGatedModel(t)
	p1 := startFileProcess(t, dir, gate.Model)
	thread := p1.newThread("alice")
	run := p1.send("alice", thread, "Think about it")
	gate.waitCalled(t)

	// Act: what a kill -9 leaves is the journal as it is right now. The snapshot omits the lock file, which the
	// kernel drops with the dead process; process 1 is only released afterwards, once the snapshot is taken.
	crashed := snapshotJournal(t, dir)
	gate.open()
	p1.stop()
	p2 := startFileProcess(t, crashed, scriptedModel())

	// Assert: the run is interrupted and its stream ends with a single RUN_ERROR that says so.
	r := p2.run("alice", run)
	if r.Status != "interrupted" || r.ErrorCode != "interrupted" {
		t.Fatalf("run after the crash = %+v", r)
	}
	evs := p2.stream("alice", run)
	last := evs[len(evs)-1]
	if last.Type() != "RUN_ERROR" || terminals(evs) != 1 || !strings.Contains(last.Data, `"code":"interrupted"`) {
		t.Fatalf("events after the crash = %v, last %s", types(evs), last.Data)
	}
	if evs[0].Type() != "RUN_STARTED" {
		t.Fatalf("the stream does not start with RUN_STARTED: %v", types(evs))
	}

	// Assert: the thread is not stuck and keeps the message it had.
	th := p2.thread("alice", thread)
	if th.ActiveRun != nil {
		t.Fatalf("thread still has an active run: %+v", th.ActiveRun)
	}
	if len(th.Messages) != 1 || th.Messages[0]["content"] != "Think about it" {
		t.Fatalf("messages after the crash = %v", th.Messages)
	}

	// Act: a new message on the same thread.
	next := p2.send("alice", thread, "Are you back?")
	nextEvents := p2.finishedRun("alice", next)

	// Assert: it runs to completion, with the interrupted turn's user message in its history.
	if got := p2.run("alice", next).Status; got != "completed" {
		t.Fatalf("new run status = %s (%v)", got, types(nextEvents))
	}
	if last := p2.thread("alice", thread).Messages; last[len(last)-1]["content"] != "Reply to: Are you back?" {
		t.Fatalf("messages after the new run: %v", last)
	}
}

// snapshotJournal copies the journal of a live state directory into a new directory.
func snapshotJournal(t *testing.T, dir string) string {
	t.Helper()
	src, err := os.Open(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	snap := t.TempDir()
	dst, err := os.Create(filepath.Join(snap, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	return snap
}
