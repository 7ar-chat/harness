package memstore_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/GhaziBenDahmane/harness/adapters/store/memstore"
	"github.com/GhaziBenDahmane/harness/core/store"
)

// twoTurns returns a store with a thread of two answered turns.
func twoTurns(t *testing.T) (s *memstore.Store, threadID string, userIDs []string) {
	t.Helper()
	ctx := context.Background()
	s = memstore.New()
	th, _ := s.CreateThread(ctx, "alice", "t")
	for _, text := range []string{"first", "second"} {
		id := uuid.NewString()
		run, _, err := s.CreateRun(ctx, store.NewRun{RunID: uuid.NewString(), ThreadID: th.ID, UserLogin: "alice", Model: "m",
			Message: store.Message{ID: id, Role: store.RoleUser, Content: text}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimRun(ctx, run.ID, "rt"); err != nil {
			t.Fatal(err)
		}
		if err := s.AppendMessages(ctx, &store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleAssistant, Content: "re: " + text}); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishRun(ctx, run.ID, "rt", store.StatusCompleted, "", ""); err != nil {
			t.Fatal(err)
		}
		userIDs = append(userIDs, id)
	}
	return s, th.ID, userIDs
}

func TestSupersedeFromHidesTheMessageAndWhatFollows(t *testing.T) {
	// Arrange
	ctx := context.Background()
	s, id, users := twoTurns(t)
	if err := s.SetThreadSummary(ctx, id, store.ThreadSummary{Text: "sum", ThroughPosition: 3}); err != nil {
		t.Fatal(err)
	}

	// Act
	err := s.SupersedeFrom(ctx, "alice", id, users[1])

	// Assert
	msgs, _ := s.ListMessages(ctx, id)
	sum, _ := s.GetThreadSummary(ctx, id)
	if err != nil || len(msgs) != 2 || msgs[1].Content != "re: first" {
		t.Fatalf("err %v, messages %d", err, len(msgs))
	}
	if sum.Text != "" {
		t.Fatalf("the summary covering the hidden messages stays: %+v", sum)
	}
}

func TestSupersedeFromErrors(t *testing.T) {
	// Arrange
	ctx := context.Background()
	s, id, users := twoTurns(t)
	msgs, _ := s.ListMessages(ctx, id)
	if _, _, err := s.CreateRun(ctx, store.NewRun{RunID: uuid.NewString(), ThreadID: id, UserLogin: "alice", Model: "m",
		Message: store.Message{Role: store.RoleUser, Content: "third"}}); err != nil {
		t.Fatal(err)
	}

	// Act
	busy := s.SupersedeFrom(ctx, "alice", id, users[0])
	stranger := s.SupersedeFrom(ctx, "bob", id, users[0])
	other, _ := s.CreateThread(ctx, "alice", "other")
	notUser := s.SupersedeFrom(ctx, "alice", other.ID, msgs[1].ID)

	// Assert
	if !errors.Is(busy, store.ErrRunActive) {
		t.Errorf("a thread with an active run: %v", busy)
	}
	if !errors.Is(stranger, store.ErrNotFound) {
		t.Errorf("another user: %v", stranger)
	}
	if !errors.Is(notUser, store.ErrInvalidReplace) {
		t.Errorf("a message of another thread: %v", notUser)
	}
}
