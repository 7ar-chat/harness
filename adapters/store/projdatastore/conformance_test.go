package projdatastore_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/7ar-chat/harness/adapters/store/pgstore/migrate"
	"github.com/7ar-chat/harness/adapters/store/projdatastore"
	"github.com/7ar-chat/harness/core/projdata"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func newPG(t *testing.T) projdata.Store {
	t.Helper()
	dsn := os.Getenv("HARNESS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HARNESS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("work_test_projdata_%d", time.Now().UnixNano())
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
	return projdatastore.NewPG(pool)
}

func eachStore(t *testing.T, fn func(t *testing.T, s projdata.Store)) {
	t.Helper()
	for _, b := range []struct {
		name string
		open func(t *testing.T) projdata.Store
	}{
		{"mem", func(*testing.T) projdata.Store { return projdatastore.NewMem() }},
		{"file", func(t *testing.T) projdata.Store {
			f, err := projdatastore.Open(filepath.Join(t.TempDir(), "projdata.json"))
			if err != nil {
				t.Fatal(err)
			}
			return f
		}},
		{"pg", newPG},
	} {
		t.Run(b.name, func(t *testing.T) { fn(t, b.open(t)) })
	}
}

func TestDigestSettings(t *testing.T) {
	eachStore(t, func(t *testing.T, s projdata.Store) {
		ctx := context.Background()
		got, err := s.GetDigestSettings(ctx, "p1")
		if err != nil || got.Enabled || got.ProjectID != "p1" {
			t.Fatalf("default = %+v, %v", got, err)
		}
		want := projdata.DigestSettings{ProjectID: "p1", Enabled: true, Weekday: 1, Hour: 9, Timezone: "Europe/Berlin", EnabledAt: t0, LastSentAt: t0.Add(time.Hour), LastSlot: "2026-11-02"}
		if err := s.PutDigestSettings(ctx, want); err != nil {
			t.Fatal(err)
		}
		if err := s.PutDigestSettings(ctx, projdata.DigestSettings{ProjectID: "p2", Enabled: false}); err != nil {
			t.Fatal(err)
		}
		got, err = s.GetDigestSettings(ctx, "p1")
		if err != nil || !got.EnabledAt.Equal(want.EnabledAt) || !got.LastSentAt.Equal(want.LastSentAt) || got.Timezone != want.Timezone || got.Hour != 9 || got.LastSlot != want.LastSlot {
			t.Fatalf("get = %+v, %v", got, err)
		}
		on, err := s.EnabledDigests(ctx)
		if err != nil || len(on) != 1 || on[0].ProjectID != "p1" {
			t.Fatalf("enabled = %+v, %v", on, err)
		}
	})
}

func TestClaimSlotIsExclusiveAndReleasable(t *testing.T) {
	eachStore(t, func(t *testing.T, s projdata.Store) {
		ctx := context.Background()
		var wins atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ok, err := s.ClaimSlot(ctx, "p1", "2026-11-02", t0)
				if err != nil {
					t.Error(err)
				}
				if ok {
					wins.Add(1)
				}
			}()
		}
		wg.Wait()
		if wins.Load() != 1 {
			t.Fatalf("%d claimers won the same slot", wins.Load())
		}
		if ok, _ := s.ClaimSlot(ctx, "p2", "2026-11-02", t0); !ok {
			t.Fatal("another project's slot was refused")
		}
		if ok, _ := s.ClaimSlot(ctx, "p1", "2026-11-09", t0); !ok {
			t.Fatal("another slot was refused")
		}
		if err := s.ReleaseSlot(ctx, "p1", "2026-11-02"); err != nil {
			t.Fatal(err)
		}
		if ok, _ := s.ClaimSlot(ctx, "p1", "2026-11-02", t0); !ok {
			t.Fatal("a released slot cannot be claimed again")
		}
	})
}

