package websearch_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/GhaziBenDahmane/harness/store"
	"github.com/GhaziBenDahmane/harness/store/memstore"
	"github.com/GhaziBenDahmane/harness/websearch"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// jwt builds an unsigned JWT with the given exp (the service never verifies it).
func jwt(sub string, exp time.Time) string {
	enc := base64.RawURLEncoding
	claims, _ := json.Marshal(map[string]any{"sub": sub, "exp": exp.Unix()})
	return enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." + enc.EncodeToString(claims) + ".sig"
}

func testKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func newVault(t *testing.T) (*websearch.Vault, *memstore.Store) {
	t.Helper()
	st := memstore.New()
	s, err := websearch.NewSealer(testKey())
	if err != nil {
		t.Fatal(err)
	}
	return &websearch.Vault{Store: st, Sealer: s}, st
}

// activeRun creates a queued run of login on a new thread.
func activeRun(t *testing.T, st *memstore.Store, login string) *store.Run {
	t.Helper()
	ctx := context.Background()
	th, err := st.CreateThread(ctx, login, "t")
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := st.CreateRun(ctx, store.NewRun{RunID: uuid.NewString(), ThreadID: th.ID, UserLogin: login, Model: "m",
		Message: store.Message{Role: store.RoleUser, Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func finishRun(t *testing.T, st *memstore.Store, runID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.ClaimRun(ctx, runID, "rt"); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishRun(ctx, runID, "rt", store.StatusCompleted, "", ""); err != nil {
		t.Fatal(err)
	}
}
