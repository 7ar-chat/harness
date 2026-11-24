package notifystore_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/7ar-chat/harness/adapters/store/notifystore"
	"github.com/7ar-chat/harness/adapters/store/pgstore/migrate"
	"github.com/7ar-chat/harness/core/notify"
	"github.com/7ar-chat/harness/core/notify/notifytest"
)

func newPG(t *testing.T) notify.Store {
	t.Helper()
	dsn := os.Getenv("HARNESS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HARNESS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("work_test_notify_%d", time.Now().UnixNano())
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
	return notifystore.NewPG(pool, nil)
}

func TestMemConformance(t *testing.T) {
	notifytest.Run(t, func(*testing.T) notify.Store { return notifystore.NewMem(nil) })
}

func TestFileConformance(t *testing.T) {
	notifytest.Run(t, func(t *testing.T) notify.Store {
		s, err := notifystore.Open(filepath.Join(t.TempDir(), "notifications.json"), nil)
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

func TestPGConformance(t *testing.T) { notifytest.Run(t, newPG) }

func TestFilePersistsAcrossOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.json")
	s, err := notifystore.Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_ = s.PutChannel(ctx, notify.Channel{ID: "a", ProjectID: "p", Name: "n", Sealed: []byte("sealed")})
	_ = s.PutPrefs(ctx, notify.Prefs{Login: "ann", Digest: true})
	_ = s.AppendLog(ctx, notify.LogEntry{ID: "1", ProjectID: "p", Status: notify.Failed, Error: "x"})
	again, err := notifystore.Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := again.GetChannel(ctx, "p", "a")
	if err != nil || string(c.Sealed) != "sealed" {
		t.Fatalf("channel = %+v, %v", c, err)
	}
	if p, _ := again.GetPrefs(ctx, "ann"); !p.Digest {
		t.Fatal("prefs lost")
	}
	if l, _ := again.ListLog(ctx, "p", "", 5); len(l) != 1 || l[0].Error != "x" {
		t.Fatal("log lost")
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", st.Mode())
	}
}

func TestFileRefusesAForeignFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.json")
	_ = os.WriteFile(path, []byte(`{"version":9}`), 0o600)
	if _, err := notifystore.Open(path, nil); err == nil {
		t.Fatal("unknown version must be refused")
	}
}
