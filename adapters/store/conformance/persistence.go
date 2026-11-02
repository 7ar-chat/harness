package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/store"
)

// Persistence runs the restart suite: everything a persisting backend (Backend.Reopen != nil) stores must come back
// identical after the store is closed and opened again. Backends that do not persist skip every subtest.
func Persistence(t *testing.T, f Factory) {
	t.Helper()
	t.Run("Threads", func(t *testing.T) { persThreads(t, f) })
	t.Run("Messages", func(t *testing.T) { persMessagesSuite(t, f) })
	t.Run("Runs", func(t *testing.T) { persRuns(t, f) })
	t.Run("Approvals", func(t *testing.T) { persApprovals(t, f) })
	t.Run("Harness", func(t *testing.T) { persHarness(t, f) })
	t.Run("Deletion", func(t *testing.T) { persDeletion(t, f) })
	t.Run("Repeated", func(t *testing.T) { persRepeated(t, f) })
}

// persStale is the heartbeat age InterruptStaleRuns is given; the tests advance the clock past it.
const persStale = time.Minute

// persBackend returns a fresh backend, skipping the test when it cannot reopen.
func persBackend(t *testing.T, f Factory) Backend {
	t.Helper()
	b := f(t)
	if b.Reopen == nil {
		t.Skip("the backend does not persist between instances")
	}
	return b
}

// persJSON renders v for comparisons: times lose their location and monotonic clock, numbers their Go type.
func persJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	return string(b)
}

func persSame(t *testing.T, what, before, after string) {
	t.Helper()
	if before != after {
		t.Errorf("%s changed over the restart\nbefore %s\nafter  %s", what, before, after)
	}
}

func persThread(t *testing.T, s Store, login, title string) *store.Thread {
	t.Helper()
	th, err := s.CreateThread(context.Background(), login, title)
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	return th
}

