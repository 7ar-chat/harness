package split_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestAMessageSentThroughTheWebTierIsExecutedByAWorkerProcessAndStreamedBack(t *testing.T) {
	// Arrange: the web tier only queues; worker-1 is the only consumer.
	r := newRig(t)
	w := r.startWorker("worker-1")
	thread := r.newThread()

	// Act
	run := r.send(thread, "hello there")
	evs := r.stream(run, 0)

	// Assert: the stream is complete and ordered.
	types := eventTypes(evs)
	if types[0] != "RUN_STARTED" || types[len(types)-1] != "RUN_FINISHED" {
		t.Fatalf("event types %v", types)
	}
	for i, e := range evs {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d (%s) has seq %d, want %d (no gaps)", i, e.Type, e.Seq, i+1)
		}
	}
	if got := streamedText(evs); got != "Reply to: hello there" {
		t.Fatalf("streamed text %q", got)
	}
	// Assert: the run row says who ran it and how it ended.
	r.waitStatus(run, "completed")
	if owner := r.runtimeOf(run); owner != w.id {
		t.Fatalf("the run was executed by %q, want %q", owner, w.id)
	}
	// Assert: the history holds both sides of the exchange.
	msgs := r.messages(thread)
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[0].Content != "hello there" ||
		msgs[1].Role != "assistant" || msgs[1].Content != "Reply to: hello there" {
		t.Fatalf("messages %+v", msgs)
	}
}

func TestAToolTurnRunsOnTheWorkersDiskAndItsResultIsInTheHistory(t *testing.T) {
	// Arrange
	r := newRig(t)
	w := r.startWorker("worker-1")
	thread := r.newThread()

	// Act
	run := r.send(thread, "please write the file")
	evs := r.stream(run, 0)

	// Assert: the tool call is streamed between the start and the end of the run.
	types := eventTypes(evs)
	if types[0] != "RUN_STARTED" || types[len(types)-1] != "RUN_FINISHED" {
		t.Fatalf("event types %v", types)
	}
	start := slices.Index(types, "TOOL_CALL_START")
	result := slices.Index(types, "TOOL_CALL_RESULT")
	if start < 0 || result < start || evs[start].field("toolCallName") != "write_file" {
		t.Fatalf("tool events %v", types)
	}
	if got := streamedText(evs); got != "Wrote out.md" {
		t.Fatalf("streamed text %q", got)
	}
	// Assert: the file is on the disk of the worker process.
	body, err := os.ReadFile(filepath.Join(w.workspace, "out.md"))
	if err != nil || string(body) != "hello" {
		t.Fatalf("out.md on the worker: %q, %v\nworker stderr:\n%s", body, err, w.stderr.String())
	}
	// Assert: the history is user, assistant with the call, tool result, final assistant.
	r.waitStatus(run, "completed")
	msgs := r.messages(thread)
	var roles []string
	for _, m := range msgs {
		roles = append(roles, m.Role)
	}
	if !reflect.DeepEqual(roles, []string{"user", "assistant", "tool", "assistant"}) {
		t.Fatalf("roles %v", roles)
	}
	if len(msgs[1].ToolCalls) != 1 || msgs[1].ToolCalls[0].Function.Name != "write_file" ||
		msgs[2].ToolCallID == "" || msgs[3].Content != "Wrote out.md" {
		t.Fatalf("messages %+v", msgs)
	}
}

func TestTwoWorkersAtConcurrencyOneShareTheLoad(t *testing.T) {
	// Arrange: no model reply is sent before two model requests are in flight at once; each worker runs one
	// run at a time, so the barrier opens only if both workers hold one run each.
	r := newRig(t)
	r.llm.Barrier(2)
	r.startWorker("worker-1")
	r.startWorker("worker-2")
	runs := []string{r.send(r.newThread(), "first"), r.send(r.newThread(), "second")}

	// Act
	for _, id := range runs {
		r.stream(id, 0)
	}

	// Assert
	var owners []string
	for _, id := range runs {
		r.waitStatus(id, "completed")
		owners = append(owners, r.runtimeOf(id))
	}
	slices.Sort(owners)
	if !reflect.DeepEqual(owners, []string{"worker-1", "worker-2"}) {
		t.Fatalf("the runs were executed by %v, want both workers", owners)
	}
}

