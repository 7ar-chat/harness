package threadstate_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/7ar-chat/harness/adapters/store/pgstore/migrate"
	"github.com/7ar-chat/harness/adapters/store/threadstate"
)

// newPGPool opens a pool on a fresh schema with every migration applied; the test skips without a database.
func newPGPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("HARNESS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HARNESS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("work_test_threadstate_%d", time.Now().UnixNano())
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
	return pool
}

func TestPG(t *testing.T) { suite(t, threadstate.NewPG(newPGPool(t))) }

// Two replicas appending to one document at once must not lose an update: the advisory lock serialises them.
func TestPGConcurrentUpdatesAreSerialised(t *testing.T) {
	st := threadstate.NewPG(newPGPool(t))
	ctx := context.Background()
	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := st.UpdateThreadState(ctx, "t-conc", "counter", func(old json.RawMessage) (json.RawMessage, error) {
				c := 0
				if len(old) > 0 {
					_, _ = fmt.Sscanf(string(old), "%d", &c)
				}
				return json.RawMessage(fmt.Sprint(c + 1)), nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, err := st.GetThreadState(ctx, "t-conc", "counter")
	if err != nil || string(got) != fmt.Sprint(n) {
		t.Fatalf("counter = %q, %v; want %d", got, err, n)
	}
}
