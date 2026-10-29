package clienttest

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/core/agui"
	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/hosts/client"
)

func wantErr(t *testing.T, what string, err error, want *client.Error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("%s: error %v, want %s", what, err, want.Code)
	}
}

func testModels(t *testing.T, mk Factory) {
	r := newRig(t, mk, rigOpts{})

	m, err := r.alice.Models(r.ctx)

	if err != nil || m.Default != DefaultModel || len(m.Models) != 1 || m.Models[0] != DefaultModel {
		t.Fatalf("Models = %+v, %v", m, err)
	}
}

func testThreadLifecycle(t *testing.T, mk Factory) {
	// Arrange
	r := newRig(t, mk, rigOpts{})

	// Act
	th, err := r.alice.CreateThread(r.ctx, "First")
	if err != nil || th.ID == "" || th.Title != "First" || th.Pinned || th.ActiveRun != nil {
		t.Fatalf("CreateThread = %+v, %v", th, err)
	}
	title, pinned := "Renamed", true
	renamed, err := r.alice.RenameThread(r.ctx, th.ID, client.ThreadPatch{Title: &title, Pinned: &pinned})
	if err != nil || renamed.Title != "Renamed" || !renamed.Pinned {
		t.Fatalf("RenameThread = %+v, %v", renamed, err)
	}
	other, _ := r.alice.CreateThread(r.ctx, "Second")
	list, err := r.alice.ListThreads(r.ctx, 10)
	got, err2 := r.alice.GetThread(r.ctx, th.ID)

	// Assert
	if err != nil || len(list) != 2 || list[0].ID != th.ID || list[1].ID != other.ID {
		t.Fatalf("ListThreads = %+v, %v (the pinned thread comes first)", list, err)
	}
	if err2 != nil || got.Title != "Renamed" {
		t.Fatalf("GetThread = %+v, %v", got, err2)
	}
	_, err = r.alice.RenameThread(r.ctx, th.ID, client.ThreadPatch{})
	wantErr(t, "empty patch", err, client.ErrInvalidRequest)
	blank := "   "
	_, err = r.alice.RenameThread(r.ctx, th.ID, client.ThreadPatch{Title: &blank})
	wantErr(t, "blank title", err, client.ErrInvalidRequest)

	if err := r.alice.DeleteThread(r.ctx, th.ID); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}
	_, err = r.alice.GetThread(r.ctx, th.ID)
	wantErr(t, "deleted thread", err, client.ErrNotFound)
}

func testSandboxMode(t *testing.T, mk Factory) {
	// Arrange
	r := newRig(t, mk, rigOpts{})
	id := newThread(t, r, r.alice)

	// Act
	off := mustSend(t, r, r.alice, id, "Say hello", client.SendOptions{Sandbox: "off"})
	stream(t, r, r.alice, off.ID, 0)
	waitIdle(t, r, r.alice, id)
	got, err := r.alice.GetRun(r.ctx, off.ID)
	_, badErr := r.alice.Send(r.ctx, id, "again", client.SendOptions{Sandbox: "sometimes"})

	// Assert
	if err != nil || got.Sandbox != "off" || off.Sandbox != "off" {
		t.Fatalf("the sandbox mode must round-trip: sent %q, read %q, err %v", off.Sandbox, got.Sandbox, err)
	}
	if !errors.Is(badErr, client.ErrInvalidRequest) {
		t.Fatalf("an unknown sandbox mode must be an invalid request, got %v", badErr)
	}
}

func testSendAndStream(t *testing.T, mk Factory) {
	// Arrange
	r := newRig(t, mk, rigOpts{})
	id := newThread(t, r, r.alice)

	// Act
	run := mustSend(t, r, r.alice, id, "Say hello", client.SendOptions{})
	evs := stream(t, r, r.alice, run.ID, 0)
	th := waitIdle(t, r, r.alice, id)
	got, err := r.alice.GetRun(r.ctx, run.ID)

	// Assert
	if outcome, _ := finished(t, evs); outcome == "interrupt" || outcome == "cancelled" {
		t.Fatalf("outcome %q", outcome)
	}
	if evs[0].Type != agui.RunStarted {
		t.Fatalf("first event %q", evs[0].Type)
	}
	var text strings.Builder
	for i, e := range evs {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d: seqs must count up from 1", i, e.Seq)
		}
		if len(e.Data) == 0 {
			t.Fatalf("event %d has no data", i)
		}
		if e.Type == agui.TextMessageContent {
			text.WriteString(string(e.Data))
		}
	}
	if !strings.Contains(text.String(), "Hi there") {
		t.Fatalf("streamed text %q", text.String())
	}
	if err != nil || got.Status == "queued" || got.Status == "running" || got.LastSeq != evs[len(evs)-1].Seq || got.Model != DefaultModel {
		t.Fatalf("GetRun = %+v, %v", got, err)
	}
	var roles []string
	for _, m := range th.Messages {
		roles = append(roles, m.Role)
	}
	if len(th.Messages) < 2 || th.Messages[0].Role != "user" || th.Messages[0].Content != "Say hello" || roles[len(roles)-1] != "assistant" {
		t.Fatalf("messages %v", roles)
	}
}

