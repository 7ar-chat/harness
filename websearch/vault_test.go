package websearch_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/GhaziBenDahmane/harness/agent"
	"github.com/GhaziBenDahmane/harness/runtime"
	"github.com/GhaziBenDahmane/harness/store"
	"github.com/GhaziBenDahmane/harness/websearch"
)

func TestVaultStoresTokensEncryptedPerRunAndUser(t *testing.T) {
	// The stored row is encrypted; only the run's user gets the token back; terminal runs and other users' runs store nothing.
	// Arrange
	v, st := newVault(t)
	ctx := context.Background()
	run := activeRun(t, st, "alice")
	done := activeRun(t, st, "alice")
	finishRun(t, st, done.ID)
	tok := websearch.Token{Value: "alice-token", ExpiresAt: time.Now().Add(time.Hour)}

	// Act
	stored, err := v.Put(ctx, run.ID, "alice", tok, true)
	asBob, _ := v.Put(ctx, run.ID, "bob", websearch.Token{Value: "bob-token", ExpiresAt: time.Now().Add(2 * time.Hour)}, true)
	onDone, _ := v.Put(ctx, done.ID, "alice", tok, true)
	got, getErr := v.Token(ctx, run.ID, "alice")
	_, bobErr := v.Token(ctx, run.ID, "bob")
	row, _ := st.GetRunCredential(ctx, run.ID)

	// Assert
	if !stored || err != nil || asBob || onDone {
		t.Fatalf("stored %v err %v asBob %v onDone %v", stored, err, asBob, onDone)
	}
	if getErr != nil || got.Value != "alice-token" || !errors.Is(bobErr, websearch.ErrNoToken) {
		t.Fatalf("got %v err %v bob %v", got.Value, getErr, bobErr)
	}
	if bytes.Contains(row.Sealed, []byte("alice-token")) || row.UserLogin != "alice" {
		t.Fatalf("row %+v", row)
	}
}

func TestVaultRefreshOnlyKeepsTheLaterExpiry(t *testing.T) {
	// A steer refresh (replace=false) replaces the token only when it expires later; expired tokens are reported as such.
	// Arrange
	v, st := newVault(t)
	ctx := context.Background()
	run := activeRun(t, st, "alice")
	base := time.Now()
	_, _ = v.Put(ctx, run.ID, "alice", websearch.Token{Value: "first", ExpiresAt: base.Add(time.Hour)}, true)

	// Act
	older, _ := v.Put(ctx, run.ID, "alice", websearch.Token{Value: "older", ExpiresAt: base.Add(30 * time.Minute)}, false)
	afterOlder, _ := v.Token(ctx, run.ID, "alice")
	newer, _ := v.Put(ctx, run.ID, "alice", websearch.Token{Value: "newer", ExpiresAt: base.Add(2 * time.Hour)}, false)
	afterNewer, _ := v.Token(ctx, run.ID, "alice")
	_, expiredPut := v.Put(ctx, run.ID, "alice", websearch.Token{Value: "dead", ExpiresAt: base.Add(-time.Minute)}, true)
	v.Now = func() time.Time { return base.Add(3 * time.Hour) }
	_, expiredGet := v.Token(ctx, run.ID, "alice")

	// Assert
	if older || afterOlder.Value != "first" || !newer || afterNewer.Value != "newer" {
		t.Fatalf("older %v %q newer %v %q", older, afterOlder.Value, newer, afterNewer.Value)
	}
	if !errors.Is(expiredPut, websearch.ErrTokenExpired) || !errors.Is(expiredGet, websearch.ErrTokenExpired) {
		t.Fatalf("expired put %v get %v", expiredPut, expiredGet)
	}
}

func TestVaultRowFromAnotherRunDoesNotDecrypt(t *testing.T) {
	// A sealed token copied to another run's row cannot be opened (bound to run id + login).
	// Arrange
	v, st := newVault(t)
	ctx := context.Background()
	r1 := activeRun(t, st, "alice")
	r2 := activeRun(t, st, "alice")
	_, _ = v.Put(ctx, r1.ID, "alice", websearch.Token{Value: "t1", ExpiresAt: time.Now().Add(time.Hour)}, true)
	row, _ := st.GetRunCredential(ctx, r1.ID)
	row.RunID = r2.ID
	_, _ = st.PutRunCredential(ctx, *row, true)

	// Act
	_, err := v.Token(ctx, r2.ID, "alice")

	// Assert
	if !errors.Is(err, websearch.ErrNoToken) {
		t.Fatalf("err %v", err)
	}
}

