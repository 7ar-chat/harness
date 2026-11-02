package conformance

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/7ar-chat/harness/core/store"
)

func harnessReplaceRun(threadID, text, replaceID string) store.NewRun {
	in := harnessNewRun(threadID, text)
	in.ReplaceFromMessageID = replaceID
	return in
}

// ---- edit and resend (CreateRun with ReplaceFromMessageID) ----

func harnessReplace(t *testing.T, f Factory) {
	ctx := context.Background()
	t.Run("HidesTheTargetAndEverythingAfterIt", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		harnessTurn(t, s, th.ID, "first", "re: first")
		second, _ := harnessTurn(t, s, th.ID, "second", "re: second")
		harnessTurn(t, s, th.ID, "third", "re: third")

		// Act
		_, msg, err := s.CreateRun(ctx, harnessReplaceRun(th.ID, "second, edited", second.ID))

		// Assert
		if err != nil || msg == nil {
			t.Fatalf("err %v, message %+v", err, msg)
		}
		harnessEqual(t, "messages", harnessContents(t, s, th.ID), []string{"first", "re: first", "second, edited"})
	})
	t.Run("RefusesTargetsThatAreNotVisibleUserMessages", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		other := harnessThread(t, s, "alice", "other")
		_, answer := harnessTurn(t, s, th.ID, "first", "re: first")
		second, _ := harnessTurn(t, s, th.ID, "second", "re: second")
		foreign, _ := harnessTurn(t, s, other.ID, "elsewhere", "re: elsewhere")
		if _, _, err := s.CreateRun(ctx, harnessReplaceRun(th.ID, "second, edited", second.ID)); err != nil {
			t.Fatal(err)
		}
		targets := map[string]string{"assistant message": answer.ID, "unknown message": uuid.NewString(),
			"message of another thread": foreign.ID, "already superseded message": second.ID}

		// Act / Assert
		for name, id := range targets {
			harnessFinishActive(t, s, th.ID)
			_, _, err := s.CreateRun(ctx, harnessReplaceRun(th.ID, "again", id))
			if !errors.Is(err, store.ErrInvalidReplace) {
				t.Errorf("%s: %v, want ErrInvalidReplace", name, err)
			}
		}
		harnessEqual(t, "messages", harnessContents(t, s, th.ID), []string{"first", "re: first", "second, edited"})
	})
	t.Run("DropsOnlyASummaryThatCoversHiddenMessages", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		first, _ := harnessTurn(t, s, th.ID, "first", "re: first")
		second, _ := harnessTurn(t, s, th.ID, "second", "re: second")
		_ = s.SetThreadSummary(ctx, th.ID, store.ThreadSummary{Text: "sum", ThroughPosition: first.Position})

		// Act
		_, _, err := s.CreateRun(ctx, harnessReplaceRun(th.ID, "second, edited", second.ID))
		kept, _ := s.GetThreadSummary(ctx, th.ID)
		harnessFinishActive(t, s, th.ID)
		_, _, err2 := s.CreateRun(ctx, harnessReplaceRun(th.ID, "first, edited", first.ID))
		dropped, _ := s.GetThreadSummary(ctx, th.ID)

		// Assert
		if err != nil || err2 != nil {
			t.Fatalf("errors: %v %v", err, err2)
		}
		if kept != (store.ThreadSummary{Text: "sum", ThroughPosition: first.Position}) {
			t.Errorf("a summary of the kept messages was dropped: %+v", kept)
		}
		if dropped != (store.ThreadSummary{}) {
			t.Errorf("a summary covering hidden messages stays: %+v", dropped)
		}
	})
	t.Run("RecomputesThePlanFromTheVisibleSuccessfulCalls", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		first := harnessPlanTurn(t, s, th.ID, "plan 1", "p1", "Load", false)
		second := harnessPlanTurn(t, s, th.ID, "plan 2", "p2", "Chart", false)
		third := harnessPlanTurn(t, s, th.ID, "plan 3", "p3", "Rejected", true)
		_ = s.SetThreadPlan(ctx, th.ID, []store.PlanItem{{ID: "9", Title: "stale", Status: store.PlanPending}})

		// Act
		_, _, err3 := s.CreateRun(ctx, harnessReplaceRun(th.ID, "edit 3", third.ID))
		afterThird := harnessPlanTitles(t, s, th.ID)
		harnessFinishActive(t, s, th.ID)
		_, _, err2 := s.CreateRun(ctx, harnessReplaceRun(th.ID, "edit 2", second.ID))
		afterSecond := harnessPlanTitles(t, s, th.ID)
		harnessFinishActive(t, s, th.ID)
		_, _, err1 := s.CreateRun(ctx, harnessReplaceRun(th.ID, "edit 1", first.ID))
		afterFirst := harnessPlanTitles(t, s, th.ID)

		// Assert
		if err1 != nil || err2 != nil || err3 != nil {
			t.Fatalf("errors: %v %v %v", err1, err2, err3)
		}
		harnessEqual(t, "plan after hiding the rejected call", afterThird, []string{"Chart"})
		harnessEqual(t, "plan after hiding the second call", afterSecond, []string{"Load"})
		harnessEqual(t, "plan after hiding every call", afterFirst, nil)
	})
	t.Run("CancelsTheQuestionOfAVisibleCallAndHidesTheAnswerOfAHiddenOne", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		run, first, _ := s.CreateRun(ctx, harnessNewRun(th.ID, "ask me"))
		_ = s.AppendMessages(ctx,
			&store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleAssistant, ToolCalls: []store.ToolCall{
				{ID: "ask", Type: "function", Function: store.ToolCallFunction{Name: "ask_user", Arguments: `{"question":"Which?"}`}}}},
			&store.Message{ID: "steer", ThreadID: th.ID, RunID: run.ID, Role: store.RoleUser, Content: "and more", Metadata: map[string]any{"steer": true}})
		_ = s.SetPendingQuestion(ctx, th.ID, &store.PendingQuestion{InterruptID: "i1", RunID: run.ID, ToolCallID: "ask", Question: "Which?"})
		harnessFinish(t, s, run.ID)

		// Act: the call stays visible when only the steer message is edited
		_, _, err := s.CreateRun(ctx, harnessReplaceRun(th.ID, "and different", "steer"))

		// Assert
		msgs, _ := s.ListMessages(ctx, th.ID)
		var results []string
		for _, m := range msgs {
			if m.Role == store.RoleTool && m.ToolCallID == "ask" {
				results = append(results, m.Content)
			}
		}
		harnessEqual(t, "answers of the visible call", results, []string{store.ImplicitCancelResult})
		if th2, _ := s.GetThread(ctx, "alice", th.ID); err != nil || th2.PendingQuestion != nil {
			t.Fatalf("err %v, question %+v", err, th2.PendingQuestion)
		}

		// Arrange: a second question whose call the edit hides
		harnessFinishActive(t, s, th.ID)
		_ = s.SetPendingQuestion(ctx, th.ID, &store.PendingQuestion{InterruptID: "i2", ToolCallID: "ask", Question: "Which?"})

		// Act: edit from the very first message, which hides the call
		_, _, err = s.CreateRun(ctx, harnessReplaceRun(th.ID, "start over", first.ID))

		// Assert
		harnessEqual(t, "messages", harnessContents(t, s, th.ID), []string{"start over"})
		if th3, _ := s.GetThread(ctx, "alice", th.ID); err != nil || th3.PendingQuestion != nil {
			t.Errorf("err %v, question %+v", err, th3.PendingQuestion)
		}
	})
	t.Run("RefusedEditChangesNothing", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		first, _ := harnessTurn(t, s, th.ID, "first", "re: first")
		harnessTurn(t, s, th.ID, "second", "re: second")
		_ = s.SetThreadSummary(ctx, th.ID, store.ThreadSummary{Text: "sum", ThroughPosition: first.Position})
		_ = s.SetPendingQuestion(ctx, th.ID, &store.PendingQuestion{InterruptID: "i1", ToolCallID: "ask", Question: "Which?"})
		in := harnessReplaceRun(th.ID, "edited", first.ID)
		in.Resume = &store.Resume{InterruptID: "wrong", Content: "x"}

		// Act
		_, _, err := s.CreateRun(ctx, in)

		// Assert
		if !errors.Is(err, store.ErrNoPendingQuestion) {
			t.Fatalf("err = %v, want ErrNoPendingQuestion", err)
		}
		harnessEqual(t, "messages", harnessContents(t, s, th.ID), []string{"first", "re: first", "second", "re: second"})
		sum, _ := s.GetThreadSummary(ctx, th.ID)
		got, _ := s.GetThread(ctx, "alice", th.ID)
		if sum.Text != "sum" || got.PendingQuestion == nil {
			t.Errorf("a refused edit changed the thread: summary %+v, question %+v", sum, got.PendingQuestion)
		}
	})
}

