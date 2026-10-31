package conformance

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/7ar-chat/harness/core/store"
)

// ---- pending question ----

func harnessQuestion(t *testing.T, f Factory) {
	ctx := context.Background()
	question := func(runID string) *store.PendingQuestion {
		return &store.PendingQuestion{InterruptID: "int-1", RunID: runID, ToolCallID: "call_ask", Question: "Which one?",
			Options: []store.QuestionOption{{Label: "A", Description: "first"}, {Label: "B"}}, AllowFreeText: true}
	}
	t.Run("OpensAndClearsOnTheThread", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")

		// Act
		setErr := s.SetPendingQuestion(ctx, th.ID, question(""))
		open, _ := s.GetThread(ctx, "alice", th.ID)
		clearErr := s.SetPendingQuestion(ctx, th.ID, nil)
		closed, _ := s.GetThread(ctx, "alice", th.ID)

		// Assert
		q := open.PendingQuestion
		if setErr != nil || q == nil || q.InterruptID != "int-1" || q.ToolCallID != "call_ask" || q.Question != "Which one?" || !q.AllowFreeText ||
			len(q.Options) != 2 || q.Options[0] != (store.QuestionOption{Label: "A", Description: "first"}) || q.Options[1].Label != "B" {
			t.Errorf("open question = %+v, %v", q, setErr)
		}
		if clearErr != nil || closed.PendingQuestion != nil {
			t.Errorf("a nil question must close it: %+v, %v", closed.PendingQuestion, clearErr)
		}
	})
	t.Run("ResumeAnswersTheToolCallWithoutAUserMessage", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		_ = s.SetPendingQuestion(ctx, th.ID, question(""))
		in := harnessNewRun(th.ID, "")
		in.Message = store.Message{}
		in.Resume = &store.Resume{InterruptID: "int-1", Content: "The user chose: A"}

		// Act
		run, msg, err := s.CreateRun(ctx, in)

		// Assert
		if err != nil || msg != nil {
			t.Fatalf("err %v message %+v", err, msg)
		}
		msgs, _ := s.ListMessages(ctx, th.ID)
		if len(msgs) != 1 || msgs[0].Role != store.RoleTool || msgs[0].ToolCallID != "call_ask" || msgs[0].Content != "The user chose: A" || msgs[0].RunID != run.ID {
			t.Errorf("messages = %+v", msgs)
		}
		after, _ := s.GetThread(ctx, "alice", th.ID)
		if after.PendingQuestion != nil {
			t.Errorf("the question stays open after its answer: %+v", after.PendingQuestion)
		}
	})
	t.Run("ResumeOfAnotherInterruptIsRefusedAndChangesNothing", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		_ = s.SetPendingQuestion(ctx, th.ID, question(""))
		in := harnessNewRun(th.ID, "")
		in.Message = store.Message{}
		in.Resume = &store.Resume{InterruptID: "other", Content: "x"}

		// Act
		_, _, err := s.CreateRun(ctx, in)

		// Assert
		after, _ := s.GetThread(ctx, "alice", th.ID)
		msgs, _ := s.ListMessages(ctx, th.ID)
		if !errors.Is(err, store.ErrNoPendingQuestion) {
			t.Errorf("err = %v, want ErrNoPendingQuestion", err)
		}
		if after.PendingQuestion == nil || after.ActiveRun != nil || len(msgs) != 0 {
			t.Errorf("a refused resume must leave the thread as it was: question %+v, run %+v, %d messages", after.PendingQuestion, after.ActiveRun, len(msgs))
		}
	})
	t.Run("ResumeWithoutAQuestionIsRefused", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		in := harnessNewRun(th.ID, "")
		in.Message = store.Message{}
		in.Resume = &store.Resume{InterruptID: "int-1", Content: "x"}

		// Act
		_, _, err := s.CreateRun(ctx, in)

		// Assert
		if !errors.Is(err, store.ErrNoPendingQuestion) {
			t.Errorf("err = %v, want ErrNoPendingQuestion", err)
		}
	})
	t.Run("RunWithoutMessageWhileAQuestionIsPendingIsRefused", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		_ = s.SetPendingQuestion(ctx, th.ID, question(""))
		compact := store.NewRun{RunID: uuid.NewString(), ThreadID: th.ID, UserLogin: "alice", Model: "m", Kind: store.KindCompact}

		// Act
		_, _, err := s.CreateRun(ctx, compact)

		// Assert
		after, _ := s.GetThread(ctx, "alice", th.ID)
		if !errors.Is(err, store.ErrQuestionPending) {
			t.Errorf("err = %v, want ErrQuestionPending", err)
		}
		if after.PendingQuestion == nil {
			t.Errorf("the question must stay open")
		}
	})
	t.Run("NewMessageImplicitlyCancelsTheQuestion", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		_ = s.SetPendingQuestion(ctx, th.ID, question(""))

		// Act
		run, msg, err := s.CreateRun(ctx, harnessNewRun(th.ID, "something else"))

		// Assert
		if err != nil || msg == nil {
			t.Fatalf("err %v message %+v", err, msg)
		}
		msgs, _ := s.ListMessages(ctx, th.ID)
		if len(msgs) != 2 || msgs[0].Role != store.RoleTool || msgs[0].ToolCallID != "call_ask" || msgs[0].Content != store.ImplicitCancelResult ||
			msgs[0].RunID != run.ID || msgs[1].ID != msg.ID {
			t.Errorf("messages = %+v", msgs)
		}
		after, _ := s.GetThread(ctx, "alice", th.ID)
		if after.PendingQuestion != nil {
			t.Errorf("the question stays open after a new message")
		}
	})
	t.Run("DecidingAnApprovalLeavesTheOpenQuestionOpen", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		_, a := apprPausedDone(t, s, th.ID)
		_ = s.SetPendingQuestion(ctx, th.ID, question(""))

		// Act
		_, _, err := s.CreateRun(ctx, apprResume(th.ID, a.ID, store.DecisionApprove, store.ApprovalScopeOnce, store.ApprovalResponse{}))

		// Assert
		after, _ := s.GetThread(ctx, "alice", th.ID)
		if err != nil || after.PendingQuestion == nil || after.PendingQuestion.InterruptID != "int-1" {
			t.Errorf("err %v, question %+v", err, after.PendingQuestion)
		}
	})
	t.Run("ResumeKeepsTheRunScopedApprovalsOfThePausedRun", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		_, a := apprPausedDone(t, s, th.ID)
		approved, _, err := s.CreateRun(ctx, apprResume(th.ID, a.ID, store.DecisionApprove, store.ApprovalScopeRun, store.ApprovalResponse{}))
		if err != nil {
			t.Fatal(err)
		}
		_ = s.SetPendingQuestion(ctx, th.ID, question(approved.ID))
		harnessFinish(t, s, approved.ID)
		in := harnessNewRun(th.ID, "")
		in.Message = store.Message{}
		in.Resume = &store.Resume{InterruptID: "int-1", Content: "A"}

		// Act
		answered, _, err := s.CreateRun(ctx, in)

		// Assert
		pausedRun, _ := s.GetRun(ctx, approved.ID)
		got, _ := s.GetRun(ctx, answered.ID)
		if err != nil || len(pausedRun.ApprovedTools) == 0 || !slices.Equal(got.ApprovedTools, pausedRun.ApprovedTools) {
			t.Errorf("err %v, approved tools of the paused run %q and of the answering run %q", err, pausedRun.ApprovedTools, got.ApprovedTools)
		}
	})
}

