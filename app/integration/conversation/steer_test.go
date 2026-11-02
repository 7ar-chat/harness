package conversation_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/engine/tools"
)

// paintModel calls the "paint" tool for "paint the wall" and answers "Painted" to its result.
func paintModel() *llmtest.Model {
	return llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if titleCall(c) {
			return titleTurn()
		}
		if c.Messages[len(c.Messages)-1].Role == ai.RoleUser && !strings.HasPrefix(c.Messages[len(c.Messages)-1].Content, agent.SteerPrefix) {
			return llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, "c1", "paint", `{}`), "tool_calls")}}
		}
		return llmtest.Turn{Chunks: llmtest.Text("Painted")}
	})
}

func TestSteeringARunningRunReachesTheNextModelRequestTheStreamAndTheTranscript(t *testing.T) {
	// Arrange
	paint := newGateTool("paint", "red done")
	model := paintModel()
	r := newRig(t, rigOpts{model: model, tools: []tools.Provider{paint}})
	id := r.newThread("alice")
	runID := r.send("alice", id, "paint the wall", false)
	paint.waitEntered(t)

	// Act
	status, body := r.request("POST", "/api/runs/"+runID+"/steer", "alice", `{"text":"also use blue"}`)
	paint.open()
	evs := r.replay(runID, "alice")
	th := r.waitIdle("alice", id)

	// Assert: accepted, and given to the model before its next call, after the tool result.
	if status != 202 {
		t.Fatalf("steer = %d %s, want 202", status, body)
	}
	calls := agentCalls(model)
	if len(calls) != 2 {
		t.Fatalf("%d agent model calls, want 2", len(calls))
	}
	// (The solo API has no author identity, so the steer carries no "[login] " tag.)
	want := []string{"user:paint the wall", "assistant:", "tool:red done", "user:" + agent.SteerPrefix + "also use blue"}
	if got := history(calls[1]); !slices.Equal(got, want) {
		t.Errorf("second request history:\n got %q\nwant %q", got, want)
	}
	if first := history(calls[0]); slices.ContainsFunc(first, func(s string) bool { return strings.Contains(s, "also use blue") }) {
		t.Errorf("control: the first request already holds the steer: %q", first)
	}

	// Assert: announced in the stream.
	var steers []event
	for _, e := range evs {
		if e.custom() == "work.steer" {
			steers = append(steers, e)
		}
	}
	if len(steers) != 1 {
		t.Fatalf("%d work.steer events in %v, want 1", len(steers), types(evs))
	}
	v := steers[0].fields()["value"].(map[string]any)
	if v["text"] != "also use blue" || v["delivered"] != true || v["id"] == "" {
		t.Errorf("work.steer value = %v", v)
	}
	if typ, _ := outcome(t, evs); typ != "success" {
		t.Errorf("outcome %q", typ)
	}

	// Assert: persisted as a flagged user message between the tool result and the answer.
	type row struct{ role, content string }
	var got []row
	var steerMsg map[string]any
	for _, m := range th["messages"].([]any) {
		m := m.(map[string]any)
		got = append(got, row{m["role"].(string), m["content"].(string)})
		if md, _ := m["metadata"].(map[string]any); md["steer"] == true {
			steerMsg = m
		}
	}
	wantRows := []row{{"user", "paint the wall"}, {"assistant", ""}, {"tool", "red done"}, {"user", "also use blue"}, {"assistant", "Painted"}}
	if !slices.Equal(got, wantRows) {
		t.Errorf("transcript:\n got %v\nwant %v", got, wantRows)
	}
	if steerMsg == nil || steerMsg["content"] != "also use blue" || steerMsg["runId"] != runID || steerMsg["id"] != v["id"] {
		t.Errorf("steer message = %v, want metadata.steer=true in run %s with the event's id %v", steerMsg, runID, v["id"])
	}
}

func TestSteerIsRefusedForFinishedRunsEmptyTextAndOtherUsers(t *testing.T) {
	// Arrange
	paint := newGateTool("paint", "red done")
	model := paintModel()
	r := newRig(t, rigOpts{model: model, tools: []tools.Provider{paint}})
	id := r.newThread("alice")
	runID := r.send("alice", id, "paint the wall", false)
	paint.waitEntered(t)

	// Act
	empty, _ := r.request("POST", "/api/runs/"+runID+"/steer", "alice", `{"text":"  "}`)
	missing, _ := r.request("POST", "/api/runs/"+runID+"/steer", "alice", `{}`)
	foreign, _ := r.request("POST", "/api/runs/"+runID+"/steer", "mallory", `{"text":"burn it down"}`)
	owner, _ := r.request("POST", "/api/runs/"+runID+"/steer", "alice", `{"text":"use green"}`)
	paint.open()
	evs := r.replay(runID, "alice")
	th := r.waitIdle("alice", id)
	late, lateBody := r.request("POST", "/api/runs/"+runID+"/steer", "alice", `{"text":"too late"}`)

	// Assert
	if empty != 400 || missing != 400 {
		t.Errorf("empty steer: %d, missing text: %d, want 400 for both", empty, missing)
	}
	if foreign != 404 {
		t.Errorf("another user's steer = %d, want 404", foreign)
	}
	if owner != 202 {
		t.Errorf("control: the owner's steer = %d, want 202", owner)
	}
	if late != 409 {
		t.Errorf("steering a finished run = %d %s, want 409", late, lateBody)
	}
	if typ, _ := outcome(t, evs); typ != "success" {
		t.Errorf("outcome %q", typ)
	}
	all := strings.Join(transcript(th), "\n")
	for _, bad := range []string{"burn it down", "too late"} {
		if strings.Contains(all, bad) || slices.ContainsFunc(agentCalls(model)[1].Messages, func(m *ai.Message) bool { return strings.Contains(m.Content, bad) }) {
			t.Errorf("the refused steer %q reached the transcript or the model", bad)
		}
	}
	if !strings.Contains(all, "use green") {
		t.Errorf("control: the owner's steer is missing from the transcript:\n%s", all)
	}
	n := 0
	for _, e := range evs {
		if e.custom() == "work.steer" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d work.steer events, want only the owner's", n)
	}
}
