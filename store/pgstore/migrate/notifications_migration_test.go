package migrate

import (
	"context"
	"fmt"
	"github.com/GhaziBenDahmane/harness/store/pgstore"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestNotificationsMigrationLeavesExistingThreadsRead(t *testing.T) {
	// 0100 counts the threads that existed before it as seen: a deploy does not flood users with unread threads.
	dsn := os.Getenv("WORK_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WORK_TEST_DATABASE_URL not set")
	}
	// Arrange
	ctx := context.Background()
	schema := fmt.Sprintf("work_test_notif_%d", time.Now().UnixNano())
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
	if err := run(ctx, pool, schema, migrationsBefore(t, "0100")); err != nil {
		t.Fatal(err)
	}
	thread, runID := "6f1c2d3e-4b5a-4c6d-8e7f-9a0b1c2d3e4f", "7f1c2d3e-4b5a-4c6d-8e7f-9a0b1c2d3e4f"
	if _, err := pool.Exec(ctx, `INSERT INTO work_threads (id, user_login, title) VALUES ($1, 'alice', 't')`, thread); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO work_runs (id, thread_id, user_login, status, model, finished_at)
		VALUES ($1, $2, 'alice', 'completed', 'm', now() - interval '1 day')`, runID, thread); err != nil {
		t.Fatal(err)
	}

	// Act
	err = Run(ctx, pool, schema)

	// Assert
	n, cerr := pgstore.NewPG(pool).CountUnreadThreads(ctx, "alice")
	if err != nil || cerr != nil || n != 0 {
		t.Fatalf("migrate %v, count %d (%v)", err, n, cerr)
	}
}