// harnessFinishActive finishes the thread's queued run, so that the next run can be created.
func harnessFinishActive(t *testing.T, s Store, threadID string) {
	t.Helper()
	th, err := s.GetThread(context.Background(), "alice", threadID)
	if err != nil {
		t.Fatal(err)
	}
	if th.ActiveRun != nil {
		harnessFinish(t, s, th.ActiveRun.ID)
	}
}

// ---- SupersedeFrom (undo) ----

func harnessSupersede(t *testing.T, f Factory) {
	ctx := context.Background()
	t.Run("HidesTheMessageAndWhatFollows", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		harnessTurn(t, s, th.ID, "first", "re: first")
		second, _ := harnessTurn(t, s, th.ID, "second", "re: second")

		// Act
		err := s.SupersedeFrom(ctx, "alice", th.ID, second.ID)

		// Assert
		if err != nil {
			t.Fatal(err)
		}
		harnessEqual(t, "messages", harnessContents(t, s, th.ID), []string{"first", "re: first"})
		_, _, resend := s.CreateRun(ctx, harnessNewRun(th.ID, "second again"))
		if resend != nil {
			t.Errorf("the thread accepts a new message after an undo: %v", resend)
		}
	})
	t.Run("DropsASummaryThatCoversHiddenMessagesAndKeepsOthers", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		first, _ := harnessTurn(t, s, th.ID, "first", "re: first")
		second, _ := harnessTurn(t, s, th.ID, "second", "re: second")
		_ = s.SetThreadSummary(ctx, th.ID, store.ThreadSummary{Text: "sum", ThroughPosition: first.Position})

		// Act
		err2 := s.SupersedeFrom(ctx, "alice", th.ID, second.ID)
		kept, _ := s.GetThreadSummary(ctx, th.ID)
		err1 := s.SupersedeFrom(ctx, "alice", th.ID, first.ID)
		dropped, _ := s.GetThreadSummary(ctx, th.ID)

		// Assert
		if err1 != nil || err2 != nil {
			t.Fatalf("errors: %v %v", err1, err2)
		}
		if kept.Text != "sum" {
			t.Errorf("a summary of kept messages was dropped: %+v", kept)
		}
		if dropped != (store.ThreadSummary{}) {
			t.Errorf("a summary covering hidden messages stays: %+v", dropped)
		}
	})
	t.Run("ClosesTheQuestionAndRecomputesThePlan", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		harnessPlanTurn(t, s, th.ID, "plan 1", "p1", "Load", false)
		second := harnessPlanTurn(t, s, th.ID, "plan 2", "p2", "Chart", false)
		_ = s.SetPendingQuestion(ctx, th.ID, &store.PendingQuestion{InterruptID: "i1", ToolCallID: "ask", Question: "Which?"})
		_ = s.SetThreadPlan(ctx, th.ID, []store.PlanItem{{ID: "9", Title: "stale", Status: store.PlanPending}})

		// Act
		err := s.SupersedeFrom(ctx, "alice", th.ID, second.ID)

		// Assert
		got, _ := s.GetThread(ctx, "alice", th.ID)
		if err != nil || got.PendingQuestion != nil {
			t.Errorf("err %v, question %+v", err, got.PendingQuestion)
		}
		harnessEqual(t, "plan", harnessPlanTitles(t, s, th.ID), []string{"Load"})
	})
	t.Run("RefusesWhatItCannotUndoAndChangesNothing", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		first, answer := harnessTurn(t, s, th.ID, "first", "re: first")
		other := harnessThread(t, s, "alice", "other")
		foreign, _ := harnessTurn(t, s, other.ID, "elsewhere", "re: elsewhere")
		gone := harnessThread(t, s, "alice", "gone")
		goneUser, _ := harnessTurn(t, s, gone.ID, "q", "a")
		if _, err := s.DeleteThread(ctx, "alice", gone.ID); err != nil {
			t.Fatal(err)
		}
		hidden, _ := harnessTurn(t, s, th.ID, "second", "re: second")
		if err := s.SupersedeFrom(ctx, "alice", th.ID, hidden.ID); err != nil {
			t.Fatal(err)
		}

		// Act
		stranger := s.SupersedeFrom(ctx, "bob", th.ID, first.ID)
		unknownThread := s.SupersedeFrom(ctx, "alice", uuid.NewString(), first.ID)
		badThreadID := s.SupersedeFrom(ctx, "alice", "not-a-uuid", first.ID)
		deleted := s.SupersedeFrom(ctx, "alice", gone.ID, goneUser.ID)
		assistant := s.SupersedeFrom(ctx, "alice", th.ID, answer.ID)
		unknownMsg := s.SupersedeFrom(ctx, "alice", th.ID, uuid.NewString())
		otherThread := s.SupersedeFrom(ctx, "alice", th.ID, foreign.ID)
		already := s.SupersedeFrom(ctx, "alice", th.ID, hidden.ID)
		_, _, runErr := s.CreateRun(ctx, harnessNewRun(th.ID, "third"))
		busy := s.SupersedeFrom(ctx, "alice", th.ID, first.ID)

		// Assert
		for name, err := range map[string]error{"another user": stranger, "unknown thread": unknownThread,
			"malformed thread id": badThreadID, "deleted thread": deleted} {
			if !errors.Is(err, store.ErrNotFound) {
				t.Errorf("%s: %v, want ErrNotFound", name, err)
			}
		}
		for name, err := range map[string]error{"assistant message": assistant, "unknown message": unknownMsg,
			"message of another thread": otherThread, "already hidden message": already} {
			if !errors.Is(err, store.ErrInvalidReplace) {
				t.Errorf("%s: %v, want ErrInvalidReplace", name, err)
			}
		}
		if runErr != nil || !errors.Is(busy, store.ErrRunActive) {
			t.Errorf("run %v, undo during it: %v; want ErrRunActive", runErr, busy)
		}
		harnessEqual(t, "messages", harnessContents(t, s, th.ID), []string{"first", "re: first", "third"})
		harnessEqual(t, "other thread", harnessContents(t, s, other.ID), []string{"elsewhere", "re: elsewhere"})
	})
}

