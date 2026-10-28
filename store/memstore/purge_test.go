package memstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/GhaziBenDahmane/harness/store"
	"github.com/GhaziBenDahmane/harness/store/memstore"
)

func TestPurgeRemovesWhatThePostgresCascadeRemoves(t *testing.T) {
	// Purging a deleted thread drops its approvals and keeps its idea usage without the thread (ON DELETE SET NULL), like Postgres.
	// Arrange
	ctx := context.Background()
	s := memstore.New()
	th, _ := s.CreateThread(ctx, "alice", "t")
	run, _, err := s.CreateRun(ctx, store.NewRun{RunID: uuid.NewString(), ThreadID: th.ID, UserLogin: "alice", Model: "m",
		Message: store.Message{Role: store.RoleUser, Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	a := &store.Approval{ThreadID: th.ID, RunID: run.ID, ToolCallID: "c1", ToolName: "write_file"}
	if err := s.CreateApproval(ctx, a); err != nil {
		t.Fatal(err)
	}
	usage, err := s.RecordIdeaUsage(ctx, store.IdeaUsage{UserLogin: "alice", ThreadID: th.ID, IdeaID: "idea-1", Action: store.IdeaSubmitted, Surface: "composer"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.DeleteThread(ctx, "alice", th.ID)
	s.SetThreadDeletedAt(th.ID, time.Now().Add(-48*time.Hour))

	// Act
	n, err := s.PurgeDeletedThreads(ctx, 24*time.Hour, 10)

	// Assert
	_, approvalErr := s.GetApproval(ctx, a.ID)
	list, _ := s.ListIdeaUsage(ctx, "alice")
	if err != nil || n != 1 || !errors.Is(approvalErr, store.ErrNotFound) {
		t.Fatalf("purged %d %v approval %v", n, err, approvalErr)
	}
	if len(list) != 1 || list[0].ID != usage.ID || list[0].ThreadID != "" {
		t.Fatalf("idea usage %+v", list)
	}
}
