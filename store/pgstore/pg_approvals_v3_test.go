package pgstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/store"
)

func TestPGScopeRunApprovalIsInheritedAlongTheRunChain(t *testing.T) {
	// Approving with scope run gives the resume run the tool kind; a later answer to a question of that run keeps it, a new message does not.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "t")
	run, a := pausedRun(t, s, th.ID)
	finish(t, s, run.ID)
	resume, _, err := s.CreateRun(ctx, store.NewRun{RunID: newRun(th.ID, "").RunID, ThreadID: th.ID, UserLogin: "alice", Model: "m",
		Resume: &store.Resume{InterruptID: a.ID, Decision: store.DecisionApprove, Scope: store.ApprovalScopeRun}})
	if err != nil {
		t.Fatal(err)
	}
	finish(t, s, resume.ID)
	_ = s.SetPendingQuestion(ctx, th.ID, &store.PendingQuestion{InterruptID: "q1", RunID: resume.ID, ToolCallID: "ask", Question: "?"})
	answer, _, err := s.CreateRun(ctx, store.NewRun{RunID: newRun(th.ID, "").RunID, ThreadID: th.ID, UserLogin: "alice", Model: "m",
		Resume: &store.Resume{InterruptID: "q1", Content: "The user answered: yes"}})
	if err != nil {
		t.Fatal(err)
	}
	finish(t, s, answer.ID)

	// Act
	fresh, _, err := s.CreateRun(ctx, newRun(th.ID, "something else"))

	// Assert
	r1, _ := s.GetRun(ctx, resume.ID)
	r2, _ := s.GetRun(ctx, answer.ID)
	r3, _ := s.GetRun(ctx, fresh.ID)
	ap, _ := s.GetApproval(ctx, a.ID)
	kind := store.ToolKind("write_file")
	if err != nil || ap.Scope != store.ApprovalScopeRun || len(r1.ApprovedTools) != 1 || r1.ApprovedTools[0] != kind ||
		len(r2.ApprovedTools) != 1 || len(r3.ApprovedTools) != 0 {
		t.Fatalf("err %v scope %q resume %v answer %v fresh %v", err, ap.Scope, r1.ApprovedTools, r2.ApprovedTools, r3.ApprovedTools)
	}
}

func TestPGExpireApprovalsExpiresOldPendingOnes(t *testing.T) {
	// A pending approval older than the TTL becomes expired with the tool result "The approval expired."; the thread waits for nothing.
	// Arrange
	s, pool := newPGPool(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "t")
	run, a := pausedRun(t, s, th.ID)
	finish(t, s, run.ID)
	fresh, _ := s.ExpireApprovals(ctx, 72*time.Hour, "")
	_, _ = pool.Exec(ctx, `UPDATE work_approvals SET created_at = now() - interval '73 hours' WHERE id = $1`, a.ID)

	// Act
	n, err := s.ExpireApprovals(ctx, 72*time.Hour, th.ID)

	// Assert
	ap, _ := s.GetApproval(ctx, a.ID)
	got, _ := s.GetThread(ctx, "alice", th.ID)
	results := toolResults(t, s, th.ID)
	if fresh != 0 || err != nil || n != 1 || ap.Status != store.ApprovalExpired || got.PendingApproval != nil ||
		len(results) != 1 || results[0].Content != store.ApprovalExpiredResult {
		t.Fatalf("fresh %d n %d err %v status %s pending %+v results %d", fresh, n, err, ap.Status, got.PendingApproval, len(results))
	}
}

func TestPGLatestExpiredApprovalIsOnlyTheLatestRunsOne(t *testing.T) {
	// LatestExpiredApproval returns the thread's last approval when it expired and belongs to the latest run; a later run hides it (review round 1).
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "")
	run, _, _ := s.CreateRun(ctx, newRun(th.ID, "write"))
	_, _ = s.ClaimRun(ctx, run.ID, "rt")
	_ = s.FinishRun(ctx, run.ID, "rt", store.StatusCompleted, "", "")
	a := &store.Approval{ThreadID: th.ID, RunID: run.ID, ToolCallID: "c1", ToolName: "write_file", ToolInputPreview: "{}"}
	_ = s.CreateApproval(ctx, a)
	_, _ = s.ExpireApprovals(ctx, -time.Second, th.ID)

	// Act
	expired, err := s.LatestExpiredApproval(ctx, th.ID)
	later, _, _ := s.CreateRun(ctx, newRun(th.ID, "next"))
	hidden, hiddenErr := s.LatestExpiredApproval(ctx, th.ID)

	// Assert
	if err != nil || expired == nil || expired.ID != a.ID || expired.Status != store.ApprovalExpired || later == nil || hiddenErr != nil || hidden != nil {
		t.Fatalf("err %v expired %+v later %v hidden %+v %v", err, expired, later, hidden, hiddenErr)
	}
}