// persQueue queues a chat run with a user message on the thread.
func persQueue(t *testing.T, s Store, in store.NewRun) *store.Run {
	t.Helper()
	if in.RunID == "" {
		in.RunID = uuid.NewString()
	}
	if in.UserLogin == "" {
		in.UserLogin = "alice"
	}
	in.Model = "m"
	if in.Message.Role == "" {
		in.Message = store.Message{Role: store.RoleUser, Content: "go"}
	}
	r, _, err := s.CreateRun(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return r
}

func persClaim(t *testing.T, s Store, runID string) {
	t.Helper()
	if _, err := s.ClaimRun(context.Background(), runID, "rt"); err != nil {
		t.Fatalf("ClaimRun: %v", err)
	}
}

// persDone queues, claims and finishes a run with status.
func persDone(t *testing.T, s Store, threadID, text, status string) *store.Run {
	t.Helper()
	r := persQueue(t, s, store.NewRun{ThreadID: threadID, Message: store.Message{Role: store.RoleUser, Content: text}})
	persClaim(t, s, r.ID)
	if err := s.FinishRun(context.Background(), r.ID, "rt", status, "", ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	return r
}

func persEvent(seq int64, typ string) store.Event {
	return store.Event{Seq: seq, Type: typ, Data: json.RawMessage(fmt.Sprintf(`{"type":%q,"seq":%d}`, typ, seq))}
}

// persMsgs lists the visible messages of a thread or fails.
func persMsgs(t *testing.T, s Store, threadID string) []*store.Message {
	t.Helper()
	msgs, err := s.ListMessages(context.Background(), threadID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	return msgs
}

func persGetThread(t *testing.T, s Store, login, id string) *store.Thread {
	t.Helper()
	th, err := s.GetThread(context.Background(), login, id)
	if err != nil {
		t.Fatalf("GetThread(%s): %v", id, err)
	}
	return th
}

func persGetRun(t *testing.T, s Store, id string) *store.Run {
	t.Helper()
	r, err := s.GetRun(context.Background(), id)
	if err != nil {
		t.Fatalf("GetRun(%s): %v", id, err)
	}
	return r
}

func persGetApproval(t *testing.T, s Store, id string) *store.Approval {
	t.Helper()
	a, err := s.GetApproval(context.Background(), id)
	if err != nil {
		t.Fatalf("GetApproval(%s): %v", id, err)
	}
	return a
}

// persPaused makes a completed interactive run of the thread that waits for the approval of call "c1" (write_file).
func persPaused(t *testing.T, s Store, threadID string) (*store.Run, *store.Approval) {
	t.Helper()
	ctx := context.Background()
	run := persQueue(t, s, store.NewRun{ThreadID: threadID, InteractionMode: store.InteractionInteractive,
		Message: store.Message{Role: store.RoleUser, Content: "write it"}})
	persClaim(t, s, run.ID)
	if err := s.AppendMessages(ctx, &store.Message{ThreadID: threadID, RunID: run.ID, Role: store.RoleAssistant,
		ToolCalls: []store.ToolCall{{ID: "c1", Type: "function", Function: store.ToolCallFunction{Name: "write_file", Arguments: `{"path":"a"}`}}}}); err != nil {
		t.Fatal(err)
	}
	a := &store.Approval{ThreadID: threadID, RunID: run.ID, ToolCallID: "c1", ToolName: "write_file", ToolInputPreview: `{"path":"a"}`}
	if err := s.CreateApproval(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(ctx, run.ID, "rt", store.StatusCompleted, "", ""); err != nil {
		t.Fatal(err)
	}
	return run, a
}

// persResume queues the run that decides approval a.
func persResume(t *testing.T, s Store, a *store.Approval, decision, scope string) *store.Run {
	t.Helper()
	return persQueue(t, s, store.NewRun{ThreadID: a.ThreadID, Message: store.Message{},
		Resume: &store.Resume{InterruptID: a.ID, Decision: decision, Scope: scope}})
}

// ---- threads ----

func persThreads(t *testing.T, f Factory) {
	ctx := context.Background()

	t.Run("EveryThreadFieldSurvivesARestart", func(t *testing.T) {
		// Arrange
		b := persBackend(t, f)
		s := b.Store
		pinned, title := true, "Trip to Rome"
		trip := persThread(t, s, "alice", "Trip")
		if _, err := s.UpdateThread(ctx, "alice", trip.ID, store.ThreadPatch{Title: &title, Pinned: &pinned}); err != nil {
			t.Fatal(err)
		}
		plan := []store.PlanItem{{ID: "1", Title: "Book", Status: store.PlanInProgress}, {ID: "2", Title: "Pack", Status: store.PlanPending}}
		question := &store.PendingQuestion{InterruptID: "i1", RunID: uuid.NewString(), ToolCallID: "ask", Question: "Which hotel?",
			Options: []store.QuestionOption{{Label: "A", Description: "near"}, {Label: "B"}}, AllowFreeText: true}
		if err := s.SetThreadPlan(ctx, trip.ID, plan); err != nil {
			t.Fatal(err)
		}
		if err := s.SetPendingQuestion(ctx, trip.ID, question); err != nil {
			t.Fatal(err)
		}
		if err := s.SetThreadInteractionMode(ctx, "alice", trip.ID, store.InteractionInteractive); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkThreadSeen(ctx, "alice", trip.ID); err != nil {
			t.Fatal(err)
		}
		auto := persThread(t, s, "alice", "")
		hi := persQueue(t, s, store.NewRun{ThreadID: auto.ID, Message: store.Message{Role: store.RoleUser, Content: "hi"}, AutoTitle: "hi"})
		persClaim(t, s, hi.ID)
		if err := s.FinishRun(ctx, hi.ID, "rt", store.StatusCompleted, "", ""); err != nil {
			t.Fatal(err)
		}
		if ok, err := s.SetGeneratedTitle(ctx, auto.ID, "A greeting"); !ok || err != nil {
			t.Fatalf("SetGeneratedTitle: %v %v", ok, err)
		}
		bob := persThread(t, s, "bob", "bob's")
		ids := []string{trip.ID, auto.ID}
		before := map[string]string{}
		for _, id := range ids {
			before[id] = persJSON(t, persGetThread(t, s, "alice", id))
		}
		bobBefore := persJSON(t, persGetThread(t, s, "bob", bob.ID))
		listBefore, err := s.ListThreads(ctx, "alice", 10, nil)
		if err != nil {
			t.Fatal(err)
		}

		// Act
		s = b.Reopen(t)

		// Assert
		for _, id := range ids {
			persSame(t, "thread "+id, before[id], persJSON(t, persGetThread(t, s, "alice", id)))
		}
		persSame(t, "bob's thread", bobBefore, persJSON(t, persGetThread(t, s, "bob", bob.ID)))
		got := persGetThread(t, s, "alice", trip.ID)
		if got.Title != title || got.TitleSource != store.TitleUser || got.PinnedAt == nil || got.InteractionMode != store.InteractionInteractive ||
			len(got.Plan) != 2 || got.Plan[1].Title != "Pack" || got.PendingQuestion == nil || got.PendingQuestion.Question != "Which hotel?" ||
			len(got.PendingQuestion.Options) != 2 || !got.PendingQuestion.AllowFreeText || got.LastSeenAt == nil {
			t.Errorf("thread after restart %+v", got)
		}
		if g := persGetThread(t, s, "alice", auto.ID); g.Title != "A greeting" || g.TitleSource != store.TitleGenerated {
			t.Errorf("generated title %q (%s)", g.Title, g.TitleSource)
		}
		if _, err := s.GetThread(ctx, "bob", trip.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another user reads the thread: %v", err)
		}
		listAfter, err := s.ListThreads(ctx, "alice", 10, nil)
		if err != nil {
			t.Fatal(err)
		}
		persSame(t, "thread list (order included)", persJSON(t, listBefore), persJSON(t, listAfter))
	})

	t.Run("ChangesMadeAfterARestartSurviveTheNextOne", func(t *testing.T) {
		// Arrange
		b := persBackend(t, f)
		th := persThread(t, b.Store, "alice", "first")
		s := b.Reopen(t)
		title, pinned := "second", true

		// Act
		_, err := s.UpdateThread(ctx, "alice", th.ID, store.ThreadPatch{Title: &title, Pinned: &pinned})
		s = b.Reopen(t)

		// Assert
		got := persGetThread(t, s, "alice", th.ID)
		if err != nil || got.Title != "second" || got.PinnedAt == nil {
			t.Fatalf("update %v, thread %+v", err, got)
		}
	})
}

// ---- messages ----

func persMessagesSuite(t *testing.T, f Factory) {
	ctx := context.Background()

	t.Run("MessagesKeepEveryFieldAndOrderAcrossARestart", func(t *testing.T) {
		// Arrange
		b := persBackend(t, f)
		s := b.Store
		th := persThread(t, s, "alice", "t")
		run := persQueue(t, s, store.NewRun{ThreadID: th.ID, Message: store.Message{Role: store.RoleUser, Content: "question", AuthorLogin: "alice", AuthorKind: store.AuthorUser,
			Metadata: map[string]any{"attachments": []any{map[string]any{"name": "a.txt"}}}}})
		persClaim(t, s, run.ID)
		call := store.ToolCall{ID: "c1", Type: "function", Function: store.ToolCallFunction{Name: "bash", Arguments: `{"cmd":"ls"}`}}
		assistant := &store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleAssistant, Content: "running", ToolCalls: []store.ToolCall{call},
			Metadata: map[string]any{"model": "m", "n": 3.5}, ReasoningItems: json.RawMessage(`[{"id":"rs_1","type":"reasoning"}]`)}
		tool := &store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleTool, ToolCallID: "c1", Content: "out", Error: "exit 1"}
		final := &store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleAssistant, Content: "multi\nline \"quoted\" ünï", AuthorKind: store.AuthorSystem, OnBehalfOf: "bob"}
		if err := s.AppendMessages(ctx, assistant, tool, final); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishRun(ctx, run.ID, "rt", store.StatusCompleted, "", ""); err != nil {
			t.Fatal(err)
		}
		before := persMsgs(t, s, th.ID)
		beforeJSON := persJSON(t, before)

		// Act
		s = b.Reopen(t)

		// Assert
		after := persMsgs(t, s, th.ID)
		persSame(t, "messages", beforeJSON, persJSON(t, after))
		if len(after) != 4 {
			t.Fatalf("%d messages", len(after))
		}
		for i, want := range []string{"question", "running", "out", "multi\nline \"quoted\" ünï"} {
			if after[i].Content != want || after[i].ID != before[i].ID || after[i].Position != before[i].Position || after[i].ThreadID != th.ID {
				t.Errorf("message %d = %+v, want content %q and the original id and position", i, after[i], want)
			}
			if i > 0 && after[i].Position <= after[i-1].Position {
				t.Errorf("positions not increasing: %d then %d", after[i-1].Position, after[i].Position)
			}
		}
		if after[0].AuthorLogin != "alice" || after[0].AuthorKind != store.AuthorUser || after[0].RunID != run.ID {
			t.Errorf("author of the user message %+v", after[0])
		}
		if len(after[1].ToolCalls) != 1 || after[1].ToolCalls[0] != call || after[1].Metadata["model"] != "m" || after[1].Metadata["n"] != 3.5 ||
			string(after[1].ReasoningItems) != `[{"id":"rs_1","type":"reasoning"}]` {
			t.Errorf("assistant message %+v", after[1])
		}
		if after[2].Role != store.RoleTool || after[2].ToolCallID != "c1" || after[2].Error != "exit 1" {
			t.Errorf("tool message %+v", after[2])
		}
		if after[3].AuthorKind != store.AuthorSystem || after[3].OnBehalfOf != "bob" {
			t.Errorf("authorship of the last message %+v", after[3])
		}
		if md, _ := after[0].Metadata["attachments"].([]any); len(md) != 1 {
			t.Errorf("attachments %v", after[0].Metadata)
		}
	})

	t.Run("PositionsContinueAfterARestartAcrossThreads", func(t *testing.T) {
		// Arrange
		b := persBackend(t, f)
		s := b.Store
		a, c := persThread(t, s, "alice", "a"), persThread(t, s, "alice", "c")
		persDone(t, s, a.ID, "first", store.StatusCompleted)
		persDone(t, s, c.ID, "second", store.StatusCompleted)
		last := persMsgs(t, s, c.ID)[0]
		s = b.Reopen(t)

		// Act
		later := &store.Message{ThreadID: a.ID, Role: store.RoleUser, Content: "third"}
		err := s.AppendMessages(ctx, later)

		// Assert
		msgs := persMsgs(t, s, a.ID)
		if err != nil || len(msgs) != 2 || msgs[1].Position <= last.Position {
			t.Errorf("append %v, positions: new %v, previous maximum %d", err, msgs, last.Position)
		}
	})

	t.Run("AToolVerificationAppendedToAToolMessageSurvivesARestart", func(t *testing.T) {
		// Arrange
		b := persBackend(t, f)
		s := b.Store
		th := persThread(t, s, "alice", "t")
		run := persQueue(t, s, store.NewRun{ThreadID: th.ID})
		persClaim(t, s, run.ID)
		if err := s.AppendMessages(ctx, &store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleTool, ToolCallID: "c1", Content: "done",
			Metadata: map[string]any{"keep": "me"}}); err != nil {
			t.Fatal(err)
		}
		if err := s.AppendToolVerification(ctx, th.ID, "c1", []any{map[string]any{"file": "a.go", "ok": true}}); err != nil {
			t.Fatal(err)
		}
		if err := s.AppendToolVerification(ctx, th.ID, "c1", []any{map[string]any{"file": "b.go", "ok": false}}); err != nil {
			t.Fatal(err)
		}
		before := persJSON(t, persMsgs(t, s, th.ID))

		// Act
		s = b.Reopen(t)

		// Assert
		after := persMsgs(t, s, th.ID)
		persSame(t, "messages", before, persJSON(t, after))
		ver, _ := after[len(after)-1].Metadata["verification"].([]any)
		if len(ver) != 2 || after[len(after)-1].Metadata["keep"] != "me" || len(after) != 2 {
			t.Errorf("metadata %v in %d messages", after[len(after)-1].Metadata, len(after))
		}
	})

	t.Run("HiddenMessagesStayHiddenAfterARestart", func(t *testing.T) {
		// Arrange
		b := persBackend(t, f)
		s := b.Store
		th := persThread(t, s, "alice", "t")
		_, first := harnessTurn(t, s, th.ID, "first", "re: first")
		second, _ := harnessTurn(t, s, th.ID, "second", "re: second")
		if err := s.SupersedeFrom(ctx, "alice", th.ID, second.ID); err != nil {
			t.Fatal(err)
		}
		undo := persQueue(t, s, store.NewRun{ThreadID: th.ID, Message: store.Message{Role: store.RoleUser, Content: "third"}})
		persClaim(t, s, undo.ID)
		if err := s.FinishRun(ctx, undo.ID, "rt", store.StatusCompleted, "", ""); err != nil {
			t.Fatal(err)
		}
		before := persJSON(t, persMsgs(t, s, th.ID))

		// Act
		s = b.Reopen(t)

		// Assert
		after := persMsgs(t, s, th.ID)
		persSame(t, "visible messages", before, persJSON(t, after))
		var contents []string
		for _, m := range after {
			contents = append(contents, m.Content)
		}
		if !slices.Equal(contents, []string{"first", "re: first", "third"}) {
			t.Errorf("visible contents %v (first answer %s)", contents, first.ID)
		}
		if err := s.SupersedeFrom(ctx, "alice", th.ID, second.ID); !errors.Is(err, store.ErrInvalidReplace) {
			t.Errorf("a hidden message cannot be hidden again: %v", err)
		}
	})
}