func TestAWorkerKilledMidRunLeavesTheRunInterruptedAndTheThreadUsable(t *testing.T) {
	// Arrange: worker-1 holds a run whose model reply is held by the fake.
	r := newRig(t)
	hold := r.llm.Hold("slow")
	w1 := r.startWorker("worker-1")
	thread := r.newThread()
	run := r.send(thread, "slow question")
	<-hold.arrived
	r.waitStatus(run, "running")
	live := make(chan []sseEvent, 1)
	go func() { live <- r.stream(run, 0) }()

	// Act: the worker dies without any chance to clean up; the heartbeat goes stale; a live runtime sweeps.
	w1.Kill()
	r.ageHeartbeat(run)
	r.web.Runtime.Sweep(context.Background())

	// Assert: the run is interrupted and the open stream ends with RUN_ERROR.
	r.waitStatus(run, "interrupted")
	evs := <-live
	last := evs[len(evs)-1]
	if evs[0].Type != "RUN_STARTED" || last.Type != "RUN_ERROR" || !strings.Contains(last.Raw, "interrupted") {
		t.Fatalf("live stream %v, last %s", eventTypes(evs), last.Raw)
	}
	// Assert: the same history can be replayed from seq 0.
	if replay := r.stream(run, 0); !reflect.DeepEqual(eventTypes(replay), eventTypes(evs)) {
		t.Fatalf("replay %v differs from the live stream %v", eventTypes(replay), eventTypes(evs))
	}

	// Act: a new message to the same thread is served by a surviving worker.
	w2 := r.startWorker("worker-2")
	next := r.send(thread, "are you there")
	nextEvs := r.stream(next, 0)

	// Assert
	if got := streamedText(nextEvs); got != "Reply to: are you there" {
		t.Fatalf("streamed text %q", got)
	}
	r.waitStatus(next, "completed")
	if owner := r.runtimeOf(next); owner != w2.id {
		t.Fatalf("the new run was executed by %q, want %q", owner, w2.id)
	}
	var got []string
	for _, m := range r.messages(thread) {
		got = append(got, m.Role+": "+m.Content)
	}
	want := []string{"user: slow question", "user: are you there", "assistant: Reply to: are you there"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("history %v", got)
	}
}

func TestCancellingARunThroughTheWebTierStopsItOnAnotherProcessAndTheWorkerKeepsServing(t *testing.T) {
	// Arrange
	r := newRig(t)
	hold := r.llm.Hold("slow")
	w := r.startWorker("worker-1")
	thread := r.newThread()
	run := r.send(thread, "slow question")
	<-hold.arrived
	r.waitStatus(run, "running")
	live := make(chan []sseEvent, 1)
	go func() { live <- r.stream(run, 0) }()

	// Act: the web tier cancels; the run lives in the worker process.
	r.call("POST", "/api/runs/"+run+"/cancel", "", 202)

	// Assert: the stream ends cancelled and so does the row.
	evs := <-live
	last := evs[len(evs)-1]
	if last.Type != "RUN_FINISHED" || !strings.Contains(last.Raw, `"cancelled"`) {
		t.Fatalf("stream %v, last %s", eventTypes(evs), last.Raw)
	}
	r.waitStatus(run, "cancelled")

	// Assert: the worker is alive and executes the next run.
	if !w.Ready() {
		t.Fatalf("the worker is no longer ready after the cancellation:\n%s", w.stderr.String())
	}
	next := r.send(thread, "still there")
	if got := streamedText(r.stream(next, 0)); got != "Reply to: still there" {
		t.Fatalf("streamed text %q", got)
	}
	r.waitStatus(next, "completed")
	if owner := r.runtimeOf(next); owner != w.id {
		t.Fatalf("the next run was executed by %q, want %q", owner, w.id)
	}
}
