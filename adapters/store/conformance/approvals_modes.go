package conformance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/7ar-chat/harness/core/store"
)

func approvalsModes(t *testing.T, f Factory) {
	t.Helper()
	t.Run("the owner's switch updates the thread and its active run, not finished runs, and the next run inherits it", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		ctx := context.Background()
		th := apprThread(t, s, "alice", "t")
		done, _, _ := s.CreateRun(ctx, apprNewRun(th.ID, "first"))
		apprFinish(t, s, done.ID, store.StatusCompleted)
		active, _, _ := s.CreateRun(ctx, apprNewRun(th.ID, "second"))
		if _, err := s.ClaimRun(ctx, active.ID, "rt"); err != nil {
			t.Fatal(err)
		}

		// Act
		err := s.SetThreadInteractionMode(ctx, "alice", th.ID, store.InteractionInteractive)

		// Assert
		if err != nil {
			t.Fatal(err)
		}
		thread, _ := s.GetThread(ctx, "alice", th.ID)
		activeMode, activeErr := s.RunInteractionMode(ctx, active.ID)
		doneMode, doneErr := s.RunInteractionMode(ctx, done.ID)
		if thread.InteractionMode != store.InteractionInteractive || activeErr != nil || activeMode != store.InteractionInteractive ||
			doneErr != nil || doneMode != store.InteractionAutomatic {
			t.Fatalf("thread %q active %q %v done %q %v", thread.InteractionMode, activeMode, activeErr, doneMode, doneErr)
		}
		if err := s.FinishRun(ctx, active.ID, "rt", store.StatusCompleted, "", ""); err != nil {
			t.Fatal(err)
		}
		next, _, _ := s.CreateRun(ctx, apprNewRun(th.ID, "third"))
		if next.InteractionMode != store.InteractionInteractive {
			t.Fatalf("next run %q", next.InteractionMode)
		}
	})

	t.Run("a queued run follows the switch too", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		ctx := context.Background()
		th := apprThread(t, s, "alice", "t")
		queued, _, _ := s.CreateRun(ctx, apprNewRun(th.ID, "first"))

		// Act
		err := s.SetThreadInteractionMode(ctx, "alice", th.ID, store.InteractionInteractive)
		mode, modeErr := s.RunInteractionMode(ctx, queued.ID)

		// Assert
		if err != nil || modeErr != nil || mode != store.InteractionInteractive {
			t.Fatalf("switch %v mode %q %v", err, mode, modeErr)
		}
	})

	t.Run("a switch leaves the pending approval pending", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		ctx := context.Background()
		th := apprThread(t, s, "alice", "t")
		_, a := apprPausedDone(t, s, th.ID)

		// Act
		err := s.SetThreadInteractionMode(ctx, "alice", th.ID, store.InteractionAutomatic)

		// Assert
		if p := apprPending(t, s, th.ID); err != nil || p == nil || p.ID != a.ID || p.Status != store.ApprovalPending {
			t.Fatalf("switch %v pending %+v", err, p)
		}
	})

	t.Run("a run's explicit mode is remembered by the thread; an empty one uses it", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		ctx := context.Background()
		th := apprThread(t, s, "alice", "t")
		fresh, _, _ := s.CreateRun(ctx, apprNewRun(th.ID, "default"))
		apprFinish(t, s, fresh.ID, store.StatusCompleted)
		explicit := apprNewRun(th.ID, "explicit")
		explicit.InteractionMode = store.InteractionInteractive

		// Act
		interactive, _, err := s.CreateRun(ctx, explicit)
		apprFinish(t, s, interactive.ID, store.StatusCompleted)
		inherited, _, inheritedErr := s.CreateRun(ctx, apprNewRun(th.ID, "inherits"))
		thread, _ := s.GetThread(ctx, "alice", th.ID)

		// Assert
		if err != nil || inheritedErr != nil {
			t.Fatalf("errors %v %v", err, inheritedErr)
		}
		if fresh.InteractionMode != store.InteractionAutomatic || interactive.InteractionMode != store.InteractionInteractive ||
			inherited.InteractionMode != store.InteractionInteractive || thread.InteractionMode != store.InteractionInteractive {
			t.Fatalf("fresh %q explicit %q inherited %q thread %q", fresh.InteractionMode, interactive.InteractionMode, inherited.InteractionMode, thread.InteractionMode)
		}
	})

	t.Run("another user's, deleted and unknown threads are not found", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		ctx := context.Background()
		th := apprThread(t, s, "alice", "t")
		gone := apprThread(t, s, "alice", "gone")
		if _, err := s.DeleteThread(ctx, "alice", gone.ID); err != nil {
			t.Fatal(err)
		}

		// Act
		other := s.SetThreadInteractionMode(ctx, "bob", th.ID, store.InteractionInteractive)
		deleted := s.SetThreadInteractionMode(ctx, "alice", gone.ID, store.InteractionInteractive)
		unknown := s.SetThreadInteractionMode(ctx, "alice", uuid.NewString(), store.InteractionInteractive)
		malformed := s.SetThreadInteractionMode(ctx, "alice", "not-a-uuid", store.InteractionInteractive)

		// Assert
		for name, err := range map[string]error{"other user": other, "deleted": deleted, "unknown": unknown, "malformed": malformed} {
			if !errors.Is(err, store.ErrNotFound) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if thread, _ := s.GetThread(ctx, "alice", th.ID); thread.InteractionMode != "" {
			t.Fatalf("another user changed the mode to %q", thread.InteractionMode)
		}
	})

	t.Run("an unknown run has no interaction mode", func(t *testing.T) {
		// Arrange
		s := f(t).Store

		// Act
		_, unknown := s.RunInteractionMode(context.Background(), uuid.NewString())
		_, malformed := s.RunInteractionMode(context.Background(), "not-a-uuid")

		// Assert
		if !errors.Is(unknown, store.ErrNotFound) || !errors.Is(malformed, store.ErrNotFound) {
			t.Fatalf("unknown %v malformed %v", unknown, malformed)
		}
	})
}