// ---- goals ----

func harnessGoal(t *testing.T, f Factory) {
	ctx := context.Background()
	t.Run("PutReplacesTheGoalWithAFreshActiveOne", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		first, err := s.PutGoal(ctx, "alice", th.ID, "ship it", 3)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = s.UpdateGoal(ctx, th.ID, store.GoalUpdate{ID: first.ID, Verdict: store.GoalVerdict{Reason: "no"}, EvalFailed: true, Status: store.GoalExhausted})

		// Act
		second, err := s.PutGoal(ctx, "alice", th.ID, "ship it better", 5)

		// Assert
		if err != nil {
			t.Fatal(err)
		}
		if second.ID == "" || second.ID == first.ID || second.ThreadID != th.ID || second.Text != "ship it better" || second.Status != store.GoalActive ||
			second.TurnsUsed != 0 || second.MaxTurns != 5 || second.LastVerdict != nil || second.EvalFailures != 0 || second.CreatedAt.IsZero() {
			t.Errorf("replacement goal = %+v", second)
		}
		got, _ := s.GetGoal(ctx, "alice", th.ID)
		viaThread, _ := s.GetThread(ctx, "alice", th.ID)
		runtime, _ := s.ThreadGoal(ctx, th.ID)
		for name, g := range map[string]*store.Goal{"GetGoal": got, "ThreadGoal": runtime, "Thread.Goal": viaThread.Goal} {
			if g == nil || g.ID != second.ID || g.Text != "ship it better" || g.Status != store.GoalActive || g.MaxTurns != 5 {
				t.Errorf("%s = %+v, want the replacement goal", name, g)
			}
		}
	})
	t.Run("MaxTurnsMustBeBetween1And20", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")

		// Act
		_, zero := s.PutGoal(ctx, "alice", th.ID, "g", 0)
		_, over := s.PutGoal(ctx, "alice", th.ID, "g", 21)
		low, lowErr := s.PutGoal(ctx, "alice", th.ID, "g", 1)
		high, highErr := s.PutGoal(ctx, "alice", th.ID, "g", 20)

		// Assert
		if !store.IsDataError(zero) || !store.IsDataError(over) {
			t.Errorf("0 turns: %v, 21 turns: %v; want data errors", zero, over)
		}
		if lowErr != nil || low.MaxTurns != 1 || highErr != nil || high.MaxTurns != 20 {
			t.Errorf("boundaries: %+v %v, %+v %v", low, lowErr, high, highErr)
		}
	})
	t.Run("NoGoalIsNotFoundForTheUserAndNilForTheRuntime", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")

		// Act
		_, getErr := s.GetGoal(ctx, "alice", th.ID)
		clearErr := s.ClearGoal(ctx, "alice", th.ID)
		runtime, runtimeErr := s.ThreadGoal(ctx, th.ID)
		updated, updateErr := s.UpdateGoal(ctx, th.ID, store.GoalUpdate{ID: uuid.NewString(), Status: store.GoalMet})

		// Assert
		if !errors.Is(getErr, store.ErrNotFound) || !errors.Is(clearErr, store.ErrNotFound) {
			t.Errorf("GetGoal %v, ClearGoal %v; want ErrNotFound", getErr, clearErr)
		}
		if runtime != nil || runtimeErr != nil || updated != nil || updateErr != nil {
			t.Errorf("ThreadGoal %+v %v, UpdateGoal %+v %v; want nil, nil", runtime, runtimeErr, updated, updateErr)
		}
	})
	t.Run("UserCallsAreScopedToTheOwnersLiveThreads", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		deleted := harnessThread(t, s, "alice", "gone")
		if _, err := s.PutGoal(ctx, "alice", th.ID, "g", 2); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PutGoal(ctx, "alice", deleted.ID, "g", 2); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DeleteThread(ctx, "alice", deleted.ID); err != nil {
			t.Fatal(err)
		}

		// Act
		_, putOther := s.PutGoal(ctx, "bob", th.ID, "g", 2)
		_, getOther := s.GetGoal(ctx, "bob", th.ID)
		clearOther := s.ClearGoal(ctx, "bob", th.ID)
		_, putDeleted := s.PutGoal(ctx, "alice", deleted.ID, "g", 2)
		_, getDeleted := s.GetGoal(ctx, "alice", deleted.ID)
		clearDeleted := s.ClearGoal(ctx, "alice", deleted.ID)
		_, putUnknown := s.PutGoal(ctx, "alice", "not-a-uuid", "g", 2)
		_, getUnknown := s.GetGoal(ctx, "alice", uuid.NewString())
		_, getMalformed := s.GetGoal(ctx, "alice", "not-a-uuid")
		clearMalformed := s.ClearGoal(ctx, "alice", "not-a-uuid")

		// Assert
		for name, err := range map[string]error{"PutGoal other": putOther, "GetGoal other": getOther, "ClearGoal other": clearOther,
			"PutGoal deleted": putDeleted, "GetGoal deleted": getDeleted, "ClearGoal deleted": clearDeleted,
			"PutGoal unknown": putUnknown, "GetGoal unknown": getUnknown,
			"GetGoal malformed id": getMalformed, "ClearGoal malformed id": clearMalformed} {
			if !errors.Is(err, store.ErrNotFound) {
				t.Errorf("%s: %v, want ErrNotFound", name, err)
			}
		}
		if g, _ := s.ThreadGoal(ctx, th.ID); g == nil || g.Status != store.GoalActive {
			t.Errorf("a stranger changed alice's goal: %+v", g)
		}
	})
	t.Run("ClearMarksTheGoalCleared", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		put, _ := s.PutGoal(ctx, "alice", th.ID, "g", 2)

		// Act
		err := s.ClearGoal(ctx, "alice", th.ID)

		// Assert
		got, getErr := s.GetGoal(ctx, "alice", th.ID)
		if err != nil || getErr != nil || got.ID != put.ID || got.Status != store.GoalCleared {
			t.Errorf("ClearGoal %v, goal %+v %v", err, got, getErr)
		}
	})
	t.Run("UpdateRecordsVerdictsAndCountsConsecutiveFailures", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		g, _ := s.PutGoal(ctx, "alice", th.ID, "g", 5)
		verdict := store.GoalVerdict{Met: false, Reason: "tests still fail", Blockers: []string{"unit tests", "lint"}}

		// Act
		failed1, err1 := s.UpdateGoal(ctx, th.ID, store.GoalUpdate{ID: g.ID, Verdict: verdict, EvalFailed: true})
		failed2, err2 := s.UpdateGoal(ctx, th.ID, store.GoalUpdate{ID: g.ID, Verdict: verdict, EvalFailed: true})
		ok, err3 := s.UpdateGoal(ctx, th.ID, store.GoalUpdate{ID: g.ID, Verdict: store.GoalVerdict{Met: true, Reason: "all green"}})
		met, err4 := s.UpdateGoal(ctx, th.ID, store.GoalUpdate{ID: g.ID, Verdict: store.GoalVerdict{Met: true, Reason: "done"}, Status: store.GoalMet})

		// Assert
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
			t.Fatalf("errors: %v %v %v %v", err1, err2, err3, err4)
		}
		v := failed1.LastVerdict
		if failed1.Status != store.GoalActive || failed1.EvalFailures != 1 || v == nil || v.Met || v.Reason != "tests still fail" ||
			len(v.Blockers) != 2 || v.Blockers[0] != "unit tests" || v.Blockers[1] != "lint" {
			t.Errorf("after the first failure: %+v (verdict %+v)", failed1, v)
		}
		if failed2.EvalFailures != 2 {
			t.Errorf("consecutive failures = %d, want 2", failed2.EvalFailures)
		}
		if ok.EvalFailures != 0 || ok.Status != store.GoalActive || ok.LastVerdict == nil || !ok.LastVerdict.Met || ok.LastVerdict.Reason != "all green" {
			t.Errorf("a successful evaluation resets the count and keeps the status: %+v", ok)
		}
		if met.Status != store.GoalMet || met.LastVerdict.Reason != "done" {
			t.Errorf("met goal = %+v", met)
		}
		stored, _ := s.ThreadGoal(ctx, th.ID)
		if stored.Status != store.GoalMet || stored.LastVerdict == nil || stored.LastVerdict.Reason != "done" {
			t.Errorf("stored goal = %+v", stored)
		}
	})
	t.Run("UpdateOfAReplacedOrFinishedGoalIsIgnored", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		old, _ := s.PutGoal(ctx, "alice", th.ID, "old", 2)
		current, _ := s.PutGoal(ctx, "alice", th.ID, "current", 2)
		other := harnessThread(t, s, "alice", "other")
		finished, _ := s.PutGoal(ctx, "alice", other.ID, "finished", 2)
		_ = s.ClearGoal(ctx, "alice", other.ID)

		// Act
		stale, staleErr := s.UpdateGoal(ctx, th.ID, store.GoalUpdate{ID: old.ID, Verdict: store.GoalVerdict{Reason: "late"}, Status: store.GoalMet})
		late, lateErr := s.UpdateGoal(ctx, other.ID, store.GoalUpdate{ID: finished.ID, Verdict: store.GoalVerdict{Reason: "late"}, Status: store.GoalMet})

		// Assert
		if staleErr != nil || stale == nil || stale.ID != current.ID || stale.Status != store.GoalActive || stale.LastVerdict != nil {
			t.Errorf("stale update returned %+v, %v; want the untouched current goal", stale, staleErr)
		}
		if lateErr != nil || late == nil || late.Status != store.GoalCleared || late.LastVerdict != nil {
			t.Errorf("update of a cleared goal returned %+v, %v; want it untouched", late, lateErr)
		}
	})
	t.Run("ReturnedGoalsAreCopies", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		g, _ := s.PutGoal(ctx, "alice", th.ID, "g", 2)
		_, _ = s.UpdateGoal(ctx, th.ID, store.GoalUpdate{ID: g.ID, Verdict: store.GoalVerdict{Reason: "r"}})
		got, _ := s.ThreadGoal(ctx, th.ID)

		// Act
		got.Text, got.LastVerdict.Reason = "mutated", "mutated"

		// Assert
		again, _ := s.ThreadGoal(ctx, th.ID)
		if again.Text != "g" || again.LastVerdict.Reason != "r" {
			t.Errorf("a caller changed the stored goal: %+v", again)
		}
	})
}

