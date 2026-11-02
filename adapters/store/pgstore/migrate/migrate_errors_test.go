package migrate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var schemaSeq atomic.Int64

// newSchemaPool returns a pool whose search_path is a fresh schema name (not yet created) and drops it at cleanup.
func newSchemaPool(t *testing.T, maxConns int32) (*pgxpool.Pool, string) {
	t.Helper()
	dsn := os.Getenv("HARNESS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HARNESS_TEST_DATABASE_URL not set")
	}
	schema := fmt.Sprintf("work_test_err_%d_%d", time.Now().UnixNano(), schemaSeq.Add(1))
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = maxConns
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
		pool.Close()
	})
	return pool, schema
}

// plainDB hides the pool's Acquire so the runner takes its transaction-lock path; failBegin makes the nth Begin fail.
type plainDB struct {
	pool      *pgxpool.Pool
	begins    int
	failBegin int
}

func (p *plainDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return p.pool.QueryRow(ctx, sql, args...)
}

func (p *plainDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return p.pool.Query(ctx, sql, args...)
}

func (p *plainDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return p.pool.Exec(ctx, sql, args...)
}

func (p *plainDB) Begin(ctx context.Context) (pgx.Tx, error) {
	p.begins++
	if p.begins == p.failBegin {
		return nil, errors.New("begin refused")
	}
	return p.pool.Begin(ctx)
}

func sqlFiles(files map[string]string) fs.FS {
	out := fstest.MapFS{}
	for name, sql := range files {
		out["migrations/"+name] = &fstest.MapFile{Data: []byte(sql)}
	}
	return out
}

func recorded(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT name FROM work_migrations ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

func tableExists(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(), `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// holdLock takes the migration advisory lock of schema on its own connection and returns its release.
func holdLock(t *testing.T, pool *pgxpool.Pool, schema string) (release func()) {
	t.Helper()
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(context.Background(), `SELECT pg_advisory_lock(`+lockKey+`)`, schema); err != nil {
		t.Fatal(err)
	}
	return func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock(`+lockKey+`)`, schema)
		conn.Release()
	}
}

func TestAFailingMigrationRollsBackAndKeepsTheEarlierOnes(t *testing.T) {
	// A migration whose SQL fails leaves nothing behind (not even its first statement) and is not recorded, while earlier migrations stay applied; after the fix only the rest runs.
	pool, schema := newSchemaPool(t, 4)
	ctx := context.Background()
	broken := sqlFiles(map[string]string{
		"0001_a.sql": `CREATE TABLE a (id int)`,
		"0002_b.sql": `CREATE TABLE b (id int); SELECT * FROM table_that_does_not_exist`,
		"0003_c.sql": `CREATE TABLE c (id int)`,
	})

	// Act
	err := run(ctx, pool, schema, broken)

	// Assert
	if err == nil || !strings.Contains(err.Error(), "execute migration 0002_b.sql") {
		t.Fatalf("err %v", err)
	}
	if got := recorded(t, pool); len(got) != 1 || got[0] != "0001_a.sql" {
		t.Fatalf("recorded %v", got)
	}
	if !tableExists(t, pool, "a") || tableExists(t, pool, "b") || tableExists(t, pool, "c") {
		t.Fatalf("tables a/b/c: %v %v %v", tableExists(t, pool, "a"), tableExists(t, pool, "b"), tableExists(t, pool, "c"))
	}

	// Act: the broken migration is fixed
	fixed := sqlFiles(map[string]string{
		"0001_a.sql": `CREATE TABLE a (id int)`,
		"0002_b.sql": `CREATE TABLE b (id int)`,
		"0003_c.sql": `CREATE TABLE c (id int)`,
	})
	err = run(ctx, pool, schema, fixed)

	// Assert
	if got := recorded(t, pool); err != nil || len(got) != 3 || got[1] != "0002_b.sql" || got[2] != "0003_c.sql" {
		t.Fatalf("after the fix: err %v recorded %v", err, got)
	}
}

