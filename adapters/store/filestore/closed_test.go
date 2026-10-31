package filestore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/store"
)

// TestEveryMutationReportsAJournalFailureAfterClose checks that a change that cannot be written to the journal is
// reported to the caller (ErrClosed) instead of being acknowledged and lost on the next start.
func TestEveryMutationReportsAJournalFailureAfterClose(t *testing.T) {
	// Arrange: a store with a finished thread, a running run and a pending approval; the mutations below each change state.
	dir := t.TempDir()
	s := open(t, dir)
	ctx := context.Background()
	threadID, _ := populate(t, s)
	other := must(s.CreateThread(ctx, "alice", "other"))
	running, _, err := s.CreateRun(ctx, store.NewRun{RunID: "run-live", ThreadID: other.ID, UserLogin: "alice", Model: "m", Message: store.Message{Role: store.RoleUser, Content: "go"}})
	ok(t, err)
	queued := must(s.CreateThread(ctx, "alice", "queued"))
	waiting, _, err := s.CreateRun(ctx, store.NewRun{RunID: "run-wait", ThreadID: queued.ID, UserLogin: "alice", Model: "m", Message: store.Message{Role: store.RoleUser, Content: "go"}})
	ok(t, err)
	_, err = s.ClaimRun(ctx, running.ID, "rt")
	ok(t, err)
	// Threads of their own for the calls that other calls would disturb (a new run cancels a pending approval and blocks edits).
	edited := must(s.CreateThread(ctx, "alice", "edited"))
	turn(t, s, edited.ID, "edit-me", "answer")
	editTarget := must(s.ListMessages(ctx, edited.ID))[0]
	gated := must(s.CreateThread(ctx, "alice", "gated"))
	gatedRun, _, err := s.CreateRun(ctx, store.NewRun{RunID: "run-gated", ThreadID: gated.ID, UserLogin: "alice", Model: "m", Message: store.Message{Role: store.RoleUser, Content: "go"}})
	ok(t, err)
	ok(t, s.CreateApproval(ctx, &store.Approval{ID: "ap-gated", ThreadID: gated.ID, RunID: gatedRun.ID, ToolCallID: "c", ToolName: "bash", ToolInputPreview: "ls"}))
	_, err = s.ClaimRun(ctx, gatedRun.ID, "rt")
	ok(t, err)
	ok(t, s.FinishRun(ctx, gatedRun.ID, "rt", store.StatusCompleted, "", ""))
	msgs := must(s.ListMessages(ctx, threadID))
	feedbackOn := msgs[3].ID
	ok(t, s.Close())
	title, pin := "renamed", false

	calls := []struct {
		name string
		call func() error
	}{
		{"CreateThread", func() error { _, err := s.CreateThread(ctx, "alice", "late"); return err }},
		{"UpdateThread", func() error {
			_, err := s.UpdateThread(ctx, "alice", threadID, store.ThreadPatch{Title: &title, Pinned: &pin})
			return err
		}},
		{"DeleteThread", func() error { _, err := s.DeleteThread(ctx, "alice", other.ID); return err }},
		{"MarkThreadSeen", func() error { return s.MarkThreadSeen(ctx, "alice", queued.ID) }},
		{"CreateRun", func() error {
			_, _, err := s.CreateRun(ctx, store.NewRun{RunID: "run-late", ThreadID: threadID, UserLogin: "alice", Model: "m", Message: store.Message{Role: store.RoleUser, Content: "x"}})
			return err
		}},
		{"ClaimRun", func() error { _, err := s.ClaimRun(ctx, waiting.ID, "rt"); return err }},
		{"RequestCancel", func() error { _, err := s.RequestCancel(ctx, running.ID); return err }},
		{"FinishRun", func() error { return s.FinishRun(ctx, running.ID, "rt", store.StatusCompleted, "", "") }},
		{"SetRunOutcome", func() error { return s.SetRunOutcome(ctx, running.ID, store.StatusFailed, "boom", "x") }},
		{"AppendMessages", func() error {
			return s.AppendMessages(ctx, &store.Message{ThreadID: threadID, Role: store.RoleUser, Content: "late"})
		}},
		{"AppendEvents", func() error {
			return s.AppendEvents(ctx, running.ID, []store.Event{{Seq: 1, Type: "X", Data: []byte(`{}`)}})
		}},
		{"SetThreadSummary", func() error { return s.SetThreadSummary(ctx, threadID, store.ThreadSummary{Text: "new"}) }},
		{"SetThreadPlan", func() error {
			return s.SetThreadPlan(ctx, threadID, []store.PlanItem{{ID: "9", Title: "n", Status: store.PlanPending}})
		}},
		{"SetPendingQuestion", func() error {
			return s.SetPendingQuestion(ctx, threadID, &store.PendingQuestion{InterruptID: "i", Question: "q"})
		}},
		{"PutGoal", func() error { _, err := s.PutGoal(ctx, "alice", queued.ID, "g", 2); return err }},
		{"ClearGoal", func() error { return s.ClearGoal(ctx, "alice", threadID) }},
		{"UpdateGoal", func() error {
			g := must(s.ThreadGoal(ctx, threadID))
			_, err := s.UpdateGoal(ctx, threadID, store.GoalUpdate{ID: g.ID, Verdict: store.GoalVerdict{Reason: "r"}})
			return err
		}},
		{"SetFeedback", func() error { return s.SetFeedback(ctx, "alice", feedbackOn, store.Feedback{Sentiment: "dislike"}) }},
		{"DeleteFeedback", func() error { return s.DeleteFeedback(ctx, "alice", feedbackOn) }},
		{"RecordCompaction", func() error { return s.RecordCompaction(ctx, store.Compaction{ThreadID: threadID, Trigger: "manual"}) }},
		{"SetThreadInteractionMode", func() error { return s.SetThreadInteractionMode(ctx, "alice", threadID, store.InteractionAutomatic) }},
		{"SupersedeFrom", func() error { return s.SupersedeFrom(ctx, "alice", edited.ID, editTarget.ID) }},
		{"SetMemoryEnabled", func() error { return s.SetMemoryEnabled(ctx, "alice", false) }},
		{"CreateMemoryEntry", func() error {
			_, err := s.CreateMemoryEntry(ctx, memEntry("alice", "late.md", "x"), store.MemoryLimits{})
			return err
		}},
		{"CreateApproval", func() error {
			return s.CreateApproval(ctx, &store.Approval{ID: "late", ThreadID: queued.ID, RunID: waiting.ID, ToolCallID: "c", ToolName: "bash"})
		}},
		{"ExpireApprovals(all)", func() error {
			s.Now = func() time.Time { return time.Now().Add(48 * time.Hour) }
			_, err := s.ExpireApprovals(ctx, time.Hour, "")
			return err
		}},
	}

	for _, c := range calls {
		// Act
		err := c.call()

		// Assert
		if !errors.Is(err, ErrClosed) {
			t.Errorf("%s after Close: %v, want ErrClosed", c.name, err)
		}
	}
}