func approvalsScopes(t *testing.T, f Factory) {
	t.Helper()
	approvedTools := func(t *testing.T, s Store, runID string) []string {
		t.Helper()
		r, err := s.GetRun(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		return r.ApprovedTools
	}

	t.Run("approving for the run gives the resume run the tool kind; a question's answer keeps it, a new message does not", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		ctx := context.Background()
		th := apprThread(t, s, "alice", "t")
		_, a := apprPausedDone(t, s, th.ID)
		resume, _, err := s.CreateRun(ctx, apprResume(th.ID, a.ID, store.DecisionApprove, store.ApprovalScopeRun, store.ApprovalResponse{}))
		if err != nil {
			t.Fatal(err)
		}
		apprFinish(t, s, resume.ID, store.StatusCompleted)
		if err := s.SetPendingQuestion(ctx, th.ID, &store.PendingQuestion{InterruptID: "q1", RunID: resume.ID, ToolCallID: "ask", Question: "?"}); err != nil {
			t.Fatal(err)
		}
		answer, _, err := s.CreateRun(ctx, store.NewRun{RunID: uuid.NewString(), ThreadID: th.ID, UserLogin: "alice", Model: "m",
			Resume: &store.Resume{InterruptID: "q1", Content: "The user answered: yes"}})
		if err != nil {
			t.Fatal(err)
		}
		apprFinish(t, s, answer.ID, store.StatusCompleted)

		// Act
		fresh, _, freshErr := s.CreateRun(ctx, apprNewRun(th.ID, "something else"))

		// Assert
		kind := store.ToolKind("write_file")
		if freshErr != nil {
			t.Fatal(freshErr)
		}
		if got := apprGet(t, s, a.ID); got.Scope != store.ApprovalScopeRun || got.Decision != store.DecisionApprove {
			t.Fatalf("approval %+v", got)
		}
		if r1 := approvedTools(t, s, resume.ID); len(r1) != 1 || r1[0] != kind {
			t.Fatalf("resume run %v", r1)
		}
		if r2 := approvedTools(t, s, answer.ID); len(r2) != 1 || r2[0] != kind {
			t.Fatalf("answer run %v", r2)
		}
		if r3 := approvedTools(t, s, fresh.ID); len(r3) != 0 {
			t.Fatalf("fresh run %v", r3)
		}
	})

	t.Run("the tool kinds of a chain accumulate without duplicates", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		ctx := context.Background()
		th := apprThread(t, s, "alice", "t")
		_, first := apprPausedDone(t, s, th.ID)
		r1, _, _ := s.CreateRun(ctx, apprResume(th.ID, first.ID, store.DecisionApprove, store.ApprovalScopeRun, store.ApprovalResponse{}))
		second := apprPauseCall(t, s, r1, "c2", "Write-File") // the same kind as write_file
		apprFinish(t, s, r1.ID, store.StatusCompleted)
		r2, _, _ := s.CreateRun(ctx, apprResume(th.ID, second.ID, store.DecisionApprove, store.ApprovalScopeRun, store.ApprovalResponse{}))
		third := apprPauseCall(t, s, r2, "c3", "run_command")
		apprFinish(t, s, r2.ID, store.StatusCompleted)

		// Act
		r3, _, err := s.CreateRun(ctx, apprResume(th.ID, third.ID, store.DecisionApprove, store.ApprovalScopeRun, store.ApprovalResponse{}))

		// Assert
		if err != nil {
			t.Fatal(err)
		}
		if got := approvedTools(t, s, r2.ID); len(got) != 1 || got[0] != "writefile" {
			t.Fatalf("second run %v", got)
		}
		got := approvedTools(t, s, r3.ID)
		if len(got) != 2 || got[0] != "writefile" || got[1] != "runcommand" {
			t.Fatalf("third run %v", got)
		}
	})

	t.Run("a decision that is not an approve for the run adds no tool kind but keeps the paused run's", func(t *testing.T) {
		cases := []struct {
			name     string
			decision string
			scope    string
		}{
			{"approve once", store.DecisionApprove, store.ApprovalScopeOnce},
			{"approve with the default scope", store.DecisionApprove, ""},
			{"deny with scope run", store.DecisionDeny, store.ApprovalScopeRun},
			{"respond with scope run", store.DecisionRespond, store.ApprovalScopeRun},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				// Arrange
				s := f(t).Store
				ctx := context.Background()
				th := apprThread(t, s, "alice", "t")
				_, first := apprPausedDone(t, s, th.ID)
				r1, _, _ := s.CreateRun(ctx, apprResume(th.ID, first.ID, store.DecisionApprove, store.ApprovalScopeRun, store.ApprovalResponse{}))
				second := apprPauseCall(t, s, r1, "c2", "run_command")
				apprFinish(t, s, r1.ID, store.StatusCompleted)

				// Act
				r2, _, err := s.CreateRun(ctx, apprResume(th.ID, second.ID, tc.decision, tc.scope, store.ApprovalResponse{}))

				// Assert
				if err != nil {
					t.Fatal(err)
				}
				if got := apprGet(t, s, second.ID); got.Scope != store.ApprovalScopeOnce {
					t.Fatalf("scope %q", got.Scope)
				}
				if got := approvedTools(t, s, r2.ID); len(got) != 1 || got[0] != "writefile" {
					t.Fatalf("approved tools %v", got)
				}
			})
		}
	})
}

