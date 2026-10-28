package pgstore_test

import (
	"context"
	"errors"
	"github.com/GhaziBenDahmane/harness/store/pgstore"
	"testing"

	"github.com/google/uuid"

	"github.com/GhaziBenDahmane/harness/store"
)

func finish(t *testing.T, s *pgstore.PG, runID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.ClaimRun(ctx, runID, "rt"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(ctx, runID, "rt", store.StatusCompleted, "", ""); err != nil {
		t.Fatal(err)
	}
}

func TestPGReplaceFromMessageSupersedesAndInvalidatesTheSummary(t *testing.T) {
	// Edit & resend hides the target and later messages, drops a summary covering them and rejects non-user targets.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "")
	r1, m1, _ := s.CreateRun(ctx, newRun(th.ID, "first"))
	_ = s.AppendMessages(ctx, &store.Message{ID: "a1", ThreadID: th.ID, RunID: r1.ID, Role: store.RoleAssistant, Content: "answer"})
	finish(t, s, r1.ID)
	_ = s.SetThreadSummary(ctx, th.ID, store.ThreadSummary{Text: "sum", ThroughPosition: m1.Position})
	in := newRun(th.ID, "edited")
	in.ReplaceFromMessageID = "a1"
	_, _, badErr := s.CreateRun(ctx, in)

	// Act
	in.ReplaceFromMessageID = m1.ID
	_, msg, err := s.CreateRun(ctx, in)

	// Assert
	msgs, _ := s.ListMessages(ctx, th.ID)
	sum, _ := s.GetThreadSummary(ctx, th.ID)
	if !errors.Is(badErr, store.ErrInvalidReplace) || err != nil || len(msgs) != 1 || msgs[0].ID != msg.ID || sum.Text != "" {
		t.Fatalf("bad %v err %v msgs %+v sum %+v", badErr, err, msgs, sum)
	}
}

func TestPGPendingQuestionResumeAndImplicitCancel(t *testing.T) {
	// A resume stores the answer for the pending tool call; a plain message stores the cancel result; runs without message are refused.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "t")
	q := &store.PendingQuestion{InterruptID: "i1", ToolCallID: "call_ask", Question: "Which?", Options: []store.QuestionOption{{Label: "A"}}, AllowFreeText: true}
	_ = s.SetPendingQuestion(ctx, th.ID, q)
	got, _ := s.GetThread(ctx, "alice", th.ID)
	compact := store.NewRun{RunID: uuid.NewString(), ThreadID: th.ID, UserLogin: "alice", Model: "m", Kind: store.KindCompact}
	_, _, pendingErr := s.CreateRun(ctx, compact)
	wrong := newRun(th.ID, "")
	wrong.Message = store.Message{}
	wrong.Resume = &store.Resume{InterruptID: "other", Content: "x"}
	_, _, wrongErr := s.CreateRun(ctx, wrong)

	// Act
	resume := newRun(th.ID, "")
	resume.Message = store.Message{}
	resume.Resume = &store.Resume{InterruptID: "i1", Content: "The user chose: A"}
	run, msg, err := s.CreateRun(ctx, resume)

	// Assert
	msgs, _ := s.ListMessages(ctx, th.ID)
	after, _ := s.GetThread(ctx, "alice", th.ID)
	if got.PendingQuestion == nil || got.PendingQuestion.Options[0].Label != "A" || !errors.Is(pendingErr, store.ErrQuestionPending) ||
		!errors.Is(wrongErr, store.ErrNoPendingQuestion) || err != nil || msg != nil || len(msgs) != 1 ||
		msgs[0].ToolCallID != "call_ask" || msgs[0].RunID != run.ID || msgs[0].Content != "The user chose: A" || after.PendingQuestion != nil {
		t.Fatalf("pending %v wrong %v err %v msgs %+v after %+v", pendingErr, wrongErr, err, msgs, after.PendingQuestion)
	}
	finish(t, s, run.ID)
	_ = s.SetPendingQuestion(ctx, th.ID, q)
	plain, _, err := s.CreateRun(ctx, newRun(th.ID, "something else"))
	msgs, _ = s.ListMessages(ctx, th.ID)
	if err != nil || msgs[1].Content != store.ImplicitCancelResult || msgs[1].RunID != plain.ID || msgs[2].Content != "something else" {
		t.Fatalf("err %v msgs %+v", err, msgs)
	}
}

func TestPGGoalRunsClaimTurns(t *testing.T) {
	// Goal runs claim turns atomically; out of turns or replaced goals are ErrGoalInactive; verdicts only apply to the same active goal.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "t")
	g, err := s.PutGoal(ctx, "alice", th.ID, "ship it", 1)
	if err != nil {
		t.Fatal(err)
	}
	in := newRun(th.ID, "go")
	in.Kind, in.GoalID = store.KindGoal, g.ID

	// Act
	run, _, err := s.CreateRun(ctx, in)
	finish(t, s, run.ID)
	in.RunID = uuid.NewString()
	in.Message.ID = ""
	_, _, outErr := s.CreateRun(ctx, in)

	// Assert
	cur, _ := s.ThreadGoal(ctx, th.ID)
	if err != nil || run.Kind != store.KindGoal || !errors.Is(outErr, store.ErrGoalInactive) || cur.TurnsUsed != 1 {
		t.Fatalf("err %v out %v goal %+v", err, outErr, cur)
	}
	updated, _ := s.UpdateGoal(ctx, th.ID, store.GoalUpdate{ID: g.ID, Verdict: store.GoalVerdict{Reason: "done", Met: true}, Status: store.GoalMet})
	stale, _ := s.UpdateGoal(ctx, th.ID, store.GoalUpdate{ID: "00000000-0000-0000-0000-000000000000", Status: store.GoalExhausted})
	viaThread, _ := s.GetThread(ctx, "alice", th.ID)
	if updated.Status != store.GoalMet || updated.LastVerdict.Reason != "done" || stale.Status != store.GoalMet || viaThread.Goal == nil || viaThread.Goal.Status != store.GoalMet {
		t.Fatalf("updated %+v stale %+v thread goal %+v", updated, stale, viaThread.Goal)
	}
	if err := s.ClearGoal(ctx, "bob", th.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("bob cleared alice's goal: %v", err)
	}
}

func TestPGSearchThreadsEscapesAndSkipsHiddenMessages(t *testing.T) {
	// Search is a literal, case-insensitive match on titles and visible user/assistant messages.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	a, _ := s.CreateThread(ctx, "alice", "Growth 100% plan")
	b, _ := s.CreateThread(ctx, "alice", "other")
	_, _, _ = s.CreateRun(ctx, newRun(b.ID, "the REPORT"))
	c, _ := s.CreateThread(ctx, "alice", "100 percent")

	// Act
	percent, err := s.SearchThreads(ctx, "alice", "100%", 10, nil)
	report, _ := s.SearchThreads(ctx, "alice", "report", 10, nil)
	underscore, _ := s.SearchThreads(ctx, "alice", "_", 10, nil)

	// Assert
	if err != nil || len(percent) != 1 || percent[0].ID != a.ID || len(report) != 1 || report[0].ID != b.ID || len(underscore) != 0 {
		t.Fatalf("err %v percent %v report %v underscore %v (c %s)", err, percent, report, underscore, c.ID)
	}
}

func TestPGTitleSourcesAndGeneratedTitles(t *testing.T) {
	// Untitled threads are auto-titled, titled or patched ones are user titles; generated titles only replace auto ones.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	auto, _ := s.CreateThread(ctx, "alice", "")
	user, _ := s.CreateThread(ctx, "alice", "Mine")

	// Act
	okAuto, err := s.SetGeneratedTitle(ctx, auto.ID, "Generated")
	okUser, _ := s.SetGeneratedTitle(ctx, user.ID, "Generated")

	// Assert
	a, _ := s.GetThread(ctx, "alice", auto.ID)
	u, _ := s.GetThread(ctx, "alice", user.ID)
	if err != nil || !okAuto || okUser || a.Title != "Generated" || a.TitleSource != store.TitleGenerated || u.Title != "Mine" || u.TitleSource != store.TitleUser {
		t.Fatalf("auto %+v user %+v", a, u)
	}
}

func TestPGPlanAndFeedback(t *testing.T) {
	// The plan round-trips on the thread, feedback only rates own visible assistant messages (memory: pg_memory_test.go).
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "t")
	run, user, _ := s.CreateRun(ctx, newRun(th.ID, "q"))
	_ = s.AppendMessages(ctx, &store.Message{ID: "a1", ThreadID: th.ID, RunID: run.ID, Role: store.RoleAssistant, Content: "answer"})
	_ = s.SetThreadPlan(ctx, th.ID, []store.PlanItem{{ID: "1", Title: "Do", Status: store.PlanInProgress}})

	// Act
	fbErr := s.SetFeedback(ctx, "alice", "a1", store.Feedback{Sentiment: "dislike", Comment: "wrong"})
	userFbErr := s.SetFeedback(ctx, "alice", user.ID, store.Feedback{Sentiment: "like"})
	foreignFbErr := s.SetFeedback(ctx, "bob", "a1", store.Feedback{Sentiment: "like"})

	// Assert
	got, _ := s.GetThread(ctx, "alice", th.ID)
	if len(got.Plan) != 1 || got.Plan[0].Status != store.PlanInProgress {
		t.Fatalf("plan %+v", got.Plan)
	}
	fb, _ := s.ListFeedback(ctx, th.ID)
	if fbErr != nil || !errors.Is(userFbErr, store.ErrNotFound) || !errors.Is(foreignFbErr, store.ErrNotFound) || fb["a1"].Comment != "wrong" {
		t.Fatalf("feedback %v %v %v %+v", fbErr, userFbErr, foreignFbErr, fb)
	}
	if err := s.DeleteFeedback(ctx, "alice", "a1"); err != nil {
		t.Fatal(err)
	}
	if fb, _ := s.ListFeedback(ctx, th.ID); len(fb) != 0 {
		t.Fatalf("feedback kept %+v", fb)
	}
}

