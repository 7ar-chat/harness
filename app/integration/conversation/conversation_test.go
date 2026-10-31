package conversation_test

import (
	"slices"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/engine/tools"
)

// lookupModel answers "first question" with a call of the lookup tool and then, once it has the result, with a
// three-chunk answer. Any other question gets a one-chunk answer. Calls without tools get the title.
func lookupModel() *llmtest.Model {
	return llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if titleCall(c) {
			return titleTurn()
		}
		last := c.Messages[len(c.Messages)-1]
		switch {
		case last.Role == ai.RoleTool:
			return llmtest.Turn{Chunks: llmtest.Text("The ", "answer ", "is 42")}
		case last.Content == "first question":
			return llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, "call-1", "lookup", `{"q":"meaning"}`), "tool_calls")}}
		}
		return llmtest.Turn{Chunks: llmtest.Text("Second answer")}
	})
}

// openLookup is a lookup tool that answers 42 at once.
func openLookup() *gateTool {
	g := newGateTool("lookup", "42")
	g.open()
	return g
}

func without(evs []event, typ string) []string {
	var out []string
	for _, e := range evs {
		if e.typ() != typ {
			out = append(out, e.typ())
		}
	}
	return out
}

func TestStreamedRunEmitsOrderedGaplessEventsAndTheScriptedAnswer(t *testing.T) {
	// Arrange
	r := newRig(t, rigOpts{model: lookupModel(), tools: []tools.Provider{openLookup()}})
	id := r.newThread("alice")

	// Act
	runID := r.send("alice", id, "first question", false)
	evs := r.replay(runID, "alice")

	// Assert
	want := []string{"RUN_STARTED", "STEP_STARTED", "TOOL_CALL_START", "TOOL_CALL_ARGS", "TOOL_CALL_END", "TOOL_CALL_RESULT", "STEP_FINISHED",
		"STEP_STARTED", "TEXT_MESSAGE_START", "TEXT_MESSAGE_CONTENT", "TEXT_MESSAGE_CONTENT", "TEXT_MESSAGE_CONTENT", "TEXT_MESSAGE_END", "STEP_FINISHED",
		"RUN_FINISHED"}
	if got := without(evs, "CUSTOM"); !slices.Equal(got, want) {
		t.Fatalf("event types (customs left out):\n got %v\nwant %v", got, want)
	}
	for i, e := range evs {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d (%s) has seq %d: ids must be 1..n without gaps", i, e.typ(), e.Seq)
		}
	}
	text, chunks := deltas(evs)
	if text != "The answer is 42" || chunks != 3 {
		t.Fatalf("deltas concatenate to %q in %d chunks, want %q in 3", text, chunks, "The answer is 42")
	}
	if typ, _ := outcome(t, evs); typ != "success" {
		t.Fatalf("outcome %q, want success", typ)
	}
	if evs[0].fields()["runId"] != runID || evs[0].fields()["threadId"] != id {
		t.Fatalf("RUN_STARTED = %s, want run %s on thread %s", evs[0].Data, runID, id)
	}
	if run := r.run("alice", runID); run["status"] != "completed" || run["lastSeq"] != float64(len(evs)) {
		t.Fatalf("run = %v, want completed with lastSeq %d", run, len(evs))
	}
}

func TestReconnectingWithLastEventIDOrAfterReplaysExactlyTheRemainingEvents(t *testing.T) {
	// Arrange
	r := newRig(t, rigOpts{model: lookupModel(), tools: []tools.Provider{openLookup()}})
	id := r.newThread("alice")
	runID := r.send("alice", id, "first question", false)
	full := r.replay(runID, "alice")
	r.waitIdle("alice", id)
	mid := full[len(full)/2].Seq
	remaining := full[mid:] // events with seq > mid (seq i+1 sits at index i)

	// Act
	byHeader := func() []event {
		s := r.open(runID, "alice", "", "Last-Event-ID", itoa(mid))
		return s.all()
	}()
	byQuery := func() []event {
		s := r.open(runID, "alice", "?after="+itoa(mid))
		return s.all()
	}()
	fromTheEnd := r.open(runID, "alice", "", "Last-Event-ID", itoa(full[len(full)-1].Seq)).all()

	// Assert
	for name, got := range map[string][]event{"Last-Event-ID": byHeader, "?after=": byQuery} {
		if !slices.Equal(got, remaining) {
			t.Errorf("%s=%d: got %v, want the %d remaining events %v", name, mid, seqs(got), len(remaining), seqs(remaining))
		}
	}
	if len(remaining) == 0 || remaining[0].Seq != mid+1 {
		t.Fatalf("control: the remaining events %v do not start after %d", seqs(remaining), mid)
	}
	if len(fromTheEnd) != 0 {
		t.Errorf("reconnecting after the last event replayed %v, want a stream that just ends", types(fromTheEnd))
	}
}

