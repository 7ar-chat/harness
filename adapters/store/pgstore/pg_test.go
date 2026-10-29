package pgstore_test

import (
	"context"
	"errors"
	"fmt"
	"github.com/GhaziBenDahmane/harness/adapters/store/pgstore"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GhaziBenDahmane/harness/adapters/store/pgstore/migrate"
	"github.com/GhaziBenDahmane/harness/core/store"
)

// newPG migrates a fresh schema of HARNESS_TEST_DATABASE_URL; skipped when unset.
func newPG(t *testing.T) *pgstore.PG {
	t.Helper()
	s, _ := newPGPool(t)
	return s
}

// newPGPool is newPG with the pool of its schema (to age rows in tests).
func newPGPool(t *testing.T) (*pgstore.PG, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("HARNESS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HARNESS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("work_test_%d", time.Now().UnixNano())
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		pool.Close()
	})
	if err := migrate.Run(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Run(ctx, pool, schema); err != nil {
		t.Fatalf("migrations are not idempotent: %v", err)
	}
	return pgstore.NewPG(pool), pool
}

func newRun(threadID, content string) store.NewRun {
	return store.NewRun{RunID: uuid.NewString(), ThreadID: threadID, UserLogin: "alice", Model: "m",
		Message: store.Message{Role: store.RoleUser, Content: content}, AutoTitle: content}
}

func TestPGThreadsAreScopedToTheirOwner(t *testing.T) {
	// Threads are only visible to their owner and disappear when deleted.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "")

	// Act
	_, otherErr := s.GetThread(ctx, "bob", th.ID)
	_, _ = s.DeleteThread(ctx, "alice", th.ID)
	_, deletedErr := s.GetThread(ctx, "alice", th.ID)

	// Assert
	if !errors.Is(otherErr, store.ErrNotFound) || !errors.Is(deletedErr, store.ErrNotFound) {
		t.Fatalf("other %v deleted %v", otherErr, deletedErr)
	}
}

func TestPGOneActiveRunPerThreadAndAutoTitle(t *testing.T) {
	// CreateRun stores the message, titles an untitled thread and refuses a second active run.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "")

	// Act
	run, msg, err := s.CreateRun(ctx, newRun(th.ID, "Plot sales"))
	_, _, again := s.CreateRun(ctx, newRun(th.ID, "again"))

	// Assert
	got, _ := s.GetThread(ctx, "alice", th.ID)
	if err != nil || msg.Position == 0 || msg.RunID != run.ID || !errors.Is(again, store.ErrRunActive) {
		t.Fatalf("err %v again %v msg %+v", err, again, msg)
	}
	if got.Title != "Plot sales" || got.ActiveRun == nil || got.ActiveRun.ID != run.ID || got.ActiveRun.Status != "queued" {
		t.Fatalf("thread %+v", got)
	}
}

func TestPGRunLifecycle(t *testing.T) {
	// A run is claimed once, heartbeats while owned, finishes, and stale runs are interrupted.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "")
	run, _, _ := s.CreateRun(ctx, newRun(th.ID, "x"))

	// Act
	claimed, err := s.ClaimRun(ctx, run.ID, "rt-1")
	_, twice := s.ClaimRun(ctx, run.ID, "rt-2")
	owned, _, _ := s.Heartbeat(ctx, run.ID, "rt-1")
	notOwner, _, _ := s.Heartbeat(ctx, run.ID, "rt-2")
	_, _ = s.RequestCancel(ctx, run.ID)
	_, cancelRequested, _ := s.Heartbeat(ctx, run.ID, "rt-1")
	stale, _ := s.InterruptStaleRuns(ctx, -time.Second)
	afterInterrupt, _ := s.GetRun(ctx, run.ID)
	_ = s.FinishRun(ctx, run.ID, "rt-1", store.StatusCompleted, "", "")
	final, _ := s.GetRun(ctx, run.ID)

	// Assert
	if err != nil || claimed.Status != "running" || !errors.Is(twice, store.ErrNotClaimable) || !owned || notOwner || !cancelRequested {
		t.Fatalf("claim %v twice %v owned %v notOwner %v cancel %v", err, twice, owned, notOwner, cancelRequested)
	}
	if len(stale) != 1 || afterInterrupt.Status != "interrupted" || afterInterrupt.ErrorCode != "interrupted" || final.Status != "interrupted" {
		t.Fatalf("stale %d status %s final %s", len(stale), afterInterrupt.Status, final.Status)
	}
}