func TestMigrationsRunOnceInNameOrderAndIgnoreOtherFiles(t *testing.T) {
	// Migrations apply in name order whatever the directory order, each exactly once; files that are not .sql and subdirectories are ignored.
	pool, schema := newSchemaPool(t, 4)
	ctx := context.Background()
	files := fstest.MapFS{
		"migrations/0002_second.sql": {Data: []byte(`INSERT INTO order_log (step) VALUES ('second')`)},
		"migrations/0001_first.sql":  {Data: []byte(`CREATE TABLE order_log (id serial, step text); INSERT INTO order_log (step) VALUES ('first')`)},
		"migrations/README.md":       {Data: []byte(`not sql at all`)},
		"migrations/archive.sql/x":   {Data: []byte(`also not a migration`)},
	}

	// Act
	first := run(ctx, pool, schema, files)
	second := run(ctx, pool, schema, files)

	// Assert
	rows, err := pool.Query(ctx, `SELECT step FROM order_log ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		steps = append(steps, s)
	}
	rows.Close()
	if first != nil || second != nil || len(steps) != 2 || steps[0] != "first" || steps[1] != "second" {
		t.Fatalf("first %v second %v steps %v", first, second, steps)
	}
	if got := recorded(t, pool); len(got) != 2 || got[0] != "0001_first.sql" || got[1] != "0002_second.sql" {
		t.Fatalf("recorded %v", got)
	}
}

func TestAMigrationThatBreaksTheLedgerIsRolledBack(t *testing.T) {
	// If recording a migration fails (here it dropped the ledger table), its changes are rolled back with the ledger restored, so it is retried instead of half-applied.
	pool, schema := newSchemaPool(t, 4)
	ctx := context.Background()
	files := sqlFiles(map[string]string{"0001_evil.sql": `CREATE TABLE evil (id int); DROP TABLE work_migrations`})

	// Act
	err := run(ctx, pool, schema, files)

	// Assert
	if err == nil || !strings.Contains(err.Error(), "record migration 0001_evil.sql") {
		t.Fatalf("err %v", err)
	}
	if tableExists(t, pool, "evil") || !tableExists(t, pool, "work_migrations") || len(recorded(t, pool)) != 0 {
		t.Fatal("a migration that could not be recorded must leave no trace")
	}
}

func TestACommitFailureIsReportedAndLeavesNothingApplied(t *testing.T) {
	// A deferred constraint violated only at COMMIT fails the migration: the error names it and nothing is applied or recorded.
	pool, schema := newSchemaPool(t, 4)
	ctx := context.Background()
	files := sqlFiles(map[string]string{"0001_deferred.sql": `
		CREATE TABLE d (a int UNIQUE DEFERRABLE INITIALLY DEFERRED);
		INSERT INTO d VALUES (1), (1)`})

	// Act
	err := run(ctx, pool, schema, files)

	// Assert
	if err == nil || !strings.Contains(err.Error(), "commit migration 0001_deferred.sql") {
		t.Fatalf("err %v", err)
	}
	if tableExists(t, pool, "d") || len(recorded(t, pool)) != 0 {
		t.Fatal("a migration that failed to commit must leave no trace")
	}
}

func TestRunReportsAnUnreadableMigrationSet(t *testing.T) {
	// A missing migrations directory and an unreadable migration file are both errors, not a silent "nothing to apply".
	pool, schema := newSchemaPool(t, 4)
	ctx := context.Background()
	readable := fstest.MapFS{"migrations/0001_a.sql": {Data: []byte(`SELECT 1`)}}

	// Act
	noDir := run(ctx, pool, schema, fstest.MapFS{})
	unreadable := run(ctx, pool, schema, openFails{readable})

	// Assert
	if noDir == nil || !strings.Contains(noDir.Error(), "read migrations dir") {
		t.Fatalf("missing directory: %v", noDir)
	}
	if unreadable == nil || !strings.Contains(unreadable.Error(), "read migration 0001_a.sql") {
		t.Fatalf("unreadable file: %v", unreadable)
	}
	if len(recorded(t, pool)) != 0 {
		t.Fatal("nothing may be recorded")
	}
}

// openFails lists the directory but cannot open migration files.
type openFails struct{ inner fs.FS }

func (o openFails) Open(name string) (fs.File, error) {
	if strings.HasSuffix(name, ".sql") {
		return nil, errors.New("disk error")
	}
	return o.inner.Open(name)
}

func TestRunReportsALedgerItCannotRead(t *testing.T) {
	// A work_migrations table that already exists with another shape (no name column) fails the run with a clear error, and one holding a NULL name fails the scan.
	pool, schema := newSchemaPool(t, 4)
	ctx := context.Background()
	files := sqlFiles(map[string]string{"0001_a.sql": `SELECT 1`})
	if _, err := pool.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE work_migrations (other int)`); err != nil {
		t.Fatal(err)
	}

	// Act
	noColumn := run(ctx, pool, schema, files)
	_, _ = pool.Exec(ctx, `DROP TABLE work_migrations`)
	_, _ = pool.Exec(ctx, `CREATE TABLE work_migrations (name varchar)`)
	_, _ = pool.Exec(ctx, `INSERT INTO work_migrations (name) VALUES (NULL)`)
	nullName := run(ctx, pool, schema, files)

	// Assert
	if noColumn == nil || !strings.Contains(noColumn.Error(), "query applied migrations") {
		t.Fatalf("no name column: %v", noColumn)
	}
	if nullName == nil || strings.Contains(nullName.Error(), "applied migrations") {
		t.Fatalf("NULL name must fail the scan: %v", nullName)
	}
}