// ---- runs ----

func persRuns(t *testing.T, f Factory) {
	ctx := context.Background()

	t.Run("RunsEventsAndSkillsSurviveARestart", func(t *testing.T) {
		// Arrange
		b := persBackend(t, f)
		s := b.Store
		th := persThread(t, s, "alice", "t")
		failed := persQueue(t, s, store.NewRun{ThreadID: th.ID, Skills: []string{"docx", "pdf"}, ReasoningEffort: "high",
			InteractionMode: store.InteractionInteractive, Sandbox: store.SandboxOff, Mode: store.ModePlan})
		persClaim(t, s, failed.ID)
		if owned, _, err := s.Heartbeat(ctx, failed.ID, "rt"); err != nil || !owned {
			t.Fatalf("Heartbeat: %v %v", owned, err)
		}
		if err := s.AppendEvents(ctx, failed.ID, []store.Event{persEvent(1, agui.RunStarted), persEvent(2, agui.TextMessageContent)}); err != nil {
			t.Fatal(err)
		}
		if err := s.AppendEvents(ctx, failed.ID, []store.Event{persEvent(3, agui.RunError)}); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishRun(ctx, failed.ID, "rt", store.StatusFailed, "boom", "it broke"); err != nil {
			t.Fatal(err)
		}
		cancelled := persQueue(t, s, store.NewRun{ThreadID: th.ID})
		if ok, err := s.CancelQueuedRun(ctx, cancelled.ID); !ok || err != nil {
			t.Fatalf("CancelQueuedRun: %v %v", ok, err)
		}
		stopped := persQueue(t, s, store.NewRun{ThreadID: th.ID})
		persClaim(t, s, stopped.ID)
		if r, err := s.RequestCancel(ctx, stopped.ID); err != nil || !r.CancelRequested {
			t.Fatalf("RequestCancel: %+v %v", r, err)
		}
		if err := s.FinishRun(ctx, stopped.ID, "rt", store.StatusCancelled, "", ""); err != nil {
			t.Fatal(err)
		}
		stalled := persQueue(t, s, store.NewRun{ThreadID: th.ID})
		persClaim(t, s, stalled.ID)
		b.Advance(t, 10*time.Minute)
		if got, err := s.InterruptStaleRuns(ctx, persStale); err != nil || len(got) != 1 || got[0].ID != stalled.ID {
			t.Fatalf("InterruptStaleRuns: %v %v", got, err)
		}
		runs := []string{failed.ID, cancelled.ID, stopped.ID, stalled.ID}
		before := map[string]string{}
		for _, id := range runs {
			before[id] = persJSON(t, persGetRun(t, s, id))
		}
		eventsBefore, _ := s.ListEvents(ctx, failed.ID, 0, 0)
		lastBefore, lastOK, _ := s.LastEvent(ctx, failed.ID)
		threadBefore := persJSON(t, persGetThread(t, s, "alice", th.ID))

		// Act
		s = b.Reopen(t)

		// Assert
		for _, id := range runs {
			persSame(t, "run "+id, before[id], persJSON(t, persGetRun(t, s, id)))
		}
		f2 := persGetRun(t, s, failed.ID)
		if f2.Status != store.StatusFailed || f2.ErrorCode != "boom" || f2.Error != "it broke" || f2.LastSeq != 3 || f2.Model != "m" ||
			f2.ReasoningEffort != "high" || f2.InteractionMode != store.InteractionInteractive || f2.Sandbox != store.SandboxOff ||
			f2.Mode != store.ModePlan || f2.RuntimeID != "rt" || f2.HeartbeatAt == nil || f2.StartedAt == nil || f2.FinishedAt == nil {
			t.Errorf("failed run %+v", f2)
		}
		if c := persGetRun(t, s, cancelled.ID); c.Status != store.StatusCancelled || !c.CancelRequested || c.FinishedAt == nil {
			t.Errorf("cancelled run %+v", c)
		}
		if c := persGetRun(t, s, stopped.ID); c.Status != store.StatusCancelled || !c.CancelRequested {
			t.Errorf("cancel-requested run %+v", c)
		}
		if i := persGetRun(t, s, stalled.ID); i.Status != store.StatusInterrupted || i.ErrorCode != "interrupted" || i.FinishedAt == nil {
			t.Errorf("interrupted run %+v", i)
		}
		skills, err := s.RunSkills(ctx, failed.ID)
		if err != nil || !slices.Equal(skills, []string{"docx", "pdf"}) {
			t.Errorf("skills %v (%v)", skills, err)
		}
		eventsAfter, _ := s.ListEvents(ctx, failed.ID, 0, 0)
		lastAfter, lastAfterOK, _ := s.LastEvent(ctx, failed.ID)
		persSame(t, "events", persJSON(t, eventsBefore), persJSON(t, eventsAfter))
		if len(eventsAfter) != 3 || eventsAfter[2].Seq != 3 || !lastOK || !lastAfterOK || lastBefore.Seq != lastAfter.Seq || lastAfter.Type != agui.RunError {
			t.Errorf("events %v last %+v", eventsAfter, lastAfter)
		}
		persSame(t, "thread", threadBefore, persJSON(t, persGetThread(t, s, "alice", th.ID)))
		if th2 := persGetThread(t, s, "alice", th.ID); th2.ActiveRun != nil || th2.LastRun == nil || th2.LastRun.ID != stalled.ID {
			t.Errorf("thread run view %+v %+v", th2.ActiveRun, th2.LastRun)
		}
	})

	t.Run("ARestartedStoreAcceptsTheNextRunAndKeepsAppendingEvents", func(t *testing.T) {
		// Arrange
		b := persBackend(t, f)
		th := persThread(t, b.Store, "alice", "t")
		first := persQueue(t, b.Store, store.NewRun{ThreadID: th.ID})
		persClaim(t, b.Store, first.ID)
		if err := b.Store.AppendEvents(ctx, first.ID, []store.Event{persEvent(1, agui.RunStarted)}); err != nil {
			t.Fatal(err)
		}
		if err := b.Store.FinishRun(ctx, first.ID, "rt", store.StatusCompleted, "", ""); err != nil {
			t.Fatal(err)
		}
		s := b.Reopen(t)

		// Act
		next := persQueue(t, s, store.NewRun{ThreadID: th.ID})
		persClaim(t, s, next.ID)
		appendErr := s.AppendEvents(ctx, next.ID, []store.Event{persEvent(1, agui.RunStarted), persEvent(2, agui.RunFinished)})
		finishErr := s.FinishRun(ctx, next.ID, "rt", store.StatusCompleted, "", "")
		s = b.Reopen(t)

		// Assert
		if appendErr != nil || finishErr != nil {
			t.Fatalf("append %v finish %v", appendErr, finishErr)
		}
		if r := persGetRun(t, s, next.ID); r.Status != store.StatusCompleted || r.LastSeq != 2 {
			t.Errorf("second run %+v", r)
		}
		if evs, _ := s.ListEvents(ctx, first.ID, 0, 0); len(evs) != 1 {
			t.Errorf("first run events %v", evs)
		}
		if msgs := persMsgs(t, s, th.ID); len(msgs) != 2 || msgs[0].Position >= msgs[1].Position {
			t.Errorf("messages %+v", msgs)
		}
	})
}