func harnessGoalRuns(t *testing.T, f Factory) {
	ctx := context.Background()
	goalRun := func(threadID, goalID, text string) store.NewRun {
		in := harnessNewRun(threadID, text)
		in.Kind, in.GoalID = store.KindGoal, goalID
		return in
	}
	t.Run("EachGoalRunClaimsATurnUntilTheGoalIsOutOfTurns", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		g, _ := s.PutGoal(ctx, "alice", th.ID, "g", 2)

		// Act
		var errs []error
		for range 3 {
			run, _, err := s.CreateRun(ctx, goalRun(th.ID, g.ID, "go"))
			errs = append(errs, err)
			if err == nil {
				if run.Kind != store.KindGoal {
					t.Errorf("run kind = %q", run.Kind)
				}
				harnessFinish(t, s, run.ID)
			}
		}

		// Assert
		if errs[0] != nil || errs[1] != nil || !errors.Is(errs[2], store.ErrGoalInactive) {
			t.Errorf("errors = %v; want nil, nil, ErrGoalInactive", errs)
		}
		got, _ := s.ThreadGoal(ctx, th.ID)
		msgs, _ := s.ListMessages(ctx, th.ID)
		if got.TurnsUsed != 2 || len(msgs) != 2 {
			t.Errorf("turns used %d, messages %d; want 2, 2 (the refused run stores nothing)", got.TurnsUsed, len(msgs))
		}
	})
	t.Run("InactiveReplacedOrMissingGoalsRefuseTheRun", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		noGoal := harnessThread(t, s, "alice", "none")
		th := harnessThread(t, s, "alice", "t")
		old, _ := s.PutGoal(ctx, "alice", th.ID, "old", 3)
		current, _ := s.PutGoal(ctx, "alice", th.ID, "current", 3)
		cleared := harnessThread(t, s, "alice", "cleared")
		clearedGoal, _ := s.PutGoal(ctx, "alice", cleared.ID, "g", 3)
		_ = s.ClearGoal(ctx, "alice", cleared.ID)
		met := harnessThread(t, s, "alice", "met")
		metGoal, _ := s.PutGoal(ctx, "alice", met.ID, "g", 3)
		_, _ = s.UpdateGoal(ctx, met.ID, store.GoalUpdate{ID: metGoal.ID, Status: store.GoalMet})

		// Act
		_, _, none := s.CreateRun(ctx, goalRun(noGoal.ID, uuid.NewString(), "go"))
		_, _, replaced := s.CreateRun(ctx, goalRun(th.ID, old.ID, "go"))
		_, _, clearedErr := s.CreateRun(ctx, goalRun(cleared.ID, clearedGoal.ID, "go"))
		_, _, metErr := s.CreateRun(ctx, goalRun(met.ID, metGoal.ID, "go"))
		_, _, valid := s.CreateRun(ctx, goalRun(th.ID, current.ID, "go"))

		// Assert
		for name, err := range map[string]error{"no goal": none, "replaced goal": replaced, "cleared goal": clearedErr, "met goal": metErr} {
			if !errors.Is(err, store.ErrGoalInactive) {
				t.Errorf("%s: %v, want ErrGoalInactive", name, err)
			}
		}
		if valid != nil {
			t.Errorf("the current goal is claimable: %v", valid)
		}
	})
	t.Run("ARefusedGoalRunLeavesTheEditItCarriedUnapplied", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		first, _ := harnessTurn(t, s, th.ID, "first", "re: first")
		in := goalRun(th.ID, uuid.NewString(), "edited")
		in.ReplaceFromMessageID = first.ID

		// Act
		_, _, err := s.CreateRun(ctx, in)

		// Assert
		if !errors.Is(err, store.ErrGoalInactive) {
			t.Fatalf("err = %v, want ErrGoalInactive", err)
		}
		harnessEqual(t, "messages", harnessContents(t, s, th.ID), []string{"first", "re: first"})
		after, _ := s.GetThread(ctx, "alice", th.ID)
		if after.ActiveRun != nil {
			t.Errorf("a refused run is active: %+v", after.ActiveRun)
		}
	})
}