func TestPGEventsAreIdempotentAndTrackLastSeq(t *testing.T) {
	// Appending events is idempotent per seq and advances work_runs.last_seq.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "")
	run, _, _ := s.CreateRun(ctx, newRun(th.ID, "x"))
	ev := func(seq int64) store.Event {
		return store.Event{Seq: seq, Type: "STEP_STARTED", Data: []byte(fmt.Sprintf(`{"type":"STEP_STARTED","n":%d}`, seq))}
	}

	// Act
	_ = s.AppendEvents(ctx, run.ID, []store.Event{ev(1), ev(2)})
	err := s.AppendEvents(ctx, run.ID, []store.Event{ev(2), ev(3)})
	evs, _ := s.ListEvents(ctx, run.ID, 1, 0)

	// Assert
	got, _ := s.GetRun(ctx, run.ID)
	if err != nil || len(evs) != 2 || evs[0].Seq != 2 || evs[1].Seq != 3 || got.LastSeq != 3 {
		t.Fatalf("err %v evs %+v lastSeq %d", err, evs, got.LastSeq)
	}
}

func TestPGListThreadsPaginatesPinnedFirst(t *testing.T) {
	// Pages follow (pinned, createdAt, id) descending without overlap.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	var ids []string
	for i := 0; i < 4; i++ {
		th, _ := s.CreateThread(ctx, "alice", fmt.Sprint(i))
		ids = append(ids, th.ID)
	}
	pin := true
	_, _ = s.UpdateThread(ctx, "alice", ids[0], store.ThreadPatch{Pinned: &pin})

	// Act
	p1, _ := s.ListThreads(ctx, "alice", 2, nil)
	last := p1[len(p1)-1]
	p2, _ := s.ListThreads(ctx, "alice", 2, &store.ThreadCursor{Pinned: last.PinnedAt != nil, CreatedAt: last.CreatedAt, ID: last.ID})

	// Assert
	if len(p1) != 2 || len(p2) != 2 || p1[0].ID != ids[0] || p1[1].ID != ids[3] || p2[0].ID != ids[2] || p2[1].ID != ids[1] {
		t.Fatalf("p1 %v p2 %v", titles(p1), titles(p2))
	}
}

func titles(ts []*store.Thread) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Title)
	}
	return out
}

func TestPGSandboxBookkeeping(t *testing.T) {
	// Sandbox ids are set with compare-and-set and counted per user.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	a, _ := s.CreateThread(ctx, "alice", "")
	b, _ := s.CreateThread(ctx, "alice", "")

	// Act
	_ = s.SetThreadSandbox(ctx, a.ID, "", "sbx-1")
	_ = s.SetThreadSandbox(ctx, a.ID, "", "sbx-2") // stale expectation: ignored
	_ = s.SetThreadSandbox(ctx, b.ID, "", "sbx-3")
	n, _ := s.CountUserSandboxes(ctx, "alice")
	id, _ := s.ThreadSandbox(ctx, a.ID)

	// Assert
	if n != 2 || id != "sbx-1" {
		t.Fatalf("count %d id %s", n, id)
	}
}

func TestPGSanitizesNULAndInvalidUTF8(t *testing.T) {
	// NUL bytes and invalid UTF-8 in messages, titles and event payloads are replaced instead of failing the insert.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, err := s.CreateThread(ctx, "alice", "t\x00itle")
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := s.CreateRun(ctx, newRun(th.ID, "hello\x00"))
	if err != nil {
		t.Fatal(err)
	}

	// Act
	msgErr := s.AppendMessages(ctx, &store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleTool, ToolCallID: "c1",
		Content: "a\x00b\xff", Error: "e\x00", Metadata: map[string]any{"k": "v\x00"},
		ToolCalls: []store.ToolCall{{ID: "c\x00", Type: "function", Function: store.ToolCallFunction{Name: "n", Arguments: "{\"a\":\"\\u0000\"}"}}}})
	evErr := s.AppendEvents(ctx, run.ID, []store.Event{{Seq: 1, Type: "TOOL_CALL_RESULT", Data: []byte(`{"type":"TOOL_CALL_RESULT","content":"x\u0000y"}`)}})

	// Assert
	msgs, _ := s.ListMessages(ctx, th.ID)
	evs, _ := s.ListEvents(ctx, run.ID, 0, 0)
	if msgErr != nil || evErr != nil || len(msgs) != 2 || len(evs) != 1 {
		t.Fatalf("msg %v ev %v msgs %d evs %d", msgErr, evErr, len(msgs), len(evs))
	}
	if msgs[1].Content != "a\ufffdb\ufffd" || msgs[0].Content != "hello\ufffd" {
		t.Fatalf("contents %q %q", msgs[0].Content, msgs[1].Content)
	}
}

