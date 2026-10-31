package pgstore_test

import (
	"context"
	"errors"
	"github.com/7ar-chat/harness/adapters/store/pgstore"
	"testing"

	"github.com/7ar-chat/harness/core/store"
)

// pausedRun creates an interactive run that ended waiting for an approval of call "c1".
func pausedRun(t *testing.T, s *pgstore.PG, threadID string) (*store.Run, *store.Approval) {
	t.Helper()
	ctx := context.Background()
	in := newRun(threadID, "write it")
	in.InteractionMode = store.InteractionInteractive
	run, _, err := s.CreateRun(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.AppendMessages(ctx, &store.Message{ThreadID: threadID, RunID: run.ID, Role: store.RoleAssistant,
		ToolCalls: []store.ToolCall{{ID: "c1", Type: "function", Function: store.ToolCallFunction{Name: "write_file", Arguments: `{"path":"a"}`}}}})
	a := &store.Approval{ThreadID: threadID, RunID: run.ID, ToolCallID: "c1", ToolName: "write_file", ToolInputPreview: `{"path":"a"}`}
	if err := s.CreateApproval(ctx, a); err != nil {
		t.Fatal(err)
	}
	return run, a
}

func toolResults(t *testing.T, s *pgstore.PG, threadID string) []*store.Message {
	t.Helper()
	msgs, err := s.ListMessages(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	var out []*store.Message
	for _, m := range msgs {
		if m.Role == store.RoleTool {
			out = append(out, m)
		}
	}
	return out
}

func TestPGApprovalIsPendingOnceItsRunCompleted(t *testing.T) {
	// A pending approval shows on the thread only after its run completed; the run and thread keep the interaction mode.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "t")
	run, a := pausedRun(t, s, th.ID)
	running, _ := s.GetThread(ctx, "alice", th.ID)

	// Act
	finish(t, s, run.ID)
	done, _ := s.GetThread(ctx, "alice", th.ID)

	// Assert
	if running.PendingApproval != nil || run.InteractionMode != store.InteractionInteractive || done.InteractionMode != store.InteractionInteractive {
		t.Fatalf("running %+v run mode %q thread mode %q", running.PendingApproval, run.InteractionMode, done.InteractionMode)
	}
	p := done.PendingApproval
	if p == nil || p.ID != a.ID || p.RunID != run.ID || p.ToolCallID != "c1" || p.ToolInputPreview != `{"path":"a"}` ||
		p.Status != store.ApprovalPending || p.Kind != store.ApprovalKindRiskyTool || p.CreatedAt.IsZero() {
		t.Fatalf("pending %+v", p)
	}
	if err := s.CreateApproval(ctx, &store.Approval{ThreadID: th.ID, RunID: run.ID, ToolCallID: "c2", ToolName: "x"}); err == nil {
		t.Fatal("a second pending approval of the run was accepted")
	}
}

func TestPGResumeDecidesTheApproval(t *testing.T) {
	// A resume with the approval id resolves it (deny stores its tool result, approve none) and inherits the thread's mode.
	cases := []struct {
		decision string
		result   string
	}{
		{store.DecisionDeny, store.ApprovalDeniedResult},
		{store.DecisionApprove, ""},
	}
	for _, tc := range cases {
		t.Run(tc.decision, func(t *testing.T) {
			// Arrange
			s := newPG(t)
			ctx := context.Background()
			th, _ := s.CreateThread(ctx, "alice", "t")
			run, a := pausedRun(t, s, th.ID)
			finish(t, s, run.ID)
			in := newRun(th.ID, "")
			in.Message = store.Message{}
			in.Resume = &store.Resume{InterruptID: a.ID, Decision: tc.decision}

			// Act
			next, _, err := s.CreateRun(ctx, in)

			// Assert
			if err != nil {
				t.Fatal(err)
			}
			got, _ := s.ResumedApproval(ctx, next.ID)
			if got == nil || got.ID != a.ID || got.Status != store.ApprovalResolved || got.Decision != tc.decision || got.ResumeRunID != next.ID || got.ResolvedAt == nil {
				t.Fatalf("approval %+v", got)
			}
			results := toolResults(t, s, th.ID)
			if tc.result == "" && len(results) != 0 || tc.result != "" && (len(results) != 1 || results[0].Content != tc.result || results[0].RunID != next.ID) {
				t.Fatalf("results %+v", results)
			}
			if next.InteractionMode != store.InteractionInteractive {
				t.Fatalf("mode %q", next.InteractionMode)
			}
			if thread, _ := s.GetThread(ctx, "alice", th.ID); thread.PendingApproval != nil {
				t.Fatal("still pending")
			}
		})
	}
}

func TestPGRunsAgainstAPendingApproval(t *testing.T) {
	// Without a message a run is refused; a message cancels the approval with a tool result; another id is no pending question.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "t")
	run, a := pausedRun(t, s, th.ID)
	finish(t, s, run.ID)
	compact := newRun(th.ID, "")
	compact.Message, compact.Kind = store.Message{}, store.KindCompact
	wrong := newRun(th.ID, "")
	wrong.Message, wrong.Resume = store.Message{}, &store.Resume{InterruptID: "other", Decision: store.DecisionApprove}

	// Act
	_, _, compactErr := s.CreateRun(ctx, compact)
	_, _, wrongErr := s.CreateRun(ctx, wrong)
	next, _, msgErr := s.CreateRun(ctx, newRun(th.ID, "something else"))

	// Assert
	if !errors.Is(compactErr, store.ErrApprovalPending) || !errors.Is(wrongErr, store.ErrNoPendingQuestion) || msgErr != nil {
		t.Fatalf("errors %v %v %v", compactErr, wrongErr, msgErr)
	}
	got, _ := s.GetApproval(ctx, a.ID)
	results := toolResults(t, s, th.ID)
	if got.Status != store.ApprovalCancelled || got.ResumeRunID != next.ID || len(results) != 1 || results[0].Content != store.ImplicitApprovalCancelResult {
		t.Fatalf("approval %+v results %+v", got, results)
	}
}

func TestPGCancelApprovals(t *testing.T) {
	// CancelPendingApproval stores the cancel result once; CancelRunApprovals drops the approval of a run that did not complete.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "t")
	run, a := pausedRun(t, s, th.ID)
	finish(t, s, run.ID)
	other, _ := s.CreateThread(ctx, "alice", "u")
	failed, b := pausedRun(t, s, other.ID)

	// Act
	cancelled, err := s.CancelPendingApproval(ctx, run.ID, store.ApprovalCancelledResult)
	_, again := s.CancelPendingApproval(ctx, run.ID, store.ApprovalCancelledResult)
	dropErr := s.CancelRunApprovals(ctx, failed.ID)

	// Assert
	if err != nil || cancelled.ID != a.ID || cancelled.Status != store.ApprovalCancelled || !errors.Is(again, store.ErrApprovalNotPending) || dropErr != nil {
		t.Fatalf("cancel %+v %v %v %v", cancelled, err, again, dropErr)
	}
	if results := toolResults(t, s, th.ID); len(results) != 1 || results[0].ToolCallID != "c1" || results[0].Content != store.ApprovalCancelledResult {
		t.Fatalf("results %+v", results)
	}
	if got, _ := s.GetApproval(ctx, b.ID); got.Status != store.ApprovalCancelled || len(toolResults(t, s, other.ID)) != 0 {
		t.Fatalf("dropped %+v", got)
	}
}

func TestPGReopenApprovalOnlyWhenTheResumeRunDidNotRunTheCall(t *testing.T) {
	// A resolved approval whose resume run failed without a result for the call becomes pending again; not while
	// the resume run is active, nor once the call has a result.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "t")
	run, a := pausedRun(t, s, th.ID)
	finish(t, s, run.ID)
	in := newRun(th.ID, "")
	in.Message = store.Message{}
	in.Resume = &store.Resume{InterruptID: a.ID, Decision: store.DecisionApprove}
	resume, _, err := s.CreateRun(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	// Act
	_, whileQueued := s.ReopenApproval(ctx, a.ID)
	if _, err := s.ClaimRun(ctx, resume.ID, "rt"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(ctx, resume.ID, "rt", store.StatusFailed, "boom", ""); err != nil {
		t.Fatal(err)
	}
	reopened, err := s.ReopenApproval(ctx, a.ID)

	// Assert
	if !errors.Is(whileQueued, store.ErrApprovalNotPending) {
		t.Fatalf("reopened while the resume run was queued: %v", whileQueued)
	}
	if err != nil || reopened.Status != store.ApprovalPending || reopened.Decision != "" || reopened.ResumeRunID != "" || reopened.ResolvedAt != nil {
		t.Fatalf("reopened %+v %v", reopened, err)
	}
	if thread, _ := s.GetThread(ctx, "alice", th.ID); thread.PendingApproval == nil || thread.PendingApproval.ID != a.ID {
		t.Fatalf("thread pending %+v", thread.PendingApproval)
	}
	in = newRun(th.ID, "")
	in.Message = store.Message{}
	in.Resume = &store.Resume{InterruptID: a.ID, Decision: store.DecisionDeny}
	denied, _, err := s.CreateRun(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	finish(t, s, denied.ID)
	if _, err := s.ReopenApproval(ctx, a.ID); !errors.Is(err, store.ErrApprovalNotPending) {
		t.Fatalf("reopened although the deny result is stored: %v", err)
	}
}

func TestPGNextRunAnnouncesAnApprovalCancelledByStop(t *testing.T) {
	// An approval cancelled with its waiting run is returned by ResumedApproval for the next (non-compact) run only.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "t")
	run, a := pausedRun(t, s, th.ID)
	finish(t, s, run.ID)
	if _, err := s.CancelPendingApproval(ctx, run.ID, store.ApprovalCancelledResult); err != nil {
		t.Fatal(err)
	}

	// Act
	next, _, err := s.CreateRun(ctx, newRun(th.ID, "never mind"))
	if err != nil {
		t.Fatal(err)
	}
	finish(t, s, next.ID)
	later, _, err := s.CreateRun(ctx, newRun(th.ID, "thanks"))

	// Assert
	got, _ := s.ResumedApproval(ctx, next.ID)
	none, _ := s.ResumedApproval(ctx, later.ID)
	if err != nil || got == nil || got.ID != a.ID || got.Status != store.ApprovalCancelled || none != nil {
		t.Fatalf("next %+v later %+v %v", got, none, err)
	}
}

func TestPGSetThreadInteractionModeSwitchesTheThreadAndItsActiveRun(t *testing.T) {
	// The owner's switch updates the thread and its active run (the gate reads it live), not finished runs; the next run inherits it; others' threads are not found.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "t")
	done, _, _ := s.CreateRun(ctx, newRun(th.ID, "first"))
	finish(t, s, done.ID)
	active, _, _ := s.CreateRun(ctx, newRun(th.ID, "second"))
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
	doneMode, _ := s.RunInteractionMode(ctx, done.ID)
	if thread.InteractionMode != store.InteractionInteractive || activeErr != nil || activeMode != store.InteractionInteractive || doneMode != store.InteractionAutomatic {
		t.Fatalf("thread %q active %q %v done %q", thread.InteractionMode, activeMode, activeErr, doneMode)
	}
	if err := s.FinishRun(ctx, active.ID, "rt", store.StatusCompleted, "", ""); err != nil {
		t.Fatal(err)
	}
	next, _, _ := s.CreateRun(ctx, newRun(th.ID, "third"))
	if next.InteractionMode != store.InteractionInteractive {
		t.Fatalf("next run %q", next.InteractionMode)
	}
	if err := s.SetThreadInteractionMode(ctx, "bob", th.ID, store.InteractionAutomatic); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("other user's thread: %v", err)
	}
	if _, err := s.RunInteractionMode(ctx, "not-a-uuid"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown run: %v", err)
	}
}
