package main

import (
	"os"
	"testing"
)

func TestWithUsageLeavesAStandaloneWorkerAlone(t *testing.T) {
	// A standalone worker has no database to write a ledger to, whatever the environment says.
	t.Setenv("POSTGRES_HOST", "db.example")
	if got := withUsage(nil, true); got != nil {
		t.Fatal("a standalone worker was given a usage ledger")
	}
}

func TestWithUsageWithoutPostgresKeepsTheFeaturesAsTheyAre(t *testing.T) {
	t.Setenv("POSTGRES_HOST", "")
	// An empty value still counts as set for LookupEnv; unset it for real.
	if err := os.Unsetenv("POSTGRES_HOST"); err != nil {
		t.Fatal(err)
	}
	if got := withUsage(nil, false); got != nil {
		t.Fatal("a worker without Postgres was given a usage ledger")
	}
}
