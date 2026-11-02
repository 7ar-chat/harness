package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/store/factorystore"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/factory"
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

func wkGetenv(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestTheFactoryIsInstalledOnlyWhenSwitchedOn(t *testing.T) {
	// Arrange / Act
	off := factoryFeatures(wkGetenv(map[string]string{"FACTORY": "true"}), true)
	on := factoryFeatures(wkGetenv(map[string]string{"FACTORY": "1"}), true)

	// Assert
	if off != nil {
		t.Error("the worker builds a factory without FACTORY=1")
	}
	if on == nil {
		t.Fatal("the worker does not build a factory with FACTORY=1")
	}
}

func TestAWorkerExecutesRunsAndLeavesTheDispatcherToTheWebTierUnlessToldOtherwise(t *testing.T) {
	// Arrange
	wkEnv(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	vars := func(extra map[string]string) func(string) string {
		m := map[string]string{"FACTORY": "1", "FACTORY_POLICY": "supervised", "FACTORY_MAX_ATTEMPTS": "5", "FACTORY_NO_REVIEW": "1"}
		for k, v := range extra {
			m[k] = v
		}
		return wkGetenv(m)
	}

	// Act
	def, err1 := factoryOptions(vars(nil), cfg, factorystore.NewMem(nil), nil)
	opted, err2 := factoryOptions(vars(map[string]string{"FACTORY_RUN": "1"}), cfg, factorystore.NewMem(nil), nil)
	zero, err3 := factoryOptions(vars(map[string]string{"FACTORY_RUN": "0"}), cfg, factorystore.NewMem(nil), nil)

	// Assert: the same settings as the web tier, a dispatcher only on request, no product attachments.
	if err1 != nil || err2 != nil || err3 != nil {
		t.Fatalf("errors: %v, %v, %v", err1, err2, err3)
	}
	if def.Run || zero.Run || !opted.Run {
		t.Errorf("Run: default %v, FACTORY_RUN=0 %v, FACTORY_RUN=1 %v; want false, false, true", def.Run, zero.Run, opted.Run)
	}
	if !def.Policy || def.DefaultPolicy != factory.PolicySupervised || def.MaxAttempts != 5 {
		t.Errorf("the FACTORY_* settings did not reach the options: %+v", def)
	}
	if def.OnSpawn == nil || def.Escalator == nil {
		t.Error("the escalator and the spawn hook must exist (they only log without a team store)")
	}
}

func TestAFactoryMisconfigurationIsReportedAtStartup(t *testing.T) {
	cases := map[string]map[string]string{
		"bad policy":          {"FACTORY": "1", "FACTORY_POLICY": "anarchy"},
		"unusable GitHub App": {"FACTORY": "1", "GITHUB_APP_ID": "1"},
	}
	for name, vars := range cases {
		t.Run(name, func(t *testing.T) {
			// Arrange
			wkEnv(t)
			cfg, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}

			// Act
			fs, release, err := factoryFeatures(wkGetenv(vars), true)(context.Background(), cfg, nil)

			// Assert
			if err == nil || fs != nil || release != nil {
				t.Errorf("features %v release %v err %v, want an error and nothing to release", fs, release != nil, err)
			}
		})
	}
}

func TestAFactoryWithAGitHubAppAndNoReviewerModelStartsAndLetsTheDefaultModelReview(t *testing.T) {
	// Arrange: the setup that used to be a startup error. A missing FACTORY_REVIEW_MODEL is now a choice (the default
	// model reviews, and says so on every approval), so the worker must come up.
	wkEnv(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{"FACTORY": "1", "GITHUB_APP_ID": "1", "GITHUB_APP_PRIVATE_KEY": string(wkKeyPEM(t))}

	// Act
	fs, release, err := factoryFeatures(wkGetenv(vars), true)(context.Background(), cfg, nil)
	if release != nil {
		defer release()
	}

	// Assert
	if err != nil || len(fs) == 0 {
		t.Errorf("features %v err %v; want the factory installed", fs, err)
	}
}

func TestTheFactoryOfAStandaloneWorkerLivesInMemoryAndOneWithAnUnreachablePostgresFailsToStart(t *testing.T) {
	// Arrange
	wkEnv(t)
	t.Setenv("POSTGRES_HOST", "127.0.0.1")
	t.Setenv("POSTGRES_PORT", "1")
	t.Setenv("PING_DB_AT_STARTUP", "true")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	vars := wkGetenv(map[string]string{"FACTORY": "1"})

	// Act
	standalone, release, errStandalone := factoryFeatures(vars, true)(context.Background(), cfg, nil)
	_, noRelease, errPG := factoryFeatures(vars, false)(context.Background(), cfg, nil)

	// Assert
	if errStandalone != nil || len(standalone) == 0 || release == nil {
		t.Fatalf("standalone: %v features, err %v", len(standalone), errStandalone)
	}
	release()
	if errPG == nil || !strings.Contains(errPG.Error(), "Postgres") || noRelease != nil {
		t.Errorf("err = %v, want a Postgres connection error and nothing to release", errPG)
	}
}

func TestAStandaloneWorkerWithTheFactoryStartsWithoutADispatcherAndStops(t *testing.T) {
	// Arrange: a standalone worker keeps the factory in memory
	wkEnv(t)
	t.Setenv("FACTORY", "1")
	t.Setenv("FACTORY_NO_REVIEW", "1")
	t.Setenv("HARNESS_MODEL_PRICES", "off")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var stderr bytes.Buffer

	// Act
	code := run(ctx, []string{"-standalone", "-once", "-addr", "127.0.0.1:0", "-id", "w-factory"}, &stderr)

	// Assert
	if code != 0 || !strings.Contains(stderr.String(), "agent factory installed") || !strings.Contains(stderr.String(), "dispatcher=false") {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
}

var (
	wkKeyOnce  sync.Once
	wkKeyBytes []byte
)

// wkKeyPEM is a throw-away GitHub App key.
func wkKeyPEM(t *testing.T) []byte {
	t.Helper()
	wkKeyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		wkKeyBytes = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	})
	return wkKeyBytes
}