func TestDigestsKeepTheNewestAndProtectTheReport(t *testing.T) {
	eachStore(t, func(t *testing.T, s projdata.Store) {
		ctx := context.Background()
		for i := 0; i < projdata.KeepDigests+3; i++ {
			d := projdata.Digest{ID: fmt.Sprintf("d%02d", i), ProjectID: "p1", Slot: fmt.Sprintf("s%d", i), Trigger: projdata.TriggerSchedule,
				From: t0, To: t0.Add(time.Hour), CreatedAt: t0.Add(time.Duration(i) * time.Minute), Summary: "sum", HTML: []byte("<html>" + fmt.Sprint(i)), JSON: []byte(`{"i":1}`)}
			if err := s.AddDigest(ctx, d); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.AddDigest(ctx, projdata.Digest{ID: "other", ProjectID: "p2", CreatedAt: t0, HTML: []byte("x")}); err != nil {
			t.Fatal(err)
		}
		list, err := s.ListDigests(ctx, "p1", 0)
		if err != nil || len(list) != projdata.KeepDigests {
			t.Fatalf("kept %d, %v", len(list), err)
		}
		if list[0].ID != fmt.Sprintf("d%02d", projdata.KeepDigests+2) || list[0].HTML != nil || list[0].JSON != nil || list[0].Summary != "sum" {
			t.Fatalf("newest first and without the report: %+v", list[0])
		}
		if last := list[len(list)-1].ID; last != "d03" {
			t.Fatalf("oldest kept = %s", last)
		}
		if two, _ := s.ListDigests(ctx, "p1", 2); len(two) != 2 {
			t.Fatalf("limit: %d", len(two))
		}
		d, err := s.GetDigest(ctx, "p1", "d10")
		if err != nil || !bytes.Equal(d.HTML, []byte("<html>10")) || !bytes.Equal(d.JSON, []byte(`{"i":1}`)) || !d.To.Equal(t0.Add(time.Hour)) {
			t.Fatalf("get = %+v, %v", d, err)
		}
		if _, err := s.GetDigest(ctx, "p2", "d10"); !errors.Is(err, projdata.ErrNotFound) {
			t.Fatalf("another project's digest: %v", err)
		}
		if _, err := s.GetDigest(ctx, "p1", "d00"); !errors.Is(err, projdata.ErrNotFound) {
			t.Fatalf("a trimmed digest: %v", err)
		}
	})
}

func TestSecretsAreSealedBlobsPerProjectAndKind(t *testing.T) {
	eachStore(t, func(t *testing.T, s projdata.Store) {
		ctx := context.Background()
		if _, err := s.GetSecret(ctx, "p1", "tapd"); !errors.Is(err, projdata.ErrNotFound) {
			t.Fatalf("missing: %v", err)
		}
		if err := s.PutSecret(ctx, "p1", "tapd", []byte{1, 2, 3}); err != nil {
			t.Fatal(err)
		}
		if err := s.PutSecret(ctx, "p1", "tapd", []byte{4, 5}); err != nil {
			t.Fatal(err)
		}
		if b, err := s.GetSecret(ctx, "p1", "tapd"); err != nil || !bytes.Equal(b, []byte{4, 5}) {
			t.Fatalf("get = %v, %v", b, err)
		}
		if _, err := s.GetSecret(ctx, "p2", "tapd"); !errors.Is(err, projdata.ErrNotFound) {
			t.Fatalf("another project: %v", err)
		}
		if err := s.DeleteSecret(ctx, "p1", "tapd"); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteSecret(ctx, "p1", "tapd"); err != nil {
			t.Fatalf("deleting twice: %v", err)
		}
		if _, err := s.GetSecret(ctx, "p1", "tapd"); !errors.Is(err, projdata.ErrNotFound) {
			t.Fatalf("after delete: %v", err)
		}
	})
}

func TestDeleteProjectRemovesEverything(t *testing.T) {
	eachStore(t, func(t *testing.T, s projdata.Store) {
		ctx := context.Background()
		_ = s.PutDigestSettings(ctx, projdata.DigestSettings{ProjectID: "p1", Enabled: true})
		_, _ = s.ClaimSlot(ctx, "p1", "x", t0)
		_ = s.AddDigest(ctx, projdata.Digest{ID: "d", ProjectID: "p1", CreatedAt: t0})
		_ = s.PutSecret(ctx, "p1", "tapd", []byte{1})
		_ = s.PutSecret(ctx, "p10", "tapd", []byte{1})
		if err := s.DeleteProject(ctx, "p1"); err != nil {
			t.Fatal(err)
		}
		if on, _ := s.EnabledDigests(ctx); len(on) != 0 {
			t.Fatal("settings remain")
		}
		if l, _ := s.ListDigests(ctx, "p1", 0); len(l) != 0 {
			t.Fatal("digests remain")
		}
		if _, err := s.GetSecret(ctx, "p1", "tapd"); !errors.Is(err, projdata.ErrNotFound) {
			t.Fatal("secret remains")
		}
		if ok, _ := s.ClaimSlot(ctx, "p1", "x", t0); !ok {
			t.Fatal("slot remains")
		}
		if _, err := s.GetSecret(ctx, "p10", "tapd"); err != nil {
			t.Fatal("a project with a similar id lost its secret")
		}
	})
}

func TestFileStoreSurvivesReopenAndRefusesAForeignFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projdata.json")
	ctx := context.Background()
	f, err := projdatastore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.PutDigestSettings(ctx, projdata.DigestSettings{ProjectID: "p1", Enabled: true, Hour: 7})
	_ = f.AddDigest(ctx, projdata.Digest{ID: "d", ProjectID: "p1", CreatedAt: t0, HTML: []byte("h")})
	_ = f.PutSecret(ctx, "p1", "tapd", []byte("sealed"))
	_, _ = f.ClaimSlot(ctx, "p1", "s", t0)
	g, err := projdatastore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := g.GetDigestSettings(ctx, "p1"); !s.Enabled || s.Hour != 7 {
		t.Fatalf("settings = %+v", s)
	}
	if d, err := g.GetDigest(ctx, "p1", "d"); err != nil || string(d.HTML) != "h" {
		t.Fatalf("digest = %+v, %v", d, err)
	}
	if b, _ := g.GetSecret(ctx, "p1", "tapd"); string(b) != "sealed" {
		t.Fatal("secret lost")
	}
	if ok, _ := g.ClaimSlot(ctx, "p1", "s", t0); ok {
		t.Fatal("claim lost")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte(`{"version":9}`), 0o600)
	if _, err := projdatastore.Open(bad); err == nil {
		t.Fatal("opened a foreign file")
	}
}