func TestPGRunKindAndReasoningEffort(t *testing.T) {
	// Runs store their kind (default chat) and reasoning effort.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "t")
	in := newRun(th.ID, "q")
	in.ReasoningEffort = "low"

	// Act
	run, _, err := s.CreateRun(ctx, in)

	// Assert
	got, _ := s.GetRun(ctx, run.ID)
	if err != nil || got.Kind != store.KindChat || got.ReasoningEffort != "low" {
		t.Fatalf("err %v run %+v", err, got)
	}
}

func TestPGCompactionsFollowTheirMessageAndHideWithIt(t *testing.T) {
	// A compaction is recorded after the last visible message and is no longer listed once an edit hides that message.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "")
	first, firstMsg, _ := s.CreateRun(ctx, newRun(th.ID, "one"))
	_ = s.AppendMessages(ctx, &store.Message{ID: "a1", ThreadID: th.ID, RunID: first.ID, Role: store.RoleAssistant, Content: "ok"})
	_ = s.RecordCompaction(ctx, store.Compaction{ThreadID: th.ID, RunID: first.ID, Trigger: "auto", TokensBefore: 10, TokensAfter: 2})
	finish(t, s, first.ID)
	second, secondMsg, _ := s.CreateRun(ctx, newRun(th.ID, "two"))
	_ = s.RecordCompaction(ctx, store.Compaction{ThreadID: th.ID, RunID: second.ID, Trigger: "manual", TokensBefore: 20, TokensAfter: 3})
	finish(t, s, second.ID)
	before, _ := s.ListCompactions(ctx, th.ID)

	// Act
	in := newRun(th.ID, "two, edited")
	in.ReplaceFromMessageID = secondMsg.ID
	_, _, err := s.CreateRun(ctx, in)
	after, _ := s.ListCompactions(ctx, th.ID)

	// Assert
	if len(before) != 2 || before[0].AfterMessageID != "a1" || before[1].AfterMessageID != secondMsg.ID || before[1].Trigger != "manual" || before[0].TokensBefore != 10 {
		t.Fatalf("before %+v (first message %s)", before, firstMsg.ID)
	}
	if err != nil || len(after) != 1 || after[0].AfterMessageID != "a1" {
		t.Fatalf("err %v after %+v", err, after)
	}
}