// ---- state survives a restart (persisting backends) ----

func harnessReopen(t *testing.T, f Factory) {
	ctx := context.Background()
	t.Run("EveryHarnessFeatureSurvivesARestart", func(t *testing.T) {
		// Arrange
		b := f(t)
		if b.Reopen == nil {
			t.Skip("the backend does not persist between instances")
		}
		s := b.Store
		th := harnessThread(t, s, "alice", "")
		first, a1 := harnessTurn(t, s, th.ID, "first", "re: first")
		_ = s.RecordCompaction(ctx, store.Compaction{ThreadID: th.ID, Trigger: "auto", TokensBefore: 9, TokensAfter: 3})
		second, a2 := harnessTurn(t, s, th.ID, "second", "re: second")
		_ = s.RecordCompaction(ctx, store.Compaction{ThreadID: th.ID, Trigger: "manual"})
		_ = s.SetThreadSummary(ctx, th.ID, store.ThreadSummary{Text: "sum", ThroughPosition: first.Position})
		if err := s.SupersedeFrom(ctx, "alice", th.ID, second.ID); err != nil {
			t.Fatal(err)
		}
		_ = s.SetThreadPlan(ctx, th.ID, []store.PlanItem{{ID: "1", Title: "Do", Status: store.PlanInProgress}})
		_ = s.SetPendingQuestion(ctx, th.ID, &store.PendingQuestion{InterruptID: "i1", ToolCallID: "ask", Question: "Which?"})
		goal, _ := s.PutGoal(ctx, "alice", th.ID, "ship it", 4)
		_, _ = s.UpdateGoal(ctx, th.ID, store.GoalUpdate{ID: goal.ID, Verdict: store.GoalVerdict{Reason: "almost"}, EvalFailed: true})
		_ = s.SetFeedback(ctx, "alice", a1.ID, store.Feedback{Sentiment: "like", Comment: "good"})
		if ok, err := s.SetGeneratedTitle(ctx, th.ID, "Generated"); !ok || err != nil {
			t.Fatalf("SetGeneratedTitle: %v %v", ok, err)
		}

		// Act
		s = b.Reopen(t)

		// Assert
		harnessEqual(t, "messages", harnessContents(t, s, th.ID), []string{"first", "re: first"})
		got, err := s.GetThread(ctx, "alice", th.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Title != "Generated" || got.TitleSource != store.TitleGenerated {
			t.Errorf("title = %q (%s)", got.Title, got.TitleSource)
		}
		if len(got.Plan) != 1 || got.Plan[0].Title != "Do" || got.PendingQuestion == nil || got.PendingQuestion.InterruptID != "i1" {
			t.Errorf("plan %+v, question %+v", got.Plan, got.PendingQuestion)
		}
		if got.Goal == nil || got.Goal.ID != goal.ID || got.Goal.EvalFailures != 1 || got.Goal.LastVerdict == nil || got.Goal.LastVerdict.Reason != "almost" || got.Goal.MaxTurns != 4 {
			t.Errorf("goal = %+v", got.Goal)
		}
		if sum, _ := s.GetThreadSummary(ctx, th.ID); sum != (store.ThreadSummary{Text: "sum", ThroughPosition: first.Position}) {
			t.Errorf("summary = %+v", sum)
		}
		fb, _ := s.ListFeedback(ctx, th.ID)
		if len(fb) != 1 || fb[a1.ID] != (store.Feedback{Sentiment: "like", Comment: "good"}) {
			t.Errorf("feedback = %+v", fb)
		}
		if err := s.SetFeedback(ctx, "alice", a2.ID, store.Feedback{Sentiment: "like"}); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("rating a message hidden before the restart: %v", err)
		}
		compactions, _ := s.ListCompactions(ctx, th.ID)
		if len(compactions) != 1 || compactions[0].AfterMessageID != a1.ID || compactions[0].Trigger != "auto" || compactions[0].TokensBefore != 9 || compactions[0].TokensAfter != 3 {
			t.Errorf("compactions = %+v", compactions)
		}
	})
}