func approvalsExpiry(t *testing.T, f Factory) {
	t.Helper()
	t.Run("a pending approval older than the ttl expires with the expired tool result", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		ctx := context.Background()
		th := apprThread(t, s, "alice", "t")
		_, a := apprPausedDone(t, s, th.ID)
		fresh, freshErr := s.ExpireApprovals(ctx, 72*time.Hour, "")
		b.Advance(t, 73*time.Hour)

		// Act
		n, err := s.ExpireApprovals(ctx, 72*time.Hour, th.ID)
		again, againErr := s.ExpireApprovals(ctx, 72*time.Hour, "")

		// Assert
		if freshErr != nil || fresh != 0 || err != nil || n != 1 || againErr != nil || again != 0 {
			t.Fatalf("fresh %d %v expired %d %v again %d %v", fresh, freshErr, n, err, again, againErr)
		}
		got := apprGet(t, s, a.ID)
		if got.Status != store.ApprovalExpired || got.ResolvedAt == nil || apprPending(t, s, th.ID) != nil {
			t.Fatalf("approval %+v", got)
		}
		results := apprToolResults(t, s, th.ID)
		if len(results) != 1 || results[0].Content != store.ApprovalExpiredResult || results[0].ToolCallID != "c1" {
			t.Fatalf("results %+v", results)
		}
	})

	t.Run("an approval younger than the ttl stays pending", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		th := apprThread(t, s, "alice", "t")
		_, a := apprPausedDone(t, s, th.ID)
		b.Advance(t, 2*time.Hour)

		// Act
		n, err := s.ExpireApprovals(context.Background(), 3*time.Hour, "")

		// Assert
		if err != nil || n != 0 || apprGet(t, s, a.ID).Status != store.ApprovalPending || apprPending(t, s, th.ID) == nil {
			t.Fatalf("expired %d %v", n, err)
		}
	})

	t.Run("a thread filter expires that thread's approvals only", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		ctx := context.Background()
		one := apprThread(t, s, "alice", "one")
		two := apprThread(t, s, "alice", "two")
		_, a := apprPausedDone(t, s, one.ID)
		_, c := apprPausedDone(t, s, two.ID)
		b.Advance(t, time.Hour)

		// Act
		n, err := s.ExpireApprovals(ctx, time.Minute, one.ID)
		unknown, unknownErr := s.ExpireApprovals(ctx, time.Minute, uuid.NewString())
		malformed, malformedErr := s.ExpireApprovals(ctx, time.Minute, "not-a-uuid")

		// Assert
		if err != nil || n != 1 || unknownErr != nil || unknown != 0 || malformedErr != nil || malformed != 0 {
			t.Fatalf("expired %d %v unknown %d %v malformed %d %v", n, err, unknown, unknownErr, malformed, malformedErr)
		}
		if apprGet(t, s, a.ID).Status != store.ApprovalExpired || apprGet(t, s, c.ID).Status != store.ApprovalPending {
			t.Fatal("the filter did not limit the expiry to one thread")
		}
		if len(apprToolResults(t, s, two.ID)) != 0 {
			t.Fatal("the other thread got a tool result")
		}
	})

	t.Run("without a filter every old pending approval expires; resolved ones do not", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		ctx := context.Background()
		one := apprThread(t, s, "alice", "one")
		two := apprThread(t, s, "alice", "two")
		decided := apprThread(t, s, "alice", "decided")
		_, a := apprPausedDone(t, s, one.ID)
		_, c := apprPausedDone(t, s, two.ID)
		_, d := apprPausedDone(t, s, decided.ID)
		if _, _, err := s.CreateRun(ctx, apprResume(decided.ID, d.ID, store.DecisionDeny, "", store.ApprovalResponse{})); err != nil {
			t.Fatal(err)
		}
		b.Advance(t, time.Hour)

		// Act
		n, err := s.ExpireApprovals(ctx, time.Minute, "")

		// Assert
		if err != nil || n != 2 {
			t.Fatalf("expired %d %v", n, err)
		}
		if apprGet(t, s, a.ID).Status != store.ApprovalExpired || apprGet(t, s, c.ID).Status != store.ApprovalExpired || apprGet(t, s, d.ID).Status != store.ApprovalResolved {
			t.Fatal("wrong approvals expired")
		}
	})

	t.Run("an approval whose run did not complete expires without a tool result", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		th := apprThread(t, s, "alice", "t")
		run, a := apprPaused(t, s, th.ID)
		apprFinish(t, s, run.ID, store.StatusFailed)
		b.Advance(t, time.Hour)

		// Act
		n, err := s.ExpireApprovals(context.Background(), time.Minute, th.ID)

		// Assert
		if err != nil || n != 1 || apprGet(t, s, a.ID).Status != store.ApprovalExpired || len(apprToolResults(t, s, th.ID)) != 0 {
			t.Fatalf("expired %d %v", n, err)
		}
	})

	t.Run("the latest expired approval is reported until another run starts", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		ctx := context.Background()
		th := apprThread(t, s, "alice", "t")
		run, a := apprPausedDone(t, s, th.ID)
		beforeExpiry, beforeErr := s.LatestExpiredApproval(ctx, th.ID)
		b.Advance(t, time.Hour)
		if n, err := s.ExpireApprovals(ctx, time.Minute, th.ID); err != nil || n != 1 {
			t.Fatalf("expire %d %v", n, err)
		}

		// Act
		expired, err := s.LatestExpiredApproval(ctx, th.ID)
		next, _, nextErr := s.CreateRun(ctx, apprNewRun(th.ID, "next"))
		hidden, hiddenErr := s.LatestExpiredApproval(ctx, th.ID)
		announced, announcedErr := s.ResumedApproval(ctx, next.ID)

		// Assert
		if beforeErr != nil || beforeExpiry != nil {
			t.Fatalf("before expiry %+v %v", beforeExpiry, beforeErr)
		}
		if err != nil || expired == nil || expired.ID != a.ID || expired.Status != store.ApprovalExpired || expired.RunID != run.ID {
			t.Fatalf("expired %+v %v", expired, err)
		}
		if nextErr != nil || hiddenErr != nil || hidden != nil {
			t.Fatalf("after another run: %+v %v %v", hidden, nextErr, hiddenErr)
		}
		if announcedErr != nil || announced == nil || announced.ID != a.ID || announced.Status != store.ApprovalExpired {
			t.Fatalf("the next run did not announce the expiry: %+v %v", announced, announcedErr)
		}
	})

	t.Run("no expired approval for a thread without approvals, an unknown or a malformed id", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		ctx := context.Background()
		th := apprThread(t, s, "alice", "t")
		run, _, _ := s.CreateRun(ctx, apprNewRun(th.ID, "hi"))
		apprFinish(t, s, run.ID, store.StatusCompleted)

		// Act
		none, noneErr := s.LatestExpiredApproval(ctx, th.ID)
		unknown, unknownErr := s.LatestExpiredApproval(ctx, uuid.NewString())
		malformed, malformedErr := s.LatestExpiredApproval(ctx, "not-a-uuid")

		// Assert
		if none != nil || noneErr != nil || unknown != nil || unknownErr != nil || malformed != nil || malformedErr != nil {
			t.Fatalf("%+v %v %+v %v %+v %v", none, noneErr, unknown, unknownErr, malformed, malformedErr)
		}
	})

	t.Run("a pending or cancelled latest approval is not an expired one", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		ctx := context.Background()
		th := apprThread(t, s, "alice", "t")
		run, _ := apprPausedDone(t, s, th.ID)
		pending, pendingErr := s.LatestExpiredApproval(ctx, th.ID)
		if _, err := s.CancelPendingApproval(ctx, run.ID, store.ApprovalCancelledResult); err != nil {
			t.Fatal(err)
		}

		// Act
		cancelled, cancelledErr := s.LatestExpiredApproval(ctx, th.ID)

		// Assert
		if pending != nil || pendingErr != nil || cancelled != nil || cancelledErr != nil {
			t.Fatalf("pending %+v %v cancelled %+v %v", pending, pendingErr, cancelled, cancelledErr)
		}
	})
}

