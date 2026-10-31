package migrate_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/7ar-chat/harness/adapters/store/pgstore/migrate"
)

func TestTwoConcurrentMigrateRunsBothSucceed(t *testing.T) {
	// Two `work migrate` jobs started together on a fresh schema both succeed (serialized by an advisory lock), five rounds in a row.
	dsn := os.Getenv("HARNESS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HARNESS_TEST_DATABASE_URL not set")
	}
	for round := 0; round < 5; round++ {
		// Arrange
		ctx := context.Background()
		schema := fmt.Sprintf("work_test_conc_%d", time.Now().UnixNano())
		pools := make([]*pgxpool.Pool, 2)
		for i := range pools {
			cfg, err := pgxpool.ParseConfig(dsn)
			if err != nil {
				t.Fatal(err)
			}
			cfg.ConnConfig.RuntimeParams["search_path"] = schema
			if pools[i], err = pgxpool.NewWithConfig(ctx, cfg); err != nil {
				t.Fatal(err)
			}
		}
		errs := make([]error, 2)
		var wg sync.WaitGroup

		// Act
		for i := range pools {
			wg.Add(1)
			go func() { defer wg.Done(); errs[i] = migrate.Run(ctx, pools[i], schema) }()
		}
		wg.Wait()

		// Assert
		var applied int
		_ = pools[0].QueryRow(ctx, `SELECT count(*) FROM work_migrations`).Scan(&applied)
		_, _ = pools[0].Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		for _, p := range pools {
			p.Close()
		}
		if errs[0] != nil || errs[1] != nil || applied == 0 {
			t.Fatalf("round %d: errors %v / %v, %d migrations recorded", round, errs[0], errs[1], applied)
		}
	}
}

func TestMigrateSucceedsWithAOneConnectionPool(t *testing.T) {
	// With DB_POOL_MAX_CONNS=1 the migration lock does not hold the only connection: migrate completes instead of deadlocking (review round 1).
	dsn := os.Getenv("HARNESS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HARNESS_TEST_DATABASE_URL not set")
	}
	// Arrange
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	schema := fmt.Sprintf("work_test_one_%d", time.Now().UnixNano())
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	// Act
	err = migrate.Run(ctx, pool, schema)

	// Assert
	var applied int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM work_migrations`).Scan(&applied)
	_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	if err != nil || applied == 0 {
		t.Fatalf("err %v, %d migrations recorded", err, applied)
	}
}