// ---- sanitizing (Postgres replaces NUL and invalid UTF-8 with U+FFFD; every backend does) ----

func harnessSanitize(t *testing.T, f Factory) {
	ctx := context.Background()
	const dirty, clean = "a\x00b\xffc", "a\uFFFDb\uFFFDc"
	t.Run("sanitizes NUL and invalid UTF-8 like Postgres", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "")
		_, answer := harnessTurn(t, s, th.ID, "q", "a")
		goal, _ := s.PutGoal(ctx, "alice", th.ID, dirty, 2)

		// Act
		errs := []error{
			s.SetThreadSummary(ctx, th.ID, store.ThreadSummary{Text: dirty, ThroughPosition: 1}),
			s.SetThreadPlan(ctx, th.ID, []store.PlanItem{{ID: dirty, Title: dirty, Status: store.PlanPending}}),
			s.SetPendingQuestion(ctx, th.ID, &store.PendingQuestion{InterruptID: "i1", ToolCallID: "ask", Question: dirty,
				Options: []store.QuestionOption{{Label: dirty, Description: dirty}}}),
			s.SetFeedback(ctx, "alice", answer.ID, store.Feedback{Sentiment: "like", Comment: dirty}),
		}
		_, updateErr := s.UpdateGoal(ctx, th.ID, store.GoalUpdate{ID: goal.ID, Verdict: store.GoalVerdict{Reason: dirty}})
		errs = append(errs, updateErr)
		titled := harnessThread(t, s, "alice", "")
		okTitle, titleErr := s.SetGeneratedTitle(ctx, titled.ID, dirty)

		// Assert
		for i, err := range errs {
			if err != nil {
				t.Errorf("call %d: %v", i, err)
			}
		}
		if !okTitle || titleErr != nil {
			t.Errorf("SetGeneratedTitle: %v %v", okTitle, titleErr)
		}
		got, _ := s.GetThread(ctx, "alice", th.ID)
		if sum, _ := s.GetThreadSummary(ctx, th.ID); sum.Text != clean {
			t.Errorf("summary = %q", sum.Text)
		}
		if len(got.Plan) != 1 || got.Plan[0] != (store.PlanItem{ID: clean, Title: clean, Status: store.PlanPending}) {
			t.Errorf("plan = %+v", got.Plan)
		}
		if q := got.PendingQuestion; q == nil || q.Question != clean || len(q.Options) != 1 || q.Options[0] != (store.QuestionOption{Label: clean, Description: clean}) {
			t.Errorf("question = %+v", q)
		}
		if g, _ := s.ThreadGoal(ctx, th.ID); g == nil || g.Text != clean || g.LastVerdict == nil || g.LastVerdict.Reason != clean {
			t.Errorf("goal = %+v", g)
		}
		if fb, _ := s.ListFeedback(ctx, th.ID); fb[answer.ID].Comment != clean {
			t.Errorf("feedback = %+v", fb)
		}
		if th2, _ := s.GetThread(ctx, "alice", titled.ID); th2.Title != clean {
			t.Errorf("title = %q", th2.Title)
		}
	})
	t.Run("sanitizes the answer to a pending question", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		_ = s.SetPendingQuestion(ctx, th.ID, &store.PendingQuestion{InterruptID: "i1", ToolCallID: "ask", Question: "Which?"})
		in := harnessNewRun(th.ID, "")
		in.Message = store.Message{}
		in.Resume = &store.Resume{InterruptID: "i1", Content: dirty}

		// Act
		_, _, err := s.CreateRun(ctx, in)

		// Assert
		if err != nil {
			t.Fatal(err)
		}
		harnessEqual(t, "messages", harnessContents(t, s, th.ID), []string{clean})
	})
}
