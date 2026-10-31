package pgstore_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/store/pgstore"
)

func poolTestDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("HARNESS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HARNESS_TEST_DATABASE_URL not set")
	}
	return dsn
}

func TestNewPoolRejectsAMalformedDSN(t *testing.T) {
	// Arrange
	ctx := context.Background()

	// Act
	pool, err := pgstore.NewPool(ctx, "postgres://user:pw@host:notaport/db", 4, false)

	// Assert
	if err == nil || pool != nil {
		if pool != nil {
			pool.Close()
		}
		t.Fatalf("a DSN that does not parse must fail: pool %v err %v", pool, err)
	}
}

func TestNewPoolConnectsLazilyWithoutPing(t *testing.T) {
	// Arrange: nothing listens on the port; without ping the pool is still returned.
	ctx := context.Background()

	// Act
	pool, err := pgstore.NewPool(ctx, "postgres://u:p@127.0.0.1:1/db?sslmode=disable&connect_timeout=1", 3, false)

	// Assert
	if err != nil {
		t.Fatalf("connections are lazy, got %v", err)
	}
	defer pool.Close()
	if got := pool.Config().MaxConns; got != 3 {
		t.Errorf("MaxConns %d, want 3", got)
	}
}

func TestNewPoolWithPingReachesTheDatabase(t *testing.T) {
	// Arrange
	dsn := poolTestDSN(t)
	ctx := context.Background()

	// Act
	pool, err := pgstore.NewPool(ctx, dsn, 7, true)

	// Assert
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()
	if got := pool.Config().MaxConns; got != 7 {
		t.Errorf("MaxConns %d, want 7", got)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Errorf("ping: %v", err)
	}
	if got := pool.Config().HealthCheckPeriod; got != 30*time.Second {
		t.Errorf("health check period %v", got)
	}
	if got := pool.Config().MaxConnLifetime; got != 10*time.Minute {
		t.Errorf("max conn lifetime %v", got)
	}
}

func TestNewPoolKeepsTheDSNMaxConnsWhenNoneIsGiven(t *testing.T) {
	// Arrange
	ctx := context.Background()

	// Act
	pool, err := pgstore.NewPool(ctx, "postgres://u:p@127.0.0.1:1/db?sslmode=disable&pool_max_conns=9", 0, false)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if got := pool.Config().MaxConns; got != 9 {
		t.Errorf("MaxConns %d, want the DSN's 9", got)
	}
}

func TestNewPoolWithPingFailsOnAnUnreachableDatabase(t *testing.T) {
	// Arrange: a port nobody listens on refuses at once, so the failure is immediate.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Act
	pool, err := pgstore.NewPool(ctx, "postgres://u:p@127.0.0.1:1/db?sslmode=disable&connect_timeout=2", 2, true)

	// Assert
	if err == nil {
		pool.Close()
		t.Fatal("pinging an unreachable database must fail")
	}
	if pool != nil {
		t.Errorf("no pool is returned with the error: %v", pool)
	}
	if ctx.Err() != nil {
		t.Errorf("the failure must come from the ping, not the test deadline: %v", ctx.Err())
	}
}