// ---- feedback ----

func harnessFeedback(t *testing.T, f Factory) {
	ctx := context.Background()
	t.Run("RatesOwnVisibleAssistantMessages", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		_, a1 := harnessTurn(t, s, th.ID, "q1", "answer 1")
		_, a2 := harnessTurn(t, s, th.ID, "q2", "answer 2")

		// Act
		err1 := s.SetFeedback(ctx, "alice", a1.ID, store.Feedback{Sentiment: "dislike", Comment: "wrong"})
		err2 := s.SetFeedback(ctx, "alice", a2.ID, store.Feedback{Sentiment: "like"})
		err3 := s.SetFeedback(ctx, "alice", a1.ID, store.Feedback{Sentiment: "like", Comment: "better now"})

		// Assert
		if err1 != nil || err2 != nil || err3 != nil {
			t.Fatalf("errors: %v %v %v", err1, err2, err3)
		}
		got, err := s.ListFeedback(ctx, th.ID)
		if err != nil || len(got) != 2 || got[a1.ID] != (store.Feedback{Sentiment: "like", Comment: "better now"}) || got[a2.ID] != (store.Feedback{Sentiment: "like"}) {
			t.Errorf("feedback = %+v, %v", got, err)
		}
	})
	t.Run("DeleteRemovesOnlyThatRating", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		_, a1 := harnessTurn(t, s, th.ID, "q1", "answer 1")
		_, a2 := harnessTurn(t, s, th.ID, "q2", "answer 2")
		_ = s.SetFeedback(ctx, "alice", a1.ID, store.Feedback{Sentiment: "like"})
		_ = s.SetFeedback(ctx, "alice", a2.ID, store.Feedback{Sentiment: "dislike"})

		// Act
		err := s.DeleteFeedback(ctx, "alice", a1.ID)

		// Assert
		got, _ := s.ListFeedback(ctx, th.ID)
		if err != nil || len(got) != 1 || got[a2.ID].Sentiment != "dislike" {
			t.Errorf("DeleteFeedback %v, remaining %+v", err, got)
		}
	})
	t.Run("OtherMessagesUsersAndSentimentsAreRefused", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		user, a1 := harnessTurn(t, s, th.ID, "q1", "answer 1")
		hiddenUser, hidden := harnessTurn(t, s, th.ID, "q2", "answer 2")
		if err := s.(store.HistoryStore).SupersedeFrom(ctx, "alice", th.ID, hiddenUser.ID); err != nil {
			t.Fatal(err)
		}
		gone := harnessThread(t, s, "alice", "gone")
		_, goneAnswer := harnessTurn(t, s, gone.ID, "q", "a")
		if _, err := s.DeleteThread(ctx, "alice", gone.ID); err != nil {
			t.Fatal(err)
		}
		like := store.Feedback{Sentiment: "like"}

		// Act
		set := map[string]error{
			"user message":      s.SetFeedback(ctx, "alice", user.ID, like),
			"other user":        s.SetFeedback(ctx, "bob", a1.ID, like),
			"unknown message":   s.SetFeedback(ctx, "alice", uuid.NewString(), like),
			"superseded":        s.SetFeedback(ctx, "alice", hidden.ID, like),
			"deleted thread":    s.SetFeedback(ctx, "alice", goneAnswer.ID, like),
			"delete user msg":   s.DeleteFeedback(ctx, "alice", user.ID),
			"delete other user": s.DeleteFeedback(ctx, "bob", a1.ID),
			"delete unknown":    s.DeleteFeedback(ctx, "alice", uuid.NewString()),
			"delete superseded": s.DeleteFeedback(ctx, "alice", hidden.ID),
		}
		badSentiment := s.SetFeedback(ctx, "alice", a1.ID, store.Feedback{Sentiment: "meh"})

		// Assert
		for name, err := range set {
			if !errors.Is(err, store.ErrNotFound) {
				t.Errorf("%s: %v, want ErrNotFound", name, err)
			}
		}
		if !store.IsDataError(badSentiment) {
			t.Errorf("sentiment meh: %v, want a data error", badSentiment)
		}
		if got, _ := s.ListFeedback(ctx, th.ID); len(got) != 0 {
			t.Errorf("refused ratings were stored: %+v", got)
		}
	})
	t.Run("ThreadWithoutRatingsListsNone", func(t *testing.T) {
		// Arrange
		s := f(t).Store

		// Act
		got, err := s.ListFeedback(ctx, uuid.NewString())

		// Assert
		if err != nil || len(got) != 0 {
			t.Errorf("feedback = %+v, %v", got, err)
		}
	})
}

