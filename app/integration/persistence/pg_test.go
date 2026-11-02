package persistence_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/7ar-chat/harness/adapters/store/pgstore"
	"github.com/7ar-chat/harness/adapters/store/pgstore/migrate"
	"github.com/7ar-chat/harness/redisinfra"
)

// database is a throwaway Postgres database of its own on the server of HARNESS_TEST_DATABASE_URL.
type database struct {
	t   *testing.T
	dsn string
}

// newDatabase creates e2e_<random> and drops it (WITH FORCE) at cleanup; it skips the test without a server.
func newDatabase(t *testing.T) *database {
	t.Helper()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil))) // migrations log at info
	server := os.Getenv("HARNESS_TEST_DATABASE_URL")
	if server == "" {
		t.Skip("HARNESS_TEST_DATABASE_URL is not set: these tests need a throwaway Postgres server")
	}
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	name := "e2e_" + hex.EncodeToString(buf)
	ctx, cancel := context.WithTimeout(context.Background(), failAfter)
	defer cancel()
	admin, err := pgx.Connect(ctx, server)
	if err != nil {
		t.Fatalf("connecting to the test server: %v", err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("creating %s: %v", name, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), failAfter)
		defer cancel()
		c, err := pgx.Connect(ctx, server)
		if err != nil {
			t.Errorf("dropping %s: %v", name, err)
			return
		}
		defer c.Close(ctx)
		if _, err := c.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("dropping %s: %v", name, err)
		}
	})
	u, err := url.Parse(server)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return &database{t: t, dsn: u.String()}
}

// open migrates the database (a no-op after the first time) and returns a new pool, closed at cleanup at the latest.
func (d *database) open() *pgxpool.Pool {
	d.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), failAfter)
	defer cancel()
	pool, err := pgstore.NewPool(ctx, d.dsn, 8, true)
	if err != nil {
		d.t.Fatal(err)
	}
	d.t.Cleanup(pool.Close)
	if err := migrate.Run(ctx, pool, ""); err != nil {
		d.t.Fatal(err)
	}
	return pool
}

// startPGProcess is a process over the pool's database; stopping it closes the pool.
func startPGProcess(t *testing.T, pool *pgxpool.Pool, o processOpts) *process {
	t.Helper()
	st := pgstore.NewPG(pool)
	o.store = st
	if o.rdb != nil {
		infra := redisinfra.Infra(o.rdb, st)
		o.infra = &infra
	}
	if o.closeStore == nil {
		o.closeStore = pool.Close
	}
	return startProcess(t, o)
}
