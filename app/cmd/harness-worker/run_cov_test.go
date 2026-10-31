package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// wkEnv gives the worker a loadable, self-contained configuration.
func wkEnv(t *testing.T) {
	t.Helper()
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	t.Setenv("HARNESS_WORKER_ADDR", "")
	t.Setenv("RUNTIME_CONCURRENCY", "") // restored after the test: run exports the -concurrency flag
}

func TestRunOnceDrainsAnEmptyQueueAndExitsZeroAndExportsTheConcurrencyFlag(t *testing.T) {
	// Arrange
	wkEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var stderr bytes.Buffer

	// Act
	code := run(ctx, []string{"-standalone", "-once", "-addr", "127.0.0.1:0", "-concurrency", "7", "-id", "w-test"}, &stderr)

	// Assert: the flag reached the configuration the worker started with
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if log := stderr.String(); !strings.Contains(log, "concurrency=7") || !strings.Contains(log, "worker stopped") {
		t.Fatalf("worker log:\n%s", log)
	}
}

func TestRunServesTheAdminEndpointsUntilTheContextEnds(t *testing.T) {
	// Arrange: a free loopback port for the admin server
	wkEnv(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	var stderr bytes.Buffer

	// Act
	go func() { done <- run(ctx, []string{"-standalone", "-addr", addr}, &stderr) }()
	var status int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get("http://" + addr + "/readyz"); err == nil {
			resp.Body.Close()
			status = resp.StatusCode
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()

	// Assert
	if status != http.StatusOK {
		t.Fatalf("readyz = %d", status)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the worker did not stop")
	}
}

func TestRunReportsStartupFailuresWithExitCodeOne(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	cases := []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{"invalid configuration", map[string]string{"LLM_PROVIDER": "magic"}, []string{"-standalone"}, "LLM_PROVIDER"},
		{"private queue without -standalone", map[string]string{"PING_DB_AT_STARTUP": "false"}, nil, "POSTGRES_HOST"},
		{"admin address already taken", nil, []string{"-standalone", "-addr", busy.Addr().String()}, "admin listen"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			wkEnv(t)
			t.Setenv("POSTGRES_HOST", "") // restores the original on cleanup; unset below: the check is on presence
			_ = os.Unsetenv("POSTGRES_HOST")
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			var stderr bytes.Buffer

			// Act
			code := run(context.Background(), c.args, &stderr)

			// Assert
			if code != 1 || !strings.Contains(stderr.String(), "harness-worker:") || !strings.Contains(stderr.String(), c.want) {
				t.Fatalf("exit %d stderr %q, want it to mention %q", code, stderr.String(), c.want)
			}
		})
	}
}

func TestRunFlagParsing(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"help exits zero", []string{"-h"}, 0},
		{"unknown flag is a usage error", []string{"-nope"}, 2},
		{"a malformed value is a usage error", []string{"-concurrency", "many"}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wkEnv(t)
			var stderr bytes.Buffer

			code := run(context.Background(), c.args, &stderr)

			if code != c.want {
				t.Fatalf("exit %d, want %d: %s", code, c.want, stderr.String())
			}
			if c.want != 0 && !strings.Contains(stderr.String(), "-concurrency") {
				t.Errorf("no usage printed: %q", stderr.String())
			}
		})
	}
}