// ---- approvals ----

func persApprovals(t *testing.T, f Factory) {
	ctx := context.Background()

	t.Run("APendingApprovalSurvivesARestartAndIsThenDecided", func(t *testing.T) {
		// Arrange
		b := persBackend(t, f)
		s := b.Store
		th := persThread(t, s, "alice", "t")
		_, a := persPaused(t, s, th.ID)
		approvalBefore := persJSON(t, persGetApproval(t, s, a.ID))
		threadBefore := persJSON(t, persGetThread(t, s, "alice", th.ID))

		// Act
		s = b.Reopen(t)
		approvalMid := persJSON(t, persGetApproval(t, s, a.ID))
		threadMid := persJSON(t, persGetThread(t, s, "alice", th.ID))
		pending := persGetThread(t, s, "alice", th.ID).PendingApproval
		decide := persResume(t, s, a, store.DecisionApprove, store.ApprovalScopeRun)
		persClaim(t, s, decide.ID)
		if err := s.FinishRun(ctx, decide.ID, "rt", store.StatusCompleted, "", ""); err != nil {
			t.Fatal(err)
		}
		resolvedBefore := persJSON(t, persGetApproval(t, s, a.ID))
		decideBefore := persJSON(t, persGetRun(t, s, decide.ID))
		s = b.Reopen(t)

		// Assert
		persSame(t, "pending approval", approvalBefore, approvalMid)
		persSame(t, "thread with a pending approval", threadBefore, threadMid)
		if pending == nil || pending.ID != a.ID || pending.Status != store.ApprovalPending || pending.ToolName != "write_file" {
			t.Errorf("thread pending approval after restart %+v", pending)
		}
		persSame(t, "resolved approval", resolvedBefore, persJSON(t, persGetApproval(t, s, a.ID)))
		persSame(t, "deciding run", decideBefore, persJSON(t, persGetRun(t, s, decide.ID)))
		got := persGetApproval(t, s, a.ID)
		if got.Status != store.ApprovalResolved || got.Decision != store.DecisionApprove || got.Scope != store.ApprovalScopeRun ||
			got.ResumeRunID != decide.ID || got.ResolvedAt == nil {
			t.Errorf("resolved approval %+v", got)
		}
		if !slices.Equal(persGetRun(t, s, decide.ID).ApprovedTools, []string{store.ToolKind("write_file")}) {
			t.Errorf("approved tools %v", persGetRun(t, s, decide.ID).ApprovedTools)
		}
		if p := persGetThread(t, s, "alice", th.ID).PendingApproval; p != nil {
			t.Errorf("a decided approval is not pending: %+v", p)
		}
		if resumed, err := s.ResumedApproval(ctx, decide.ID); err != nil || resumed == nil || resumed.ID != a.ID {
			t.Errorf("ResumedApproval %+v %v", resumed, err)
		}
	})

	t.Run("ACancelledApprovalAndItsToolResultSurviveARestart", func(t *testing.T) {
		// Arrange
		b := persBackend(t, f)
		s := b.Store
		th := persThread(t, s, "alice", "t")
		run, a := persPaused(t, s, th.ID)
		if got, err := s.CancelPendingApproval(ctx, run.ID, store.ApprovalCancelledResult); err != nil || got.ID != a.ID {
			t.Fatalf("CancelPendingApproval: %+v %v", got, err)
		}
		approvalBefore := persJSON(t, persGetApproval(t, s, a.ID))
		messagesBefore := persJSON(t, persMsgs(t, s, th.ID))

		// Act
		s = b.Reopen(t)

		// Assert
		persSame(t, "approval", approvalBefore, persJSON(t, persGetApproval(t, s, a.ID)))
		persSame(t, "messages", messagesBefore, persJSON(t, persMsgs(t, s, th.ID)))
		msgs := persMsgs(t, s, th.ID)
		if last := msgs[len(msgs)-1]; last.Role != store.RoleTool || last.ToolCallID != "c1" || last.Content != store.ApprovalCancelledResult {
			t.Errorf("tool result %+v", last)
		}
		if got := persGetApproval(t, s, a.ID); got.Status != store.ApprovalCancelled || got.ResolvedAt == nil {
			t.Errorf("approval %+v", got)
		}
		if _, err := s.CancelPendingApproval(ctx, run.ID, "again"); !errors.Is(err, store.ErrApprovalNotPending) {
			t.Errorf("second cancel after restart: %v", err)
		}
	})

	t.Run("AnExpiredApprovalSurvivesARestart", func(t *testing.T) {
		// Arrange
		b := persBackend(t, f)
		s := b.Store
		th := persThread(t, s, "alice", "t")
		_, a := persPaused(t, s, th.ID)
		other := persThread(t, s, "alice", "u")
		_, fresh := persPaused(t, s, other.ID)
		b.Advance(t, 2*time.Hour)
		n, err := s.ExpireApprovals(ctx, time.Hour, th.ID)
		if err != nil || n != 1 {
			t.Fatalf("ExpireApprovals: %d %v", n, err)
		}
		approvalBefore := persJSON(t, persGetApproval(t, s, a.ID))
		messagesBefore := persJSON(t, persMsgs(t, s, th.ID))

		// Act
		s = b.Reopen(t)

		// Assert
		persSame(t, "approval", approvalBefore, persJSON(t, persGetApproval(t, s, a.ID)))
		persSame(t, "messages", messagesBefore, persJSON(t, persMsgs(t, s, th.ID)))
		if got := persGetApproval(t, s, a.ID); got.Status != store.ApprovalExpired || got.ResolvedAt == nil {
			t.Errorf("approval %+v", got)
		}
		if msgs := persMsgs(t, s, th.ID); msgs[len(msgs)-1].Content != store.ApprovalExpiredResult {
			t.Errorf("expiry result %+v", msgs[len(msgs)-1])
		}
		if latest, err := s.LatestExpiredApproval(ctx, th.ID); err != nil || latest == nil || latest.ID != a.ID {
			t.Errorf("LatestExpiredApproval %+v %v", latest, err)
		}
		if got := persGetApproval(t, s, fresh.ID); got.Status != store.ApprovalPending {
			t.Errorf("the other thread's approval was expired: %+v", got)
		}
		// ExpireApprovals over every thread expires the remaining one.
		b.Advance(t, 2*time.Hour)
		if n, err := s.ExpireApprovals(ctx, time.Hour, ""); err != nil || n != 1 {
			t.Errorf("ExpireApprovals(all) = %d, %v", n, err)
		}
		s = b.Reopen(t)
		if got := persGetApproval(t, s, fresh.ID); got.Status != store.ApprovalExpired {
			t.Errorf("approval of the other thread after restart %+v", got)
		}
	})

	t.Run("AReopenedApprovalIsPendingAgainAfterARestart", func(t *testing.T) {
		// Arrange
		b := persBackend(t, f)
		s := b.Store
		th := persThread(t, s, "alice", "t")
		_, a := persPaused(t, s, th.ID)
		resume := persResume(t, s, a, store.DecisionApprove, "")
		persClaim(t, s, resume.ID)
		if err := s.FinishRun(ctx, resume.ID, "rt", store.StatusFailed, "boom", ""); err != nil {
			t.Fatal(err)
		}
		reopened, err := s.ReopenApproval(ctx, a.ID)
		if err != nil || reopened.Status != store.ApprovalPending {
			t.Fatalf("ReopenApproval: %+v %v", reopened, err)
		}
		approvalBefore := persJSON(t, persGetApproval(t, s, a.ID))

		// Act
		s = b.Reopen(t)

		// Assert
		persSame(t, "approval", approvalBefore, persJSON(t, persGetApproval(t, s, a.ID)))
		got := persGetApproval(t, s, a.ID)
		if got.Status != store.ApprovalPending || got.Decision != "" || got.ResumeRunID != "" || got.ResolvedAt != nil {
			t.Errorf("approval %+v", got)
		}
		if p := persGetThread(t, s, "alice", th.ID).PendingApproval; p == nil || p.ID != a.ID {
			t.Errorf("thread pending approval %+v", p)
		}
	})
}