func testResumeFromAfter(t *testing.T, mk Factory) {
	// Arrange
	r := newRig(t, mk, rigOpts{})
	id := newThread(t, r, r.alice)
	run := mustSend(t, r, r.alice, id, "hello", client.SendOptions{})
	all := stream(t, r, r.alice, run.ID, 0)
	finished(t, all)
	cut := all[len(all)/2].Seq

	// Act
	tail := stream(t, r, r.alice, run.ID, cut)
	last := stream(t, r, r.alice, run.ID, all[len(all)-2].Seq)

	// Assert
	want := all[len(all)/2+1:]
	if len(tail) != len(want) {
		t.Fatalf("after %d: %d events, want %d", cut, len(tail), len(want))
	}
	for i := range want {
		if tail[i].Seq != want[i].Seq || tail[i].Type != want[i].Type || string(tail[i].Data) != string(want[i].Data) {
			t.Fatalf("event %d after %d: %d/%s, want %d/%s", i, cut, tail[i].Seq, tail[i].Type, want[i].Seq, want[i].Type)
		}
	}
	if len(last) != 1 || last[0].Type != agui.RunFinished {
		t.Fatalf("after the penultimate seq: %d events, want only RUN_FINISHED", len(last))
	}
}

func testSteer(t *testing.T, mk Factory) {
	// Arrange
	model, started, release := blocking()
	defer release()
	r := newRig(t, mk, rigOpts{model: model})
	id := newThread(t, r, r.alice)
	run := mustSend(t, r, r.alice, id, "work", client.SendOptions{})
	waitFor(t, started)

	// Act
	err := r.alice.Steer(r.ctx, run.ID, "also check the tests")
	release()
	evs := stream(t, r, r.alice, run.ID, 0)
	waitIdle(t, r, r.alice, id)
	late := r.alice.Steer(r.ctx, run.ID, "too late")

	// Assert
	if err != nil {
		t.Fatalf("Steer a running run: %v", err)
	}
	finished(t, evs)
	wantErr(t, "steer a finished run", late, client.ErrRunNotActive)
}

func testCancel(t *testing.T, mk Factory) {
	// Arrange
	model, started, release := blocking()
	defer release()
	r := newRig(t, mk, rigOpts{model: model})
	id := newThread(t, r, r.alice)
	run := mustSend(t, r, r.alice, id, "work", client.SendOptions{})
	waitFor(t, started)

	// Act
	err := r.alice.Cancel(r.ctx, run.ID)
	release()
	evs := stream(t, r, r.alice, run.ID, 0)
	th := waitIdle(t, r, r.alice, id)
	again := r.alice.Cancel(r.ctx, run.ID)

	// Assert
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if outcome, _ := finished(t, evs); outcome != "cancelled" {
		t.Fatalf("outcome %q, want cancelled", outcome)
	}
	if th.ActiveRun != nil || again != nil {
		t.Fatalf("active %v, second cancel %v", th.ActiveRun, again)
	}
}

func testAskUser(t *testing.T, mk Factory) {
	// Arrange: the model asks once, and answers when it sees the user's answer in the history.
	model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		for _, m := range c.Messages {
			if m.Role == schema.Tool && strings.Contains(m.Content, "SQLite") {
				return llmtest.Turn{Chunks: llmtest.Text("Using SQLite")}
			}
		}
		if len(c.Tools) > 0 {
			return llmtest.Turn{Chunks: []*schema.Message{llmtest.Finish(llmtest.ToolCall(0, "q1", "ask_user",
				`{"question":"Which database?","options":[{"label":"Postgres"},{"label":"SQLite"}]}`), "tool_calls")}}
		}
		return llmtest.Turn{Chunks: llmtest.Text("A title")}
	})
	r := newRig(t, mk, rigOpts{model: model})
	id := newThread(t, r, r.alice)
	run := mustSend(t, r, r.alice, id, "set up storage", client.SendOptions{})
	outcome, interrupts := finished(t, stream(t, r, r.alice, run.ID, 0))
	th := waitIdle(t, r, r.alice, id)
	if outcome != "interrupt" || len(interrupts) != 1 {
		t.Fatalf("outcome %q interrupts %v", outcome, interrupts)
	}

	// Act
	_, blank := r.alice.Resume(r.ctx, id, client.ResumeRequest{InterruptID: interrupts[0].ID, Reason: interrupts[0].Reason})
	resumed, err := r.alice.Resume(r.ctx, id, client.ResumeRequest{InterruptID: interrupts[0].ID, Reason: interrupts[0].Reason, Text: "SQLite"})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	evs := stream(t, r, r.alice, resumed.ID, 0)
	after := waitIdle(t, r, r.alice, id)

	// Assert
	if th.PendingQuestion == nil || th.PendingQuestion.Question != "Which database?" || th.PendingQuestion.InterruptID != interrupts[0].ID {
		t.Fatalf("pending question %+v", th.PendingQuestion)
	}
	wantErr(t, "an empty answer", blank, client.ErrInvalidRequest)
	var text strings.Builder
	for _, e := range evs {
		if e.Type == agui.TextMessageContent {
			text.WriteString(string(e.Data))
		}
	}
	if out, _ := finished(t, evs); out == "interrupt" || !strings.Contains(text.String(), "Using SQLite") || after.PendingQuestion != nil {
		t.Fatalf("outcome %q text %q pending %+v", out, text.String(), after.PendingQuestion)
	}
}

