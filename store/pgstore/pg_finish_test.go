package pgstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/store"
)

func TestPGFinishRunReportsARunTheSweeperTookOver(t *testing.T) {
	// FinishRun of a run the sweeper already interrupted returns ErrNotOwned and leaves the row interrupted.
	// Arrange
	s, pool := newPGPool(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "")
	run, _, _ := s.CreateRun(ctx, newRun(th.ID, "q"))
	_, _ = s.ClaimRun(ctx, run.ID, "rt-1")
	_, _ = pool.Exec(ctx, `UPDATE work_runs SET heartbeat_at = now() - interval '5 minutes' WHERE id = $1`, run.ID)
	_, _ = s.InterruptStaleRuns(ctx, time.Minute)

	// Act
	err := s.FinishRun(ctx, run.ID, "rt-1", store.StatusCompleted, "", "")

	// Assert
	got, _ := s.GetRun(ctx, run.ID)
	if !errors.Is(err, store.ErrNotOwned) || got.Status != store.StatusInterrupted {
		t.Fatalf("err %v status %s", err, got.Status)
	}
}

func TestPGInterruptStaleRunsSparesRunsStillAliveElsewhere(t *testing.T) {
	// A stale-heartbeat run vouched for by the alive check (its runtime heartbeats in Redis) stays running; the others are interrupted.
	// Arrange
	s, pool := newPGPool(t)
	ctx := context.Background()
	var runs []*store.Run
	for i := 0; i < 2; i++ {
		th, _ := s.CreateThread(ctx, "alice", "")
		run, _, _ := s.CreateRun(ctx, newRun(th.ID, "q"))
		_, _ = s.ClaimRun(ctx, run.ID, "rt")
		runs = append(runs, run)
	}
	_, _ = pool.Exec(ctx, `UPDATE work_runs SET heartbeat_at = now() - interval '5 minutes'`)
	alive := func(r *store.Run) bool { return r.ID == runs[0].ID }

	// Act
	stale, err := s.InterruptStaleRuns(ctx, time.Minute, alive)

	// Assert
	kept, _ := s.GetRun(ctx, runs[0].ID)
	if err != nil || len(stale) != 1 || stale[0].ID != runs[1].ID || kept.Status != store.StatusRunning {
		t.Fatalf("err %v stale %d kept %s", err, len(stale), kept.Status)
	}
}

func TestPGInterruptStaleRunsHoldsNoRowLockDuringTheAliveCheck(t *testing.T) {
	// The alive check (a Redis call) runs without row locks: the run's runtime can heartbeat meanwhile, and a run that heartbeated is not interrupted (review round 1).
	// Arrange
	s, pool := newPGPool(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "")
	run, _, _ := s.CreateRun(ctx, newRun(th.ID, "q"))
	_, _ = s.ClaimRun(ctx, run.ID, "rt")
	_, _ = pool.Exec(ctx, `UPDATE work_runs SET heartbeat_at = now() - interval '5 minutes' WHERE id = $1`, run.ID)
	var heartbeatErr error
	alive := func(r *store.Run) bool {
		hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		_, heartbeatErr = pool.Exec(hctx, `UPDATE work_runs SET heartbeat_at = now() WHERE id = $1`, r.ID)
		return false
	}

	// Act
	stale, err := s.InterruptStaleRuns(ctx, time.Minute, alive)

	// Assert
	got, _ := s.GetRun(ctx, run.ID)
	if err != nil || heartbeatErr != nil || len(stale) != 0 || got.Status != store.StatusRunning {
		t.Fatalf("err %v heartbeat %v stale %d status %s", err, heartbeatErr, len(stale), got.Status)
	}
}