func TestReconnectDuringALiveRunResumesAfterTheLastReceivedEvent(t *testing.T) {
	// Arrange: the lookup tool holds the run between the tool-call events and the answer.
	gate := newGateTool("lookup", "42")
	r := newRig(t, rigOpts{model: lookupModel(), tools: []tools.Provider{gate}})
	id := r.newThread("alice")
	runID := r.send("alice", id, "first question", false)
	first := r.open(runID, "alice", "")

	// Act: read up to the end of the tool call, drop the connection, let the run go on, then reconnect.
	head := first.until(func(e event) bool { return e.typ() == "TOOL_CALL_END" })
	gate.waitEntered(t)
	first.Close()
	gate.open()
	tail := r.open(runID, "alice", "", "Last-Event-ID", itoa(head[len(head)-1].Seq)).all()
	r.waitIdle("alice", id)

	// Assert
	full := r.replay(runID, "alice")
	if got := append(slices.Clone(head), tail...); !slices.Equal(got, full) {
		t.Fatalf("head+tail = %v\n full replay = %v", types(got), types(full))
	}
	if tail[0].Seq != head[len(head)-1].Seq+1 || tail[0].typ() != "TOOL_CALL_RESULT" {
		t.Errorf("the resumed stream starts with %d %s, want %d TOOL_CALL_RESULT", tail[0].Seq, tail[0].typ(), head[len(head)-1].Seq+1)
	}
	if text, _ := deltas(tail); text != "The answer is 42" {
		t.Errorf("answer received after the reconnect: %q", text)
	}
	if typ, _ := outcome(t, tail); typ != "success" {
		t.Errorf("outcome after the reconnect: %q", typ)
	}
}

func TestTranscriptKeepsBothTurnsInOrderAndTheSecondRequestSeesTheFirst(t *testing.T) {
	// Arrange
	model := lookupModel()
	r := newRig(t, rigOpts{model: model, tools: []tools.Provider{openLookup()}})
	id := r.newThread("alice")
	other := r.newThread("bob")
	run1 := r.send("alice", id, "first question", false)
	r.replay(run1, "alice")
	r.waitIdle("alice", id)

	// Act
	run2 := r.send("alice", id, "second question", false)
	evs2 := r.replay(run2, "alice")
	th := r.waitIdle("alice", id)

	// Assert: the transcript.
	type row struct{ role, content, runID string }
	var got []row
	for _, m := range th["messages"].([]any) {
		m := m.(map[string]any)
		got = append(got, row{m["role"].(string), m["content"].(string), m["runId"].(string)})
	}
	want := []row{
		{"user", "first question", run1}, {"assistant", "", run1}, {"tool", "42", run1}, {"assistant", "The answer is 42", run1},
		{"user", "second question", run2}, {"assistant", "Second answer", run2},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("transcript:\n got %v\nwant %v", got, want)
	}
	msgs := th["messages"].([]any)
	calls := msgs[1].(map[string]any)["toolCalls"].([]any)
	fn := calls[0].(map[string]any)
	if fn["id"] != "call-1" || fn["function"].(map[string]any)["name"] != "lookup" || fn["function"].(map[string]any)["arguments"] != `{"q":"meaning"}` {
		t.Errorf("assistant tool call = %v", fn)
	}
	if msgs[2].(map[string]any)["toolCallId"] != "call-1" {
		t.Errorf("tool message answers %v, want call-1", msgs[2].(map[string]any)["toolCallId"])
	}
	if text, _ := deltas(evs2); text != "Second answer" {
		t.Errorf("second run streamed %q", text)
	}

	// Assert: the model saw the first turn when answering the second message.
	agent := agentCalls(model)
	wantHistory := []string{"user:first question", "assistant:", "tool:42", "assistant:The answer is 42", "user:second question"}
	if h := history(agent[len(agent)-1]); !slices.Equal(h, wantHistory) {
		t.Errorf("request history of the second turn:\n got %q\nwant %q", h, wantHistory)
	}

	// Assert: the title comes from the title call, and the listing is the user's own.
	waitFor(t, "the generated title", func() bool { return r.thread("alice", id)["title"] == "A title" })
	list := func(user string) []any {
		status, b := r.request("GET", "/api/threads", user, "")
		if status != 200 {
			t.Fatalf("GET /api/threads as %s: %d %s", user, status, b)
		}
		return decodeList(t, b)
	}
	alice := list("alice")
	if len(alice) != 1 || alice[0].(map[string]any)["id"] != id || alice[0].(map[string]any)["title"] != "A title" {
		t.Errorf("alice's threads = %v, want only %s titled %q", alice, id, "A title")
	}
	if bob := list("bob"); len(bob) != 1 || bob[0].(map[string]any)["id"] != other {
		t.Errorf("bob's threads = %v, want only %s (control: the listing is per user)", bob, other)
	}
}