func TestPGMessageIDsAreScopedToTheirThread(t *testing.T) {
	// Two threads may use the same client message id; reuse within a thread is a conflict.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	a, _ := s.CreateThread(ctx, "alice", "")
	b, _ := s.CreateThread(ctx, "bob", "")
	ra := newRun(a.ID, "x")
	ra.Message.ID = "m-1"
	rb := newRun(b.ID, "y")
	rb.Message.ID = "m-1"

	// Act
	_, _, errA := s.CreateRun(ctx, ra)
	_, _, errB := s.CreateRun(ctx, rb)
	dup := s.AppendMessages(ctx, &store.Message{ID: "m-1", ThreadID: a.ID, Role: store.RoleUser, Content: "again"})

	// Assert
	if errA != nil || errB != nil || !errors.Is(dup, store.ErrConflict) {
		t.Fatalf("a %v b %v dup %v", errA, errB, dup)
	}
}

func TestPGFinishRunStoresErrorCode(t *testing.T) {
	// A failed run keeps its RUN_ERROR code; a failure without code gets the generic "internal".
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "")
	a, _, _ := s.CreateRun(ctx, newRun(th.ID, "x"))
	_, _ = s.ClaimRun(ctx, a.ID, "rt")
	th2, _ := s.CreateThread(ctx, "alice", "")
	b, _, _ := s.CreateRun(ctx, newRun(th2.ID, "y"))
	_, _ = s.ClaimRun(ctx, b.ID, "rt")

	// Act
	_ = s.FinishRun(ctx, a.ID, "rt", store.StatusFailed, "model_error", "The model failed.")
	_ = s.FinishRun(ctx, b.ID, "rt", store.StatusFailed, "", "")

	// Assert
	ga, _ := s.GetRun(ctx, a.ID)
	gb, _ := s.GetRun(ctx, b.ID)
	if ga.ErrorCode != "model_error" || ga.Error != "The model failed." || gb.ErrorCode != "internal" {
		t.Fatalf("a %+v b %+v", ga, gb)
	}
}

func TestPGMigrateUpgradesDatabaseThatAppliedTheFirstMigration(t *testing.T) {
	// A database that applied the original 0001 gets work_runs.error_code from later migrations (idempotently).
	// Arrange
	dsn := os.Getenv("HARNESS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HARNESS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("work_upgrade_%d", time.Now().UnixNano())
	cfg, _ := pgxpool.ParseConfig(dsn)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); pool.Close() })
	old, _ := os.ReadFile("testdata/0001_initial_applied_before_error_code.sql")
	for _, q := range []string{"CREATE SCHEMA " + schema, string(old),
		"CREATE TABLE work_migrations (id SERIAL PRIMARY KEY, name VARCHAR(255) NOT NULL UNIQUE, applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW())",
		"INSERT INTO work_migrations (name) VALUES ('0001_initial.sql')"} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}

	// Act
	first := migrate.Run(ctx, pool, schema)
	second := migrate.Run(ctx, pool, schema)

	// Assert
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema = $1 AND table_name = 'work_runs' AND column_name = 'error_code'`, schema).Scan(&n)
	if first != nil || second != nil || n != 1 {
		t.Fatalf("first %v second %v error_code columns %d", first, second, n)
	}
}

func TestPGLastEventAndSetRunOutcome(t *testing.T) {
	// LastEvent returns the highest-seq event; SetRunOutcome makes an interrupted row match its log.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "")
	run, _, _ := s.CreateRun(ctx, newRun(th.ID, "x"))
	_, _ = s.ClaimRun(ctx, run.ID, "rt")
	_, _ = s.InterruptStaleRuns(ctx, -time.Hour)
	_ = s.AppendEvents(ctx, run.ID, []store.Event{{Seq: 2, Type: "RUN_FINISHED", Data: []byte(`{"type":"RUN_FINISHED"}`)}, {Seq: 1, Type: "RUN_STARTED", Data: []byte(`{"type":"RUN_STARTED"}`)}})

	// Act
	last, ok, err := s.LastEvent(ctx, run.ID)
	setErr := s.SetRunOutcome(ctx, run.ID, store.StatusCompleted, "", "")

	// Assert
	got, _ := s.GetRun(ctx, run.ID)
	if err != nil || !ok || last.Seq != 2 || last.Type != "RUN_FINISHED" || setErr != nil || got.Status != "completed" || got.ErrorCode != "" {
		t.Fatalf("last %+v ok %v err %v set %v run %+v", last, ok, err, setErr, got)
	}
}