func TestHookClearsTheTokenAndHandsItToTheGoalRun(t *testing.T) {
	// At run end the token is deleted; the next run of the same thread and user (goal turn) inherits it; ended runs' leftovers are purged.
	// Arrange
	v, st := newVault(t)
	ctx := context.Background()
	run := activeRun(t, st, "alice")
	exp := time.Now().Add(time.Hour)
	_, _ = v.Put(ctx, run.ID, "alice", websearch.Token{Value: "alice-token", ExpiresAt: exp}, true)
	leftover := activeRun(t, st, "bob")
	_, _ = v.Put(ctx, leftover.ID, "bob", websearch.Token{Value: "bob-token", ExpiresAt: exp}, true)
	finishRun(t, st, leftover.ID) // ended without the hook (e.g. interrupted by the sweeper)
	finishRun(t, st, run.ID)
	next, _, err := st.CreateRun(ctx, store.NewRun{RunID: uuid.NewString(), ThreadID: run.ThreadID, UserLogin: "alice", Model: "m",
		Message: store.Message{Role: store.RoleUser, Content: "continue"}})
	if err != nil {
		t.Fatal(err)
	}
	h := &websearch.Hook{Vault: v, Threads: st}

	// Act
	h.Finishing(ctx, &runtime.Finish{Run: run, Status: store.StatusCompleted, Result: agent.Result{}})(ctx)

	// Assert
	_, oldErr := st.GetRunCredential(ctx, run.ID)
	_, leftErr := st.GetRunCredential(ctx, leftover.ID)
	got, nextErr := v.Token(ctx, next.ID, "alice")
	if !errors.Is(oldErr, store.ErrNotFound) || !errors.Is(leftErr, store.ErrNotFound) {
		t.Fatalf("old %v leftover %v", oldErr, leftErr)
	}
	if nextErr != nil || got.Value != "alice-token" {
		t.Fatalf("next %q %v", got.Value, nextErr)
	}
}

func TestHookHandoffGivesTheTokenToTheGoalRunOfTheSameUserAndThread(t *testing.T) {
	// Handoff (called before the goal run is enqueued) copies the token to the next run of the same thread and user only.
	// Arrange
	v, st := newVault(t)
	ctx := context.Background()
	run := activeRun(t, st, "alice")
	_, _ = v.Put(ctx, run.ID, "alice", websearch.Token{Value: "alice-token", ExpiresAt: time.Now().Add(time.Hour)}, true)
	finishRun(t, st, run.ID)
	next, _, err := st.CreateRun(ctx, store.NewRun{RunID: uuid.NewString(), ThreadID: run.ThreadID, UserLogin: "alice", Model: "m",
		Message: store.Message{Role: store.RoleUser, Content: "continue"}})
	if err != nil {
		t.Fatal(err)
	}
	other := activeRun(t, st, "alice")
	h := &websearch.Hook{Vault: v, Threads: st}

	// Act
	h.Handoff(ctx, run, next)
	h.Handoff(ctx, run, other)

	// Assert
	got, nextErr := v.Token(ctx, next.ID, "alice")
	_, otherErr := v.Token(ctx, other.ID, "alice")
	if nextErr != nil || got.Value != "alice-token" || otherErr == nil {
		t.Fatalf("next %q %v other %v", got.Value, nextErr, otherErr)
	}
}

func TestCapturerStoresTheResolvedTokenAndIgnoresFailures(t *testing.T) {
	// The api capture stores the caller's token; no token or an expired one stores nothing and never fails.
	// Arrange
	v, st := newVault(t)
	c := &websearch.Capturer{Broker: &websearch.Broker{Mode: "legacy"}, Vault: v}
	ctx := context.Background()
	withToken := activeRun(t, st, "alice")
	without := activeRun(t, st, "alice")
	expired := activeRun(t, st, "alice")
	tok := jwt("alice", time.Now().Add(time.Hour))

	// Act
	c.CaptureRun(ctx, withToken.ID, "alice", tok)
	c.CaptureRun(ctx, without.ID, "alice", "")
	c.CaptureRun(ctx, expired.ID, "alice", jwt("alice", time.Now().Add(-time.Minute)))
	c.RefreshRun(ctx, without.ID, "alice", tok)

	// Assert
	got, err := v.Token(ctx, withToken.ID, "alice")
	refreshed, refreshErr := v.Token(ctx, without.ID, "alice")
	_, expErr := v.Token(ctx, expired.ID, "alice")
	if err != nil || got.Value != tok || refreshErr != nil || refreshed.Value != tok || !errors.Is(expErr, websearch.ErrNoToken) {
		t.Fatalf("got %v %v refreshed %v %v expired %v", got.Value, err, refreshed.Value, refreshErr, expErr)
	}
}