// ---- harness data ----

func persHarness(t *testing.T, f Factory) {
	ctx := context.Background()

	t.Run("GoalsFeedbackSummariesAndCompactionsSurviveARestart", func(t *testing.T) {
		// Arrange
		b := persBackend(t, f)
		s := b.Store
		th := persThread(t, s, "alice", "t")
		first := persQueue(t, s, store.NewRun{ThreadID: th.ID, Message: store.Message{Role: store.RoleUser, Content: "one"}})
		persClaim(t, s, first.ID)
		liked := &store.Message{ThreadID: th.ID, RunID: first.ID, Role: store.RoleAssistant, Content: "answer one"}
		disliked := &store.Message{ThreadID: th.ID, RunID: first.ID, Role: store.RoleAssistant, Content: "answer two"}
		if err := s.AppendMessages(ctx, liked, disliked); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishRun(ctx, first.ID, "rt", store.StatusCompleted, "", ""); err != nil {
			t.Fatal(err)
		}
		goal, err := s.PutGoal(ctx, "alice", th.ID, "ship it", 5)
		if err != nil {
			t.Fatal(err)
		}
		verdict := store.GoalVerdict{Met: false, Reason: "tests missing", Blockers: []string{"no tests"}, At: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)}
		if got, err := s.UpdateGoal(ctx, th.ID, store.GoalUpdate{ID: goal.ID, Verdict: verdict, EvalFailed: true}); err != nil || got.EvalFailures != 1 {
			t.Fatalf("UpdateGoal: %+v %v", got, err)
		}
		cleared := persThread(t, s, "alice", "cleared")
		if _, err := s.PutGoal(ctx, "alice", cleared.ID, "drop it", 2); err != nil {
			t.Fatal(err)
		}
		if err := s.ClearGoal(ctx, "alice", cleared.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.SetFeedback(ctx, "alice", liked.ID, store.Feedback{Sentiment: "like", Comment: "nice"}); err != nil {
			t.Fatal(err)
		}
		if err := s.SetFeedback(ctx, "alice", disliked.ID, store.Feedback{Sentiment: "dislike"}); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteFeedback(ctx, "alice", disliked.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.SetThreadSummary(ctx, th.ID, store.ThreadSummary{Text: "summary", ThroughPosition: liked.Position}); err != nil {
			t.Fatal(err)
		}
		if err := s.RecordCompaction(ctx, store.Compaction{ThreadID: th.ID, RunID: first.ID, Trigger: "auto", TokensBefore: 90, TokensAfter: 30}); err != nil {
			t.Fatal(err)
		}
		snap := func(s Store) map[string]string {
			out := map[string]string{}
			g, _ := s.GetGoal(ctx, "alice", th.ID)
			tg, _ := s.ThreadGoal(ctx, th.ID)
			cg, _ := s.GetGoal(ctx, "alice", cleared.ID)
			fb, _ := s.ListFeedback(ctx, th.ID)
			sum, _ := s.GetThreadSummary(ctx, th.ID)
			comps, _ := s.ListCompactions(ctx, th.ID)
			out["goal"], out["threadGoal"], out["clearedGoal"] = persJSON(t, g), persJSON(t, tg), persJSON(t, cg)
			out["feedback"], out["summary"], out["compactions"] = persJSON(t, fb), persJSON(t, sum), persJSON(t, comps)
			out["thread"] = persJSON(t, persGetThread(t, s, "alice", th.ID))
			return out
		}
		before := snap(s)

		// Act
		s = b.Reopen(t)

		// Assert
		after := snap(s)
		for k := range before {
			persSame(t, k, before[k], after[k])
		}
		g, err := s.GetGoal(ctx, "alice", th.ID)
		if err != nil || g.ID != goal.ID || g.Status != store.GoalActive || g.MaxTurns != 5 || g.EvalFailures != 1 || g.LastVerdict == nil ||
			g.LastVerdict.Reason != "tests missing" || !slices.Equal(g.LastVerdict.Blockers, []string{"no tests"}) {
			t.Errorf("goal %+v (%v)", g, err)
		}
		if cg, err := s.GetGoal(ctx, "alice", cleared.ID); err != nil || cg.Status != store.GoalCleared {
			t.Errorf("cleared goal %+v (%v)", cg, err)
		}
		fb, _ := s.ListFeedback(ctx, th.ID)
		if len(fb) != 1 || fb[liked.ID] != (store.Feedback{Sentiment: "like", Comment: "nice"}) {
			t.Errorf("feedback %v", fb)
		}
		if sum, _ := s.GetThreadSummary(ctx, th.ID); sum.Text != "summary" || sum.ThroughPosition != liked.Position {
			t.Errorf("summary %+v", sum)
		}
		comps, _ := s.ListCompactions(ctx, th.ID)
		if len(comps) != 1 || comps[0].Trigger != "auto" || comps[0].TokensBefore != 90 || comps[0].TokensAfter != 30 || comps[0].RunID != first.ID ||
			comps[0].AfterMessageID != disliked.ID || comps[0].ID == "" {
			t.Errorf("compactions %+v", comps)
		}
	})
}

// ---- deletion ----

func persDeletion(t *testing.T, f Factory) {
	ctx := context.Background()

	t.Run("ADeletedThreadStaysDeletedAndTheOthersAreUntouched", func(t *testing.T) {
		// Arrange
		b := persBackend(t, f)
		s := b.Store
		doomed := persThread(t, s, "alice", "doomed")
		run := persDone(t, s, doomed.ID, "bye", store.StatusCompleted)
		kept := persThread(t, s, "alice", "kept")
		keptRun := persDone(t, s, kept.ID, "stay", store.StatusCompleted)
		if _, err := s.DeleteThread(ctx, "alice", doomed.ID); err != nil {
			t.Fatal(err)
		}
		keptBefore := persJSON(t, persGetThread(t, s, "alice", kept.ID))
		keptMsgs := persJSON(t, persMsgs(t, s, kept.ID))
		keptRunBefore := persJSON(t, persGetRun(t, s, keptRun.ID))

		// Act
		s = b.Reopen(t)

		// Assert
		if _, err := s.GetThread(ctx, "alice", doomed.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("GetThread of a deleted thread: %v", err)
		}
		if _, err := s.GetUserRun(ctx, "alice", run.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("GetUserRun of a deleted thread: %v", err)
		}
		if _, err := s.DeleteThread(ctx, "alice", doomed.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("deleting twice: %v", err)
		}
		listed, err := s.ListThreads(ctx, "alice", 10, nil)
		if err != nil || len(listed) != 1 || listed[0].ID != kept.ID {
			t.Errorf("ListThreads = %v (%v)", coreThreadIDs(listed), err)
		}
		persSame(t, "kept thread", keptBefore, persJSON(t, persGetThread(t, s, "alice", kept.ID)))
		persSame(t, "kept messages", keptMsgs, persJSON(t, persMsgs(t, s, kept.ID)))
		persSame(t, "kept run", keptRunBefore, persJSON(t, persGetRun(t, s, keptRun.ID)))
	})
}

// ---- repeated restarts ----

func persRepeated(t *testing.T, f Factory) {
	ctx := context.Background()

	t.Run("RestartingTwiceChangesNothingAndWritesKeepWorking", func(t *testing.T) {
		// Arrange
		b := persBackend(t, f)
		s := b.Store
		th := persThread(t, s, "alice", "t")
		_, a := persPaused(t, s, th.ID)
		done := persDone(t, s, persThread(t, s, "alice", "u").ID, "hello", store.StatusCompleted)
		if err := s.AppendEvents(ctx, done.ID, []store.Event{persEvent(1, agui.RunStarted)}); err != nil {
			t.Fatal(err)
		}
		snap := func(s Store) string {
			ths, _ := s.ListThreads(ctx, "alice", 10, nil)
			evs, _ := s.ListEvents(ctx, done.ID, 0, 0)
			return persJSON(t, []any{ths, persMsgs(t, s, th.ID), persMsgs(t, s, done.ThreadID), persGetApproval(t, s, a.ID), persGetRun(t, s, done.ID), evs})
		}
		before := snap(s)

		// Act
		s = b.Reopen(t)
		first := snap(s)
		s = b.Reopen(t)
		second := snap(s)
		extra := &store.Message{ThreadID: th.ID, Role: store.RoleUser, Content: "later"}
		appendErr := s.AppendMessages(ctx, extra)
		s = b.Reopen(t)

		// Assert
		persSame(t, "state after one restart", before, first)
		persSame(t, "state after two restarts", before, second)
		msgs := persMsgs(t, s, th.ID)
		if appendErr != nil || len(msgs) != 3 || msgs[2].ID != extra.ID || msgs[2].Content != "later" || msgs[2].Position <= msgs[1].Position {
			t.Errorf("append %v, messages %+v", appendErr, msgs)
		}
	})

	t.Run("ConcurrentWritersAreAllDurable", func(t *testing.T) {
		// Arrange
		b := persBackend(t, f)
		s := b.Store
		const writers, perWriter = 6, 25
		threads := make([]*store.Thread, writers)
		runs := make([]*store.Run, writers)
		for i := range threads {
			threads[i] = persThread(t, s, "alice", fmt.Sprint("t", i))
			runs[i] = persQueue(t, s, store.NewRun{ThreadID: threads[i].ID})
			persClaim(t, s, runs[i].ID)
		}

		// Act
		var wg sync.WaitGroup
		for i := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for k := 1; k <= perWriter; k++ {
					m := &store.Message{ThreadID: threads[i].ID, RunID: runs[i].ID, Role: store.RoleAssistant, Content: fmt.Sprint("m", k)}
					if err := s.AppendMessages(ctx, m); err != nil {
						t.Error(err)
					}
					if err := s.AppendEvents(ctx, runs[i].ID, []store.Event{persEvent(int64(k), agui.TextMessageContent)}); err != nil {
						t.Error(err)
					}
				}
				if err := s.FinishRun(ctx, runs[i].ID, "rt", store.StatusCompleted, "", ""); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		s = b.Reopen(t)

		// Assert
		for i := range writers {
			msgs := persMsgs(t, s, threads[i].ID)
			evs, _ := s.ListEvents(ctx, runs[i].ID, 0, perWriter+10)
			if len(msgs) != perWriter+1 || len(evs) != perWriter || persGetRun(t, s, runs[i].ID).LastSeq != perWriter {
				t.Fatalf("writer %d: %d messages, %d events, run %+v", i, len(msgs), len(evs), persGetRun(t, s, runs[i].ID))
			}
			for k := 1; k <= perWriter; k++ { // the order each writer appended in is kept
				if msgs[k].Content != fmt.Sprint("m", k) || evs[k-1].Seq != int64(k) {
					t.Fatalf("writer %d: message %d = %q, event %d", i, k, msgs[k].Content, evs[k-1].Seq)
				}
			}
		}
	})
}
