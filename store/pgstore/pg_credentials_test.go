package pgstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/store"
)

func TestPGRunCredentialsAreScopedToActiveRunsOfTheirUser(t *testing.T) {
	// Credentials are stored for active runs of their user only, refreshed only by a later expiry, and purged once the run ends or they expire.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "t")
	run, _, _ := s.CreateRun(ctx, newRun(th.ID, "q"))
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	cred := store.RunCredential{RunID: run.ID, UserLogin: "alice", Sealed: []byte{1, 2, 3}, ExpiresAt: exp}

	// Act
	stored, err := s.PutRunCredential(ctx, cred, true)
	asBob, _ := s.PutRunCredential(ctx, store.RunCredential{RunID: run.ID, UserLogin: "bob", Sealed: []byte{9}, ExpiresAt: exp.Add(time.Hour)}, true)
	older, _ := s.PutRunCredential(ctx, store.RunCredential{RunID: run.ID, UserLogin: "alice", Sealed: []byte{4}, ExpiresAt: exp.Add(-time.Minute)}, false)
	later, _ := s.PutRunCredential(ctx, store.RunCredential{RunID: run.ID, UserLogin: "alice", Sealed: []byte{5}, ExpiresAt: exp.Add(time.Minute)}, false)
	got, getErr := s.GetRunCredential(ctx, run.ID)
	kept, _ := s.PurgeRunCredentials(ctx)
	finish(t, s, run.ID)
	afterEnd, _ := s.PutRunCredential(ctx, cred, true)
	purged, _ := s.PurgeRunCredentials(ctx)
	_, goneErr := s.GetRunCredential(ctx, run.ID)
	_, badID := s.GetRunCredential(ctx, "not-a-uuid")

	// Assert
	if !stored || err != nil || asBob || older || !later || afterEnd {
		t.Fatalf("stored %v %v bob %v older %v later %v afterEnd %v", stored, err, asBob, older, later, afterEnd)
	}
	if getErr != nil || got.UserLogin != "alice" || len(got.Sealed) != 1 || got.Sealed[0] != 5 || !got.ExpiresAt.Equal(exp.Add(time.Minute)) {
		t.Fatalf("got %+v err %v", got, getErr)
	}
	if kept != 0 || purged != 1 || !errors.Is(goneErr, store.ErrNotFound) || !errors.Is(badID, store.ErrNotFound) {
		t.Fatalf("kept %d purged %d gone %v bad %v", kept, purged, goneErr, badID)
	}
}

func TestPGRunCredentialsFollowTheirRun(t *testing.T) {
	// Expired credentials are purged; deleting the run (thread purge) cascades to its credential.
	// Arrange
	s, pool := newPGPool(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "t")
	run, _, _ := s.CreateRun(ctx, newRun(th.ID, "q"))
	_, _ = s.PutRunCredential(ctx, store.RunCredential{RunID: run.ID, UserLogin: "alice", Sealed: []byte{1}, ExpiresAt: time.Now().Add(time.Hour)}, true)
	_, _ = pool.Exec(ctx, `UPDATE work_run_credentials SET expires_at = now() - interval '1 second'`)

	// Act
	expired, _ := s.PurgeRunCredentials(ctx)
	_, _ = s.PutRunCredential(ctx, store.RunCredential{RunID: run.ID, UserLogin: "alice", Sealed: []byte{1}, ExpiresAt: time.Now().Add(time.Hour)}, true)
	_, _ = pool.Exec(ctx, `DELETE FROM work_runs WHERE id = $1`, run.ID)
	var left int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM work_run_credentials`).Scan(&left)

	// Assert
	if expired != 1 || left != 0 {
		t.Fatalf("expired %d left %d", expired, left)
	}
}