func TestPGReplaceCancelsThePendingQuestionAndRecomputesThePlan(t *testing.T) {
	// An edit after an open question stores the cancel result; the plan becomes the last visible update_plan call's.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "")
	run, _, _ := s.CreateRun(ctx, newRun(th.ID, "plan and ask"))
	_ = s.AppendMessages(ctx,
		&store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleAssistant, ToolCalls: []store.ToolCall{
			{ID: "p1", Type: "function", Function: store.ToolCallFunction{Name: "update_plan", Arguments: `{"items":[{"id":"1","title":"Load","status":"in_progress"}]}`}},
			{ID: "ask", Type: "function", Function: store.ToolCallFunction{Name: "ask_user", Arguments: `{"question":"Which?"}`}}}},
		&store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleTool, ToolCallID: "p1", Content: "Plan updated"},
		&store.Message{ID: "steer", ThreadID: th.ID, RunID: run.ID, Role: store.RoleUser, Content: "and a chart", Metadata: map[string]any{"steer": true}})
	_ = s.SetThreadPlan(ctx, th.ID, []store.PlanItem{{ID: "9", Title: "stale", Status: "pending"}})
	_ = s.SetPendingQuestion(ctx, th.ID, &store.PendingQuestion{InterruptID: "i1", RunID: run.ID, ToolCallID: "ask", Question: "Which?"})
	finish(t, s, run.ID)
	in := newRun(th.ID, "and two charts")
	in.ReplaceFromMessageID = "steer"

	// Act
	_, _, err := s.CreateRun(ctx, in)

	// Assert
	msgs, _ := s.ListMessages(ctx, th.ID)
	got, _ := s.GetThread(ctx, "alice", th.ID)
	var result string
	for _, m := range msgs {
		if m.Role == store.RoleTool && m.ToolCallID == "ask" {
			result = m.Content
		}
	}
	if err != nil || result != store.ImplicitCancelResult || got.PendingQuestion != nil || len(got.Plan) != 1 || got.Plan[0].Title != "Load" {
		t.Fatalf("err %v result %q thread %+v", err, result, got)
	}
}
