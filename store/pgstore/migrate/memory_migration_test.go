package migrate

import (
	"context"
	"fmt"
	"github.com/GhaziBenDahmane/harness/store/pgstore"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GhaziBenDahmane/harness/memory"
)

// migrationsBefore is the embedded migrations older than name.
func migrationsBefore(t *testing.T, name string) fs.FS {
	t.Helper()
	out := fstest.MapFS{}
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() >= name {
			continue
		}
		b, err := fs.ReadFile(migrationFS, "migrations/"+e.Name())
		if err != nil {
			t.Fatal(err)
		}
		out["migrations/"+e.Name()] = &fstest.MapFile{Data: b}
	}
	return out
}

func TestMemoryMigrationMovesTheFormerFactsWithoutLoss(t *testing.T) {
	// 0010 copies every work_memories row into work_memory_entries (semantic fact, same user, text, status, thread, date; id mem_<uuid hex>) and empties the old table (kept for the previous release).
	dsn := os.Getenv("WORK_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WORK_TEST_DATABASE_URL not set")
	}
	// Arrange
	ctx := context.Background()
	schema := fmt.Sprintf("work_test_mig_%d", time.Now().UnixNano())
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
	if err := run(ctx, pool, schema, migrationsBefore(t, "0010")); err != nil {
		t.Fatal(err)
	}
	thread := "6f1c2d3e-4b5a-4c6d-8e7f-9a0b1c2d3e4f"
	if _, err := pool.Exec(ctx, `INSERT INTO work_threads (id, user_login, title) VALUES ($1, 'alice', 't')`, thread); err != nil {
		t.Fatal(err)
	}
	rows := []struct{ id, login, text, status string }{
		{"0b0c8f8e-3a5b-4c1d-9e2f-1a2b3c4d5e6f", "alice", "Prefers answers in French", "active"},
		{"1b0c8f8e-3a5b-4c1d-9e2f-1a2b3c4d5e6f", "alice", "Always email results\nto the team", "pending"},
		{"2b0c8f8e-3a5b-4c1d-9e2f-1a2b3c4d5e6f", "bob", "Works in the risk team", "active"},
	}
	for _, r := range rows {
		if _, err := pool.Exec(ctx, `INSERT INTO work_memories (id, user_login, text, source_thread_id, status, created_at)
			VALUES ($1, $2, $3, $4, $5, '2026-01-15T10:00:00Z')`, r.id, r.login, r.text, thread, r.status); err != nil {
			t.Fatal(err)
		}
	}

	// Act
	err = run(ctx, pool, schema, migrationFS)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		id := "mem_" + strings.ReplaceAll(r.id, "-", "")
		var login, path, title, content, status, source, src, trigger, legacy string
		var created time.Time
		if err := pool.QueryRow(ctx, `SELECT user_login, path, title, content, status, source, coalesce(source_thread_id::text, ''),
				metadata->>'trigger', metadata->>'legacy_id', created_at FROM work_memory_entries WHERE id = $1`, id).
			Scan(&login, &path, &title, &content, &status, &source, &src, &trigger, &legacy, &created); err != nil {
			t.Fatalf("%s not migrated: %v", r.id, err)
		}
		if login != r.login || content != r.text || status != r.status || source != "legacy" || src != thread || trigger != "explicit_remember" ||
			legacy != r.id || path != "memories/semantic/facts/2026-01-15-"+id+".md" || !created.Equal(time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)) {
			t.Fatalf("%s migrated as %s %s %q %s %s %s %s %s %v", r.id, login, path, content, status, source, src, trigger, legacy, created)
		}
		if want := strings.SplitN(r.text, "\n", 2)[0]; title != want {
			t.Fatalf("title %q, want %q", title, want)
		}
	}
	var exists bool
	var left int
	if err := pool.QueryRow(ctx, `SELECT to_regclass('work_memories') IS NOT NULL`).Scan(&exists); err != nil || !exists {
		t.Fatalf("work_memories dropped: %v %v", exists, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM work_memories`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("work_memories still holds %d rows: %v", left, err)
	}
	if err := run(ctx, pool, schema, migrationFS); err != nil {
		t.Fatalf("migrations are not idempotent: %v", err)
	}
}

func TestMemoryFixesMigrationFlagsMigratedFactsForTheBackfill(t *testing.T) {
	// 0014 flags the facts 0010 migrated; memory.BackfillLegacy (run by `work migrate`) then redacts them and gives them DeriveTitle's title instead of 0010's 120-character markdown cut.
	dsn := os.Getenv("WORK_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WORK_TEST_DATABASE_URL not set")
	}
	// Arrange
	ctx := context.Background()
	schema := fmt.Sprintf("work_test_mig13_%d", time.Now().UnixNano())
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
	if err := run(ctx, pool, schema, migrationsBefore(t, "0010")); err != nil {
		t.Fatal(err)
	}
	text := "##   Deploy rules  " + strings.Repeat("for the risk platform ", 8) + "api_key=sk-abcdefghijklmnopqrstuvwxyz123456\nsecond line"
	id := "0b0c8f8e-3a5b-4c1d-9e2f-1a2b3c4d5e6f"
	if _, err := pool.Exec(ctx, `INSERT INTO work_memories (id, user_login, text, status, created_at)
		VALUES ($1, 'alice', $2, 'active', '2026-01-15T10:00:00Z')`, id, text); err != nil {
		t.Fatal(err)
	}
	if err := run(ctx, pool, schema, migrationsBefore(t, "0014")); err != nil {
		t.Fatal(err)
	}
	memID := "mem_" + strings.ReplaceAll(id, "-", "")
	var before string
	if err := pool.QueryRow(ctx, `SELECT title FROM work_memory_entries WHERE id = $1`, memID).Scan(&before); err != nil {
		t.Fatal(err)
	}

	// Act
	err = run(ctx, pool, schema, migrationFS)
	var flagged bool
	_ = pool.QueryRow(ctx, `SELECT metadata ? 'legacy_backfill' FROM work_memory_entries WHERE id = $1`, memID).Scan(&flagged)
	n, berr := memory.BackfillLegacy(ctx, pgstore.NewPG(pool))
	e, gerr := pgstore.NewPG(pool).GetMemoryEntry(ctx, "alice", memID)

	// Assert
	if err != nil || !flagged || berr != nil || n != 1 || gerr != nil {
		t.Fatalf("migrate %v flagged %v backfill %d %v get %v", err, flagged, n, berr, gerr)
	}
	if !strings.HasPrefix(before, "## Deploy") || len([]rune(before)) != 120 {
		t.Fatalf("0010 title %q", before)
	}
	if strings.Contains(e.Content, "sk-abcdefghij") || e.Title != memory.DeriveTitle(e.Content) || strings.HasPrefix(e.Title, "#") ||
		e.Metadata["legacy_backfill"] != nil {
		t.Fatalf("backfilled %+v", e)
	}
	if again, _ := memory.BackfillLegacy(ctx, pgstore.NewPG(pool)); again != 0 {
		t.Fatalf("second backfill %d", again)
	}
}
