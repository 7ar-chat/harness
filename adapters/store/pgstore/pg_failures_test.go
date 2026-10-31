package pgstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/7ar-chat/harness/adapters/store/pgstore"
	"github.com/7ar-chat/harness/core/store"
)

// domainSentinels are the answers callers treat as business-as-usual; a database failure must
// never be reported as one of them.
var domainSentinels = []error{
	store.ErrNotFound, store.ErrRunActive, store.ErrNotClaimable, store.ErrConflict, store.ErrNotOwned,
	store.ErrApprovalPending, store.ErrApprovalNotPending, store.ErrLockTimeout,
	store.ErrInvalidReplace, store.ErrNoPendingQuestion, store.ErrQuestionPending, store.ErrGoalInactive,
	store.ErrMemoryLimit, store.ErrMemoryPathExists, store.ErrMemoryConflict, store.ErrInvalidData,
	store.ErrForbidden, store.ErrLastOwner,
}

// A database that cannot be reached must surface as an error from every operation; in particular
// it must not be mistaken for a normal domain answer (not found, conflict, ...) nor yield data.
func TestPGSurfacesDatabaseFailures(t *testing.T) {
	// Arrange: a pool that was closed, so that every statement fails. No server is needed.
	cfg, err := pgxpool.ParseConfig("postgres://harness:harness@127.0.0.1:1/harness?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	s := pgstore.NewPG(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	login, thread, run, msg := "alice", uuid.NewString(), uuid.NewString(), uuid.NewString()
	id, artifact := uuid.NewString(), uuid.NewString()
	now := time.Now()
	title := "t"
	cursor := &store.ThreadCursor{CreatedAt: now, ID: uuid.NewString()}
	userMsg := store.Message{ID: msg, ThreadID: thread, Role: "user", Content: "hi"}
	entry := func() *store.MemoryEntry {
		return &store.MemoryEntry{
			ID: id, UserLogin: login, Path: "memories/semantic/facts/a.md", Title: "A", Content: "c",
			Status: store.MemoryActive, ContentHash: "h", Metadata: map[string]any{},
		}
	}
	limits := store.MemoryLimits{Active: 10, Pending: 10}

	calls := []struct {
		name string
		call func() error
	}{
		{"Ping", func() error { return s.Ping(ctx) }},
		{"CreateThread", func() error { _, err := s.CreateThread(ctx, login, "t"); return err }},
		{"GetThread", func() error { _, err := s.GetThread(ctx, login, thread); return err }},
		{"ListThreads", func() error { _, err := s.ListThreads(ctx, login, 10, nil); return err }},
		{"ListThreads cursor", func() error { _, err := s.ListThreads(ctx, login, 10, cursor); return err }},
		{"UpdateThread", func() error {
			_, err := s.UpdateThread(ctx, login, thread, store.ThreadPatch{Title: &title})
			return err
		}},
		{"DeleteThread", func() error { _, err := s.DeleteThread(ctx, login, thread); return err }},
		{"CreateRun", func() error {
			_, _, err := s.CreateRun(ctx, store.NewRun{RunID: run, ThreadID: thread, UserLogin: login, Model: "m", Message: userMsg})
			return err
		}},
		{"GetRun", func() error { _, err := s.GetRun(ctx, run); return err }},
		{"GetUserRun", func() error { _, err := s.GetUserRun(ctx, login, run); return err }},
		{"ClaimRun", func() error { _, err := s.ClaimRun(ctx, run, "rt"); return err }},
		{"Heartbeat", func() error { _, _, err := s.Heartbeat(ctx, run, "rt"); return err }},
		{"RequestCancel", func() error { _, err := s.RequestCancel(ctx, run); return err }},
		{"CancelQueuedRun", func() error { _, err := s.CancelQueuedRun(ctx, run); return err }},
		{"FinishRun", func() error { return s.FinishRun(ctx, run, "rt", "completed", "", "") }},
		{"ListQueuedRunsOlderThan", func() error { _, err := s.ListQueuedRunsOlderThan(ctx, time.Minute); return err }},
		{"InterruptStaleRuns", func() error { _, err := s.InterruptStaleRuns(ctx, time.Minute); return err }},
		{"InterruptStaleRuns alive", func() error {
			_, err := s.InterruptStaleRuns(ctx, time.Minute, func(*store.Run) bool { return false })
			return err
		}},
		{"ListMessages", func() error { _, err := s.ListMessages(ctx, thread); return err }},
		{"AppendMessages", func() error { m := userMsg; return s.AppendMessages(ctx, &m) }},
		{"AppendEvents", func() error {
			return s.AppendEvents(ctx, run, []store.Event{{Type: "TEXT", Data: json.RawMessage(`{"type":"TEXT"}`)}})
		}},
		{"ListEvents", func() error { _, err := s.ListEvents(ctx, run, 0, 10); return err }},
		{"ThreadSandbox", func() error { _, err := s.ThreadSandbox(ctx, thread); return err }},
		{"SetThreadSandbox", func() error { return s.SetThreadSandbox(ctx, thread, "", "sb") }},
		{"CountUserSandboxes", func() error { _, err := s.CountUserSandboxes(ctx, login); return err }},
		{"LastEvent", func() error { _, _, err := s.LastEvent(ctx, run); return err }},
		{"SetRunOutcome", func() error { return s.SetRunOutcome(ctx, run, "failed", "code", "msg") }},

		{"CreateApproval", func() error {
			return s.CreateApproval(ctx, &store.Approval{ID: id, ThreadID: thread, RunID: run, ToolCallID: "tc", ToolName: "bash", Status: "pending"})
		}},
		{"GetApproval", func() error { _, err := s.GetApproval(ctx, id); return err }},
		{"ResumedApproval", func() error { _, err := s.ResumedApproval(ctx, run); return err }},
		{"CancelRunApprovals", func() error { return s.CancelRunApprovals(ctx, run) }},
		{"CancelPendingApproval", func() error { _, err := s.CancelPendingApproval(ctx, run, "cancelled"); return err }},
		{"ExpireApprovals", func() error { _, err := s.ExpireApprovals(ctx, time.Minute, thread); return err }},
		{"ReopenApproval", func() error { _, err := s.ReopenApproval(ctx, id); return err }},
		{"LatestExpiredApproval", func() error { _, err := s.LatestExpiredApproval(ctx, thread); return err }},
		{"SetThreadInteractionMode", func() error { return s.SetThreadInteractionMode(ctx, login, thread, "plan") }},
		{"RunInteractionMode", func() error { _, err := s.RunInteractionMode(ctx, run); return err }},

		{"RunSkills", func() error { _, err := s.RunSkills(ctx, run); return err }},
		{"PutArtifactVersion", func() error {
			_, _, err := s.PutArtifactVersion(ctx, store.PutArtifact{
				ThreadID: thread, UserLogin: login, Path: "/workspace/a.txt", Name: "a.txt", MimeType: "text/plain",
				Kind: "file", SizeBytes: 1, SHA256: "abc", RunID: run, Mtime: 1, SandboxID: "sb",
			})
			return err
		}},
		{"MarkArtifactDeleted", func() error { _, err := s.MarkArtifactDeleted(ctx, artifact, run, "sb", now); return err }},
		{"MarkArtifactsMaterialized", func() error { return s.MarkArtifactsMaterialized(ctx, "sb", []string{artifact}) }},
		{"MarkArtifactsRestoreSkipped", func() error { return s.MarkArtifactsRestoreSkipped(ctx, []string{artifact}) }},
		{"Clock", func() error { _, err := s.Clock(ctx); return err }},
		{"ListArtifacts", func() error { _, err := s.ListArtifacts(ctx, thread); return err }},
		{"GetArtifact", func() error { _, err := s.GetArtifact(ctx, artifact); return err }},
		{"ListArtifactVersions", func() error { _, err := s.ListArtifactVersions(ctx, artifact); return err }},
		{"GetArtifactVersion", func() error { _, err := s.GetArtifactVersion(ctx, artifact, 1); return err }},
		{"PutBlob", func() error { return s.PutBlob(ctx, "k", []byte("x")) }},
		{"GetBlob", func() error { _, err := s.GetBlob(ctx, "k"); return err }},
		{"DeleteBlob", func() error { return s.DeleteBlob(ctx, "k") }},
		{"HasBlob", func() error { _, err := s.HasBlob(ctx, "k"); return err }},
		{"AppendRunArtifacts", func() error {
			return s.AppendRunArtifacts(ctx, run, []any{map[string]any{"artifactId": artifact, "version": 1}})
		}},
		{"AppendToolVerification", func() error {
			return s.AppendToolVerification(ctx, thread, "tc", []any{map[string]any{"ok": true}})
		}},
		{"QueueBlob", func() error { return s.QueueBlob(ctx, "k") }},

		{"SearchThreads", func() error { _, err := s.SearchThreads(ctx, login, "needle", 10, nil); return err }},
		{"SearchThreads cursor", func() error { _, err := s.SearchThreads(ctx, login, "needle", 10, cursor); return err }},
		{"GetThreadSummary", func() error { _, err := s.GetThreadSummary(ctx, thread); return err }},
		{"SetThreadSummary", func() error {
			return s.SetThreadSummary(ctx, thread, store.ThreadSummary{Text: "s", ThroughPosition: 3})
		}},
		{"SetThreadPlan", func() error {
			return s.SetThreadPlan(ctx, thread, []store.PlanItem{{ID: "1", Title: "a", Status: "pending"}})
		}},
		{"SetPendingQuestion set", func() error {
			return s.SetPendingQuestion(ctx, thread, &store.PendingQuestion{InterruptID: "i", RunID: run, ToolCallID: "tc", Question: "?"})
		}},
		{"SetPendingQuestion clear", func() error { return s.SetPendingQuestion(ctx, thread, nil) }},
		{"SetGeneratedTitle", func() error { _, err := s.SetGeneratedTitle(ctx, thread, "title"); return err }},
		{"PutGoal", func() error { _, err := s.PutGoal(ctx, login, thread, "ship it", 5); return err }},
		{"GetGoal", func() error { _, err := s.GetGoal(ctx, login, thread); return err }},
		{"ClearGoal", func() error { return s.ClearGoal(ctx, login, thread) }},
		{"ThreadGoal", func() error { _, err := s.ThreadGoal(ctx, thread); return err }},
		{"UpdateGoal", func() error {
			_, err := s.UpdateGoal(ctx, thread, store.GoalUpdate{ID: id, Verdict: store.GoalVerdict{Met: true, At: now}, Status: "met"})
			return err
		}},
		{"SetFeedback", func() error { return s.SetFeedback(ctx, login, msg, store.Feedback{Sentiment: "up"}) }},
		{"DeleteFeedback", func() error { return s.DeleteFeedback(ctx, login, msg) }},
		{"ListFeedback", func() error { _, err := s.ListFeedback(ctx, thread); return err }},
		{"RecordCompaction", func() error {
			return s.RecordCompaction(ctx, store.Compaction{ID: id, ThreadID: thread, RunID: run, AfterMessageID: msg, Trigger: "auto", TokensBefore: 2, TokensAfter: 1})
		}},
		{"ListCompactions", func() error { _, err := s.ListCompactions(ctx, thread); return err }},
		{"SupersedeFrom", func() error { return s.SupersedeFrom(ctx, login, thread, msg) }},

		{"AdvisoryLock", func() error {
			release, err := s.AdvisoryLock(ctx, "gc", time.Second)
			if release != nil {
				release()
			}
			return err
		}},
		{"PurgeDeletedThreads", func() error { _, err := s.PurgeDeletedThreads(ctx, time.Hour, 10); return err }},
		{"TouchBlob", func() error { return s.TouchBlob(ctx, "k") }},
		{"BlobCandidates", func() error { _, err := s.BlobCandidates(ctx, time.Hour, 10); return err }},
		{"CollectBlob", func() error {
			_, err := s.CollectBlob(ctx, "k", time.Hour, func(context.Context) error { return nil })
			return err
		}},
		{"CollectBlobs", func() error { _, err := s.CollectBlobs(ctx, []string{"k"}, time.Hour); return err }},

		{"MemoryEnabled", func() error { _, err := s.MemoryEnabled(ctx, login); return err }},
		{"SetMemoryEnabled", func() error { return s.SetMemoryEnabled(ctx, login, true) }},
		{"ListMemoryEntries", func() error { _, err := s.ListMemoryEntries(ctx, login); return err }},
		{"ListMemoryEntries statuses", func() error {
			_, err := s.ListMemoryEntries(ctx, login, store.MemoryActive, store.MemoryPending)
			return err
		}},
		{"GetMemoryEntry", func() error { _, err := s.GetMemoryEntry(ctx, login, id); return err }},
		{"GetMemoryEntryByPath", func() error { _, err := s.GetMemoryEntryByPath(ctx, login, "memories/semantic/facts/a.md"); return err }},
		{"CreateMemoryEntry", func() error { _, err := s.CreateMemoryEntry(ctx, entry(), limits); return err }},
		{"UpdateMemoryEntry", func() error { _, err := s.UpdateMemoryEntry(ctx, entry(), limits); return err }},
		{"DeleteMemoryEntry", func() error { return s.DeleteMemoryEntry(ctx, login, id, store.MemoryActive) }},
		{"ListMemoryUsers", func() error { _, err := s.ListMemoryUsers(ctx); return err }},
		{"PurgeDeletedMemoryEntries", func() error { _, err := s.PurgeDeletedMemoryEntries(ctx, now); return err }},
		{"PutMemoryEmbeddings", func() error {
			return s.PutMemoryEmbeddings(ctx, login, []store.EntryEmbedding{{EntryID: id, Model: "m", ContentHash: "h", Vec: []float32{1, 2}}})
		}},
		{"GetMemoryEmbeddings", func() error { _, err := s.GetMemoryEmbeddings(ctx, login); return err }},

		{"MarkThreadSeen", func() error { return s.MarkThreadSeen(ctx, login, thread) }},
		{"CountUnreadThreads", func() error { _, err := s.CountUnreadThreads(ctx, login); return err }},
		{"SearchUnreadThreads", func() error { _, err := s.SearchUnreadThreads(ctx, login, "needle", 10, nil); return err }},
		{"SearchUnreadThreads cursor", func() error { _, err := s.SearchUnreadThreads(ctx, login, "", 10, cursor); return err }},
		{"SwapThreadSandbox", func() error { _, err := s.SwapThreadSandbox(ctx, thread, "old", "new"); return err }},
		{"UserSandboxes", func() error { _, err := s.UserSandboxes(ctx, login); return err }},

		{"WorkspaceGit", func() error { _, err := s.WorkspaceGit(ctx, thread); return err }},
		{"PutWorkspaceGit", func() error {
			return s.PutWorkspaceGit(ctx, store.WorkspaceGitArchive{ThreadID: thread, BlobKey: "workspace-git/" + thread + "/" + id + ".tar.gz", Head: "abc", SizeBytes: 1})
		}},
		{"WorkspaceGitIssue", func() error { _, err := s.WorkspaceGitIssue(ctx, thread); return err }},
		{"SetWorkspaceGitIssue", func() error {
			return s.SetWorkspaceGitIssue(ctx, store.WorkspaceGitIssue{ThreadID: thread, Reason: store.GitIssueFailed, Head: "abc"})
		}},
		{"ClearWorkspaceGitIssue", func() error { return s.ClearWorkspaceGitIssue(ctx, thread) }},
	}
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			// Act
			err := tc.call()

			// Assert
			if err == nil {
				t.Fatal("err = nil, want the database failure")
			}
			for _, sentinel := range domainSentinels {
				if errors.Is(err, sentinel) {
					t.Fatalf("err = %v, a database failure was reported as %q", err, sentinel)
				}
			}
		})
	}
}