// ---- compactions ----

func harnessCompaction(t *testing.T, f Factory) {
	ctx := context.Background()
	t.Run("RecordsAfterTheLastVisibleMessageOldestFirst", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "")
		run, user, _ := s.CreateRun(ctx, harnessNewRun(th.ID, "one"))
		answer := &store.Message{ID: uuid.NewString(), ThreadID: th.ID, RunID: run.ID, Role: store.RoleAssistant, Content: "ok"}
		_ = s.AppendMessages(ctx, answer)
		first := s.RecordCompaction(ctx, store.Compaction{ThreadID: th.ID, RunID: run.ID, Trigger: "auto", TokensBefore: 100, TokensAfter: 20})
		harnessFinish(t, s, run.ID)
		_, second, _ := s.CreateRun(ctx, harnessNewRun(th.ID, "two"))

		// Act
		secondErr := s.RecordCompaction(ctx, store.Compaction{ThreadID: th.ID, Trigger: "manual", TokensBefore: 200, TokensAfter: 30})
		got, err := s.ListCompactions(ctx, th.ID)

		// Assert
		if first != nil || secondErr != nil || err != nil || len(got) != 2 {
			t.Fatalf("errors %v %v %v, compactions %+v", first, secondErr, err, got)
		}
		a, b := got[0], got[1]
		if a.ID == "" || a.ThreadID != th.ID || a.RunID != run.ID || a.AfterMessageID != answer.ID || a.Trigger != "auto" || a.TokensBefore != 100 || a.TokensAfter != 20 || a.CreatedAt.IsZero() {
			t.Errorf("first = %+v (user %s)", a, user.ID)
		}
		if b.ID == "" || b.ID == a.ID || b.RunID != "" || b.AfterMessageID != second.ID || b.Trigger != "manual" || b.TokensBefore != 200 || b.TokensAfter != 30 {
			t.Errorf("second = %+v", b)
		}
	})
	t.Run("SkipsHiddenMessagesAndOtherThreads", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		other := harnessThread(t, s, "alice", "other")
		_, a1 := harnessTurn(t, s, th.ID, "one", "answer 1")
		u2, _ := harnessTurn(t, s, th.ID, "two", "answer 2")
		harnessTurn(t, s, other.ID, "elsewhere", "later in time")
		if err := s.(store.HistoryStore).SupersedeFrom(ctx, "alice", th.ID, u2.ID); err != nil {
			t.Fatal(err)
		}

		// Act
		err := s.RecordCompaction(ctx, store.Compaction{ThreadID: th.ID, Trigger: "auto"})

		// Assert
		got, _ := s.ListCompactions(ctx, th.ID)
		elsewhere, _ := s.ListCompactions(ctx, other.ID)
		if err != nil || len(got) != 1 || got[0].AfterMessageID != a1.ID || len(elsewhere) != 0 {
			t.Errorf("err %v, compactions %+v, other thread %+v", err, got, elsewhere)
		}
	})
	t.Run("ThreadWithoutMessagesRecordsNothing", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")

		// Act
		err := s.RecordCompaction(ctx, store.Compaction{ThreadID: th.ID, Trigger: "auto"})
		unknown := s.RecordCompaction(ctx, store.Compaction{ThreadID: "not-a-uuid", Trigger: "auto"})

		// Assert
		got, listErr := s.ListCompactions(ctx, th.ID)
		none, noneErr := s.ListCompactions(ctx, "not-a-uuid")
		if err != nil || unknown != nil || listErr != nil || noneErr != nil || len(got) != 0 || len(none) != 0 {
			t.Errorf("record %v %v, list %+v %v, %+v %v", err, unknown, got, listErr, none, noneErr)
		}
	})
	t.Run("TriggerMustBeAutoOrManual", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		harnessTurn(t, s, th.ID, "q", "a")

		// Act
		err := s.RecordCompaction(ctx, store.Compaction{ThreadID: th.ID, Trigger: "sometimes"})

		// Assert
		got, _ := s.ListCompactions(ctx, th.ID)
		if !store.IsDataError(err) || len(got) != 0 {
			t.Errorf("err %v, compactions %+v", err, got)
		}
	})
	t.Run("AreNoLongerListedOnceTheirMessageIsHidden", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		_, a1 := harnessTurn(t, s, th.ID, "one", "answer 1")
		_ = s.RecordCompaction(ctx, store.Compaction{ThreadID: th.ID, Trigger: "auto"})
		u2, _ := harnessTurn(t, s, th.ID, "two", "answer 2")
		_ = s.RecordCompaction(ctx, store.Compaction{ThreadID: th.ID, Trigger: "manual"})
		before, _ := s.ListCompactions(ctx, th.ID)

		// Act
		err := s.(store.HistoryStore).SupersedeFrom(ctx, "alice", th.ID, u2.ID)

		// Assert
		after, _ := s.ListCompactions(ctx, th.ID)
		if err != nil || len(before) != 2 || len(after) != 1 || after[0].AfterMessageID != a1.ID || after[0].Trigger != "auto" {
			t.Errorf("err %v, before %+v, after %+v", err, before, after)
		}
	})
}