func testValidation(t *testing.T, mk Factory) {
	// Arrange
	r := newRig(t, mk, rigOpts{})
	id := newThread(t, r, r.alice)

	// Act
	_, blank := r.alice.Send(r.ctx, id, "  ", client.SendOptions{})
	_, badModel := r.alice.Send(r.ctx, id, "hi", client.SendOptions{Model: "nope"})
	_, noThread := r.alice.Send(r.ctx, "no-such-thread", "hi", client.SendOptions{})
	_, noRun := r.alice.GetRun(r.ctx, "no-such-run")
	_, noEvents := r.alice.Events(r.ctx, "no-such-run", 0)
	_, noInterrupt := r.alice.Resume(r.ctx, id, client.ResumeRequest{})
	_, badDecision := r.alice.Resume(r.ctx, id, client.ResumeRequest{InterruptID: "x", Reason: client.ReasonToolApproval, Decision: "maybe"})
	badSteer := r.alice.Steer(r.ctx, "no-such-run", "x")

	// Assert
	wantErr(t, "blank message", blank, client.ErrInvalidRequest)
	wantErr(t, "unknown model", badModel, client.ErrInvalidRequest)
	wantErr(t, "unknown thread", noThread, client.ErrNotFound)
	wantErr(t, "unknown run", noRun, client.ErrNotFound)
	wantErr(t, "events of an unknown run", noEvents, client.ErrNotFound)
	wantErr(t, "resume without an interrupt", noInterrupt, client.ErrInvalidRequest)
	wantErr(t, "resume with a bad decision", badDecision, client.ErrInvalidRequest)
	wantErr(t, "steer an unknown run", badSteer, client.ErrNotFound)
}

func testOwnership(t *testing.T, mk Factory) {
	// Arrange: alice has a thread with a finished run.
	r := newRig(t, mk, rigOpts{})
	id := newThread(t, r, r.alice)
	run := mustSend(t, r, r.alice, id, "private", client.SendOptions{})
	finished(t, stream(t, r, r.alice, run.ID, 0))
	waitIdle(t, r, r.alice, id)

	// Act: bob tries everything on alice's thread and run.
	_, getThread := r.bob.GetThread(r.ctx, id)
	_, send := r.bob.Send(r.ctx, id, "hi", client.SendOptions{})
	_, getRun := r.bob.GetRun(r.ctx, run.ID)
	_, events := r.bob.Events(r.ctx, run.ID, 0)
	steer := r.bob.Steer(r.ctx, run.ID, "x")
	cancel := r.bob.Cancel(r.ctx, run.ID)
	title := "mine now"
	_, rename := r.bob.RenameThread(r.ctx, id, client.ThreadPatch{Title: &title})
	del := r.bob.DeleteThread(r.ctx, id)
	_, resume := r.bob.Resume(r.ctx, id, client.ResumeRequest{InterruptID: "x", Decision: client.DecisionApprove})
	_, files := r.bob.ListFiles(r.ctx, id, "")
	_, download := r.bob.DownloadFile(r.ctx, id, "a.txt")
	bobs, listErr := r.bob.ListThreads(r.ctx, 50)
	mine, err := r.alice.GetThread(r.ctx, id)

	// Assert
	for what, got := range map[string]error{"GetThread": getThread, "Send": send, "GetRun": getRun, "Events": events, "Steer": steer,
		"Cancel": cancel, "RenameThread": rename, "DeleteThread": del, "Resume": resume, "ListFiles": files, "DownloadFile": download} {
		wantErr(t, what, got, client.ErrNotFound)
	}
	if listErr != nil || len(bobs) != 0 {
		t.Fatalf("bob sees threads %+v, %v", bobs, listErr)
	}
	if err != nil || mine.Title == "mine now" {
		t.Fatalf("alice's thread changed: %+v, %v", mine, err)
	}
}

func testSessions(t *testing.T, mk Factory) {
	r := newRig(t, mk, rigOpts{})

	_, err := r.alice.Sessions(r.ctx, 10)

	wantErr(t, "the in-memory store has no sessions", err, client.ErrNotSupported)
}

// waitFor waits for a channel to close.
func waitFor(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(20 * time.Second):
		t.Fatal("the model was not called")
	}
}