func TestRunReportsSchemaAndLedgerCreationFailures(t *testing.T) {
	// A schema name Postgres refuses, and an unusable search_path for the default schema, fail with errors saying which step broke.
	pool, schema := newSchemaPool(t, 4)
	ctx := context.Background()
	files := sqlFiles(map[string]string{"0001_a.sql": `SELECT 1`})

	// Act
	reserved := run(ctx, pool, "pg_reserved_"+schema, files)
	noTarget := run(ctx, pool, "", files) // schema "" skips CREATE SCHEMA, and the search_path schema does not exist

	// Assert
	if reserved == nil || !strings.Contains(reserved.Error(), "create schema pg_reserved_") {
		t.Fatalf("reserved schema: %v", reserved)
	}
	if noTarget == nil || !strings.Contains(noTarget.Error(), "create migrations table") {
		t.Fatalf("no target schema: %v", noTarget)
	}
}

func TestRunInTheDefaultSchemaDoesNotCreateOne(t *testing.T) {
	// For "" and "public" the runner creates no schema and migrates into whatever the connection's search_path selects.
	for _, name := range []string{"", "public"} {
		t.Run("schema="+name, func(t *testing.T) {
			pool, schema := newSchemaPool(t, 4)
			ctx := context.Background()
			if _, err := pool.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
				t.Fatal(err)
			}

			// Act
			err := run(ctx, pool, name, sqlFiles(map[string]string{"0001_a.sql": `CREATE TABLE a (id int)`}))

			// Assert
			var inSchema, elsewhere int
			scanErr := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE table_schema = $1), count(*) FILTER (WHERE table_schema <> $1)
				FROM information_schema.tables WHERE table_name = 'work_migrations' AND table_schema = ANY (ARRAY[$1, 'public'])`, schema).Scan(&inSchema, &elsewhere)
			if err != nil || scanErr != nil || inSchema != 1 {
				t.Fatalf("err %v (%v), ledger tables in %q: %d, in public: %d", err, scanErr, schema, inSchema, elsewhere)
			}
		})
	}
}

func TestRunWithoutAPoolTakesATransactionLockAndReleasesItOnEveryOutcome(t *testing.T) {
	// Through a plain DB (no Acquire) the lock is a transaction: success and failed migrations both release it, so the next run is not blocked.
	pool, schema := newSchemaPool(t, 4)
	db := &plainDB{pool: pool}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Act
	failed := run(ctx, db, schema, sqlFiles(map[string]string{"0001_bad.sql": `SELECT * FROM missing_table`}))
	ok := run(ctx, db, schema, sqlFiles(map[string]string{"0001_a.sql": `CREATE TABLE a (id int)`}))

	// Assert
	if failed == nil || ok != nil || !tableExists(t, pool, "a") {
		t.Fatalf("failed %v ok %v", failed, ok)
	}
}

func TestRunReportsWhenTheLockTransactionCannotStart(t *testing.T) {
	// If the lock transaction cannot begin, or a later migration transaction cannot, the error says which and no migration is recorded past that point.
	pool, schema := newSchemaPool(t, 4)
	ctx := context.Background()
	files := sqlFiles(map[string]string{"0001_a.sql": `CREATE TABLE a (id int)`})

	// Act
	lockFails := run(ctx, &plainDB{pool: pool, failBegin: 1}, schema, files)
	migrationFails := run(ctx, &plainDB{pool: pool, failBegin: 2}, schema, files)

	// Assert
	if lockFails == nil || !strings.Contains(lockFails.Error(), "begin migration lock") {
		t.Fatalf("lock begin: %v", lockFails)
	}
	if migrationFails == nil || !strings.Contains(migrationFails.Error(), "begin tx for 0001_a.sql") {
		t.Fatalf("migration begin: %v", migrationFails)
	}
	if tableExists(t, pool, "a") {
		t.Fatal("no migration may have been applied")
	}
}

func TestRunRefusesASchemaNameThePostgresLockCannotTake(t *testing.T) {
	// A schema name Postgres cannot take as a text parameter (NUL byte) fails at the lock, for both lock strategies, and releases what it held.
	pool, schema := newSchemaPool(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	files := sqlFiles(map[string]string{"0001_a.sql": `SELECT 1`})

	// Act
	withPool := run(ctx, pool, "bad\x00name", files)
	withTx := run(ctx, &plainDB{pool: pool}, "bad\x00name", files)
	healthy := run(ctx, pool, schema, files)

	// Assert
	if withPool == nil || !strings.Contains(withPool.Error(), "take migration lock") {
		t.Fatalf("pool: %v", withPool)
	}
	if withTx == nil || !strings.Contains(withTx.Error(), "take migration lock") {
		t.Fatalf("transaction: %v", withTx)
	}
	if healthy != nil {
		t.Fatalf("the only connection was not given back: %v", healthy)
	}
}

func TestRunWithACancelledContextFailsBeforeTouchingTheDatabase(t *testing.T) {
	// A context that is already cancelled stops the run at the lock for both strategies, without creating anything.
	pool, schema := newSchemaPool(t, 4)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	files := sqlFiles(map[string]string{"0001_a.sql": `SELECT 1`})

	// Act
	withPool := run(ctx, pool, schema, files)
	withTx := run(ctx, &plainDB{pool: pool}, schema, files)

	// Assert
	if withPool == nil || !strings.Contains(withPool.Error(), "acquire migration connection") || !errors.Is(withPool, context.Canceled) {
		t.Fatalf("pool: %v", withPool)
	}
	if withTx == nil || !errors.Is(withTx, context.Canceled) {
		t.Fatalf("transaction: %v", withTx)
	}
	var exists bool
	if err := pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, schema).Scan(&exists); err != nil || exists {
		t.Fatalf("schema created: %v %v", exists, err)
	}
}

func TestRunWaitsForTheMigrationLockHeldElsewhere(t *testing.T) {
	// While another migrator holds the lock of a schema, Run blocks and gives up with the context; locks of other schemas do not interfere; once released the run goes through.
	// Both lock strategies behave the same.
	for name, wrap := range map[string]func(*pgxpool.Pool) DB{
		"pool":        func(p *pgxpool.Pool) DB { return p },
		"transaction": func(p *pgxpool.Pool) DB { return &plainDB{pool: p} },
	} {
		t.Run(name, func(t *testing.T) {
			pool, schema := newSchemaPool(t, 4)
			files := sqlFiles(map[string]string{"0001_a.sql": `CREATE TABLE a (id int)`})
			release := holdLock(t, pool, schema)
			released := false
			defer func() {
				if !released {
					release()
				}
			}()
			db := wrap(pool)

			// Act
			blockedCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			blocked := run(blockedCtx, db, schema, files)
			cancel()
			otherPool, otherSchema := newSchemaPool(t, 4)
			other := run(context.Background(), wrap(otherPool), otherSchema, files)
			release()
			released = true
			afterCtx, cancelAfter := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancelAfter()
			after := run(afterCtx, db, schema, files)

			// Assert
			if !errors.Is(blocked, context.DeadlineExceeded) || !strings.Contains(blocked.Error(), "take migration lock") {
				t.Fatalf("blocked run: %v", blocked)
			}
			if other != nil {
				t.Fatalf("a lock on another schema must not block: %v", other)
			}
			if after != nil || !tableExists(t, pool, "a") {
				t.Fatalf("run after release: %v", after)
			}
		})
	}
}

func TestRunClosesALockConnectionThatDiedDuringTheMigration(t *testing.T) {
	// When the session holding the lock dies mid-migration the run fails with the migration's error, and the pool recovers: the next run on it succeeds.
	pool, schema := newSchemaPool(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	killer := sqlFiles(map[string]string{"0001_kill.sql": `SELECT pg_terminate_backend(pg_backend_pid())`})
	healthy := sqlFiles(map[string]string{"0001_a.sql": `CREATE TABLE a (id int)`})

	// Act
	killed := run(ctx, pool, schema, killer)
	after := run(ctx, pool, schema, healthy)

	// Assert
	if killed == nil || !strings.Contains(killed.Error(), "execute migration 0001_kill.sql") {
		t.Fatalf("killed run: %v", killed)
	}
	if after != nil || !tableExists(t, pool, "a") {
		t.Fatalf("the pool did not recover: %v", after)
	}
}
