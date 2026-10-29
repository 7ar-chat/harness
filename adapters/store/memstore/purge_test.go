package memstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/GhaziBenDahmane/harness/adapters/store/memstore"
	"github.com/GhaziBenDahmane/harness/core/store"
)

func TestPurgeRemovesWhatThePostgresCascadeRemoves(t *testing.T) {
	// Purging a deleted thread drops its approvals, like Postgres.
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
	_, _ = s.DeleteThread(ctx, "alice", th.ID)
	s.SetThreadDeletedAt(th.ID, time.Now().Add(-48*time.Hour))

	// Act
	n, err := s.PurgeDeletedThreads(ctx, 24*time.Hour, 10)

	// Assert
	_, approvalErr := s.GetApproval(ctx, a.ID)
	if err != nil || n != 1 || !errors.Is(approvalErr, store.ErrNotFound) {
		t.Fatalf("purged %d %v approval %v", n, err, approvalErr)
	}
}
