// Package migrate applies the embedded SQL migrations (mirrors authn/internal/migrate).
package migrate

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// DB is the subset of pgxpool.Pool used by the runner.
type DB interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Run creates the schema if missing and applies pending migrations, each in its own transaction.
func Run(ctx context.Context, db DB, schema string) error {
	return run(ctx, db, schema, migrationFS)
}

// acquirer hands out a dedicated connection (pgxpool.Pool).
type acquirer interface {
	Acquire(ctx context.Context) (*pgxpool.Conn, error)
}

const lockKey = `hashtextextended('work_migrate:' || $1, 0)`

// lock serializes migrators of one schema (concurrent `work migrate` jobs or
// pods). With a pool, the lock and every migration statement use one
// dedicated connection (a session advisory lock), so a pool of one connection
// (DB_POOL_MAX_CONNS=1) cannot deadlock on the lock's own connection; the
// returned db is that connection. Otherwise a transaction holds
// pg_advisory_xact_lock until unlock commits it.
func lock(ctx context.Context, db DB, schema string) (locked DB, unlock func(), err error) {
	if p, ok := db.(acquirer); ok {
		conn, err := p.Acquire(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("acquire migration connection: %w", err)
		}
		if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(`+lockKey+`)`, schema); err != nil {
			conn.Release()
			return nil, nil, fmt.Errorf("take migration lock: %w", err)
		}
		return conn, func() {
			uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			if _, err := conn.Exec(uctx, `SELECT pg_advisory_unlock(`+lockKey+`)`, schema); err != nil {
				_ = conn.Hijack().Close(uctx) // closing the session releases the lock
				return
			}
			conn.Release()
		}, nil
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("begin migration lock: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(`+lockKey+`)`, schema); err != nil {
		_ = tx.Rollback(ctx)
		return nil, nil, fmt.Errorf("take migration lock: %w", err)
	}
	return db, func() { _ = tx.Commit(context.WithoutCancel(ctx)) }, nil
}

func run(ctx context.Context, db DB, schema string, migrations fs.FS) error {
	db, unlock, err := lock(ctx, db, schema)
	if err != nil {
		return err
	}
	defer unlock()
	if schema != "" && schema != "public" {
		if _, err := db.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS `+pgx.Identifier{schema}.Sanitize()); err != nil {
			return fmt.Errorf("create schema %s: %w", schema, err)
		}
	}
	if _, err := db.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS work_migrations (
			id         SERIAL PRIMARY KEY,
			name       VARCHAR(255) NOT NULL UNIQUE,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`); err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}

	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	applied := map[string]bool{}
	rows, err := db.Query(ctx, `SELECT name FROM work_migrations`)
	if err != nil {
		return fmt.Errorf("query applied migrations: %w", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		applied[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate applied migrations: %w", err)
	}

	count := 0
	for _, name := range names {
		if applied[name] {
			continue
		}
		sql, err := fs.ReadFile(migrations, "migrations/"+name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		slog.Info("applying migration", "migration", name)
		tx, err := db.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin tx for %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("execute migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO work_migrations (name) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
		count++
	}
	slog.Info("migrations complete", "applied", count)
	return nil
}
