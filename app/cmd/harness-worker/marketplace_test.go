package main

import (
	"context"
	"os"
	"testing"

	"github.com/7ar-chat/harness/config"
)

func TestTheWorkerHasTheMarketplaceOnlyWithASharedPostgres(t *testing.T) {
	// Arrange: a lazy pool (nothing connects at startup); the catalog seed fails against nothing and is only logged
	wkEnv(t)
	t.Setenv("PING_DB_AT_STARTUP", "false")
	t.Setenv("POSTGRES_PORT", "1")
	t.Setenv("POSTGRES_SSLMODE", "disable")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	getenv := func(k string) string { return map[string]string{"HARNESS_SECRET_KEY": "k"}[k] }

	// Act / Assert: a standalone worker has no shared state to read
	if marketplaceServers(getenv, true) != nil {
		t.Fatal("a standalone worker got a marketplace")
	}
	// Without POSTGRES_HOST there is nowhere to read the installs from.
	t.Setenv("POSTGRES_HOST", "")
	_ = os.Unsetenv("POSTGRES_HOST")
	if src, release, err := marketplaceServers(getenv, false)(ctx, cfg, nil); err != nil || src != nil || release != nil {
		t.Fatalf("without Postgres: %v %v %v", src != nil, release != nil, err)
	}
	// With it: a source and the release of the pool it opened.
	t.Setenv("POSTGRES_HOST", "127.0.0.1")
	src, release, err := marketplaceServers(getenv, false)(ctx, cfg, nil)
	if err != nil || src == nil || release == nil {
		t.Fatalf("with Postgres: %v %v %v", src != nil, release != nil, err)
	}
	release()
}

func TestTheWorkersMarketplaceReportsAPoolItCannotOpen(t *testing.T) {
	// Arrange
	wkEnv(t)
	t.Setenv("POSTGRES_HOST", "127.0.0.1")
	t.Setenv("POSTGRES_PORT", "not-a-port")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	// Act
	_, release, err := marketplaceServers(func(string) string { return "k" }, false)(context.Background(), cfg, nil)

	// Assert
	if err == nil || release != nil {
		t.Fatalf("err %v, release %v", err, release != nil)
	}
}

func TestTheWorkersMarketplaceNeedsASecretKeyToReadCredentialsButNotToStart(t *testing.T) {
	// Arrange
	wkEnv(t)
	t.Setenv("PING_DB_AT_STARTUP", "false")
	t.Setenv("POSTGRES_HOST", "127.0.0.1")
	t.Setenv("POSTGRES_SSLMODE", "disable")
	cfg, _ := config.Load()

	// Act
	src, release, err := marketplaceServers(func(string) string { return "" }, false)(context.Background(), cfg, nil)

	// Assert
	if err != nil || src == nil {
		t.Fatalf("a worker without a key must still start: %v", err)
	}
	release()
}