func approvalsSanitize(t *testing.T, f Factory) {
	t.Helper()
	t.Run("approval fields and results are sanitized", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		ctx := context.Background()
		th := apprThread(t, s, "alice", "t")
		run, _, _ := s.CreateRun(ctx, apprNewRun(th.ID, "go"))
		a := &store.Approval{ThreadID: th.ID, RunID: run.ID, ToolCallID: "c\x001", ToolName: "w\xffrite", ToolInputPreview: "p\x00"}
		resp := store.ApprovalResponse{SelectedChoice: "a\x00", ResponseText: "b\xff"}

		// Act
		createErr := s.CreateApproval(ctx, a)
		apprFinish(t, s, run.ID, store.StatusCompleted)
		next, _, resumeErr := s.CreateRun(ctx, apprResume(th.ID, a.ID, store.DecisionRespond, "", resp))

		// Assert
		if createErr != nil || resumeErr != nil {
			t.Fatalf("create %v resume %v", createErr, resumeErr)
		}
		got := apprGet(t, s, a.ID)
		cleaned := store.ApprovalResponse{SelectedChoice: "a\ufffd", ResponseText: "b\ufffd"}
		if got.ToolCallID != "c\ufffd1" || got.ToolName != "w\ufffdrite" || got.ToolInputPreview != "p\ufffd" || got.Response != cleaned {
			t.Fatalf("stored %+v", got)
		}
		results := apprToolResults(t, s, th.ID)
		if len(results) != 1 || results[0].RunID != next.ID || results[0].ToolCallID != "c\ufffd1" || results[0].Content != store.ApprovalResult(store.DecisionRespond, cleaned) {
			t.Fatalf("results %+v", results)
		}
	})

	t.Run("a cancel result is sanitized", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := apprThread(t, s, "alice", "t")
		run, _ := apprPausedDone(t, s, th.ID)

		// Act
		_, err := s.CancelPendingApproval(context.Background(), run.ID, "stopped\x00 by \xffuser")

		// Assert
		results := apprToolResults(t, s, th.ID)
		if err != nil || len(results) != 1 || results[0].Content != "stopped\ufffd by \ufffduser" {
			t.Fatalf("err %v results %+v", err, results)
		}
	})
}
