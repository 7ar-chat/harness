package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/factorystore"
	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/adapters/store/pgstore"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/core/store"
)

func quietEnv(t *testing.T) {
	t.Helper()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	t.Setenv("SANDBOX_BACKEND", "")
	t.Setenv("OPENSANDBOX_URL", "")
}

func TestRunRefusesToStartWhenRedisCannotBeReached(t *testing.T) {
	// Arrange: nothing listens on the Redis port, the startup ping is on
	quietEnv(t)
	t.Setenv("REDIS_HOST", "127.0.0.1")
	t.Setenv("REDIS_PORT", "1")
	t.Setenv("PING_DB_AT_STARTUP", "true")

	// Act
	err := Run(context.Background(), Options{Addr: "127.0.0.1:0", Store: memstore.New(), Models: llmtest.Factory{M: llmtest.New()}})

	// Assert
	if err == nil || !strings.Contains(err.Error(), "connect to Redis") {
		t.Fatalf("err %v", err)
	}
}

func TestRunRefusesToStartWhenPostgresCannotBeReached(t *testing.T) {
	// Arrange: Redis is there, Postgres is configured but nothing listens on its port
	quietEnv(t)
	t.Setenv("POSTGRES_HOST", "127.0.0.1")
	t.Setenv("POSTGRES_PORT", "1")
	t.Setenv("PING_DB_AT_STARTUP", "true")
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	// Act
	err := Run(context.Background(), Options{Addr: "127.0.0.1:0", Redis: rdb, Models: llmtest.Factory{M: llmtest.New()}})

	// Assert
	if err == nil || !strings.Contains(err.Error(), "connect to Postgres") {
		t.Fatalf("err %v", err)
	}
}

func TestRunReportsAnAdminAddressThatIsTaken(t *testing.T) {
	// Arrange
	quietEnv(t)
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	// Act
	err = Run(context.Background(), Options{Addr: taken.Addr().String(), Standalone: true, Backend: sandboxtest.NewBackend(),
		Models: llmtest.Factory{M: llmtest.New()}})

	// Assert
	if err == nil || !strings.Contains(err.Error(), "admin listen") {
		t.Fatalf("err %v", err)
	}
}

func TestRunReportsAConfigurationThatDoesNotLoad(t *testing.T) {
	// Arrange
	quietEnv(t)
	t.Setenv("LLM_PROVIDER", "carrier-pigeon")

	// Act
	err := Run(context.Background(), Options{Standalone: true})

	// Assert
	if err == nil || !strings.Contains(err.Error(), "LLM_PROVIDER") {
		t.Fatalf("err %v", err)
	}
}

func TestRunReportsABackendTheHarnessCannotBuild(t *testing.T) {
	// Arrange
	quietEnv(t)
	t.Setenv("SANDBOX_BACKEND", "no-such-backend")

	// Act
	err := Run(context.Background(), Options{Standalone: true, Models: llmtest.Factory{M: llmtest.New()}})

	// Assert
	if err == nil || !strings.Contains(err.Error(), "no-such-backend") {
		t.Fatalf("err %v", err)
	}
}

func TestTheAdminAddressComesFromTheEnvironmentWhenNoneIsGiven(t *testing.T) {
	// Arrange
	quietEnv(t)
	t.Setenv("HARNESS_WORKER_ADDR", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, done := make(chan string, 1), make(chan error, 1)

	// Act
	go func() {
		done <- Run(ctx, Options{Standalone: true, Models: llmtest.Factory{M: llmtest.New()}, OnListening: func(a string) { addr <- a }})
	}()
	base := "http://" + <-addr
	waitFor(t, "healthz", func() bool { return status(base+"/healthz") == 200 })
	cancel()

	// Assert
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDescribeNamesWhatTheHarnessIsWiredTo(t *testing.T) {
	// Arrange
	quietEnv(t)
	build := func(env map[string]string, o Options) (*harness.Harness, *config.Config) {
		for k, v := range env {
			t.Setenv(k, v)
		}
		cfg, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		o.Models = llmtest.Factory{M: llmtest.New()}
		h, err := harness.New(harness.Options{Config: cfg, Store: o.Store, Models: o.Models, Backend: o.Backend})
		if err != nil {
			t.Fatal(err)
		}
		return h, cfg
	}
	pool, err := pgstore.NewPool(context.Background(), "postgres://u:p@127.0.0.1:1/db", 2, false)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	// Act
	none, noneCfg := build(nil, Options{})
	custom, customCfg := build(nil, Options{Backend: sandboxtest.NewBackend()})
	pg, pgCfg := build(nil, Options{Store: pgstore.NewPG(pool)})
	remote, remoteCfg := build(map[string]string{"SANDBOX_BACKEND": "", "OPENSANDBOX_URL": "http://127.0.0.1:1"}, Options{Backend: sandboxtest.NewBackend()})
	local, localCfg := build(map[string]string{"SANDBOX_BACKEND": "local", "LOCAL_WORKSPACE": t.TempDir(), "LOCAL_STATE_DIR": t.TempDir()}, Options{})

	// Assert
	cases := []struct {
		name string
		got  Wiring
		want Wiring
	}{
		{"memory, no sandbox", describe(none, noneCfg, Options{}), Wiring{Store: "memory", Queue: "memory", Sandbox: "none"}},
		{"a supplied backend", describe(custom, customCfg, Options{Backend: sandboxtest.NewBackend()}), Wiring{Store: "memory", Queue: "memory", Sandbox: "custom"}},
		{"postgres", describe(pg, pgCfg, Options{}), Wiring{Store: "postgres", Queue: "memory", Sandbox: "none"}},
		{"the configured remote backend", describe(remote, remoteCfg, Options{}), Wiring{Store: "memory", Queue: "memory", Sandbox: "opensandbox"}},
		{"the local backend", describe(local, localCfg, Options{}), Wiring{Store: "memory", Queue: "memory", Sandbox: "local"}},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: wiring %+v, want %+v", c.name, c.got, c.want)
		}
	}
}

func TestWaitIdleReturnsWhenItsContextEnds(t *testing.T) {
	// Arrange: a run is queued and nothing consumes it, so the worker is never idle
	quietEnv(t)
	cfg, _ := config.Load()
	st := memstore.New()
	h, err := harness.New(harness.Options{Config: cfg, Store: st, Models: llmtest.Factory{M: llmtest.New()}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	th, _ := st.CreateThread(ctx, "alice", "")
	if _, _, err := st.CreateRun(ctx, store.NewRun{RunID: "r1", ThreadID: th.ID, UserLogin: "alice", Model: "gpt-test",
		Message: store.Message{Role: store.RoleUser, Content: "hi"}}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})

	// Act
	go func() { waitIdle(ctx, h, newMetrics(), 0); close(done) }()
	cancel()

	// Assert
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("waitIdle ignored the cancelled context")
	}
}

func TestWaitIdleWaitsForTheDefaultIdlePeriodBeforeReturning(t *testing.T) {
	// Arrange: nothing queued, nothing running
	quietEnv(t)
	cfg, _ := config.Load()
	h, err := harness.New(harness.Options{Config: cfg, Models: llmtest.Factory{M: llmtest.New()}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.Run(ctx) }()
	start := time.Now()

	// Act
	waitIdle(ctx, h, newMetrics(), 0)

	// Assert: a zero idle period means one second of calm, not an immediate return
	if took := time.Since(start); took < time.Second {
		t.Fatalf("returned after %v, want at least the 1 s default", took)
	}
}

func TestFeaturesOptionReplacesTheProfileAndItsCleanupRunsWhenTheWorkerStops(t *testing.T) {
	// Arrange: the factory is the feature a worker must install to finish the runs of factory tasks
	quietEnv(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	var gotStore harness.Store
	released := 0
	o := Options{
		Standalone: true, Backend: sandboxtest.NewBackend(), Models: llmtest.Factory{M: llmtest.New()},
		Features: func(_ context.Context, _ *config.Config, st harness.Store) ([]harness.Feature, func(), error) {
			gotStore = st
			return []harness.Feature{harness.Sandbox(), harness.FactoryFeature(harness.FactoryOptions{Store: factorystore.NewMem(nil)})},
				func() { released++ }, nil
		},
	}

	// Act
	b, err := buildHarness(context.Background(), cfg, o, newMetrics())
	if err != nil {
		t.Fatal(err)
	}
	installed := b.h.FactoryOf() != nil
	releasedWhileRunning := released
	b.close()

	// Assert: the features were built over the store the harness uses and are released once, at the end
	if !installed {
		t.Error("the factory the Features option returned is not installed")
	}
	if gotStore == nil || gotStore != b.h.Store {
		t.Errorf("Features got store %v, the harness uses %v", gotStore, b.h.Store)
	}
	if releasedWhileRunning != 0 || released != 1 {
		t.Errorf("released %d times while running, %d after close; want 0 and 1", releasedWhileRunning, released)
	}
}

func TestMCPServersOptionIsBuiltOverTheWorkersStoreAndReleasedOnStopAndOnFailure(t *testing.T) {
	// Arrange
	quietEnv(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	var gotStore harness.Store
	released := 0
	o := Options{
		Standalone: true, Backend: sandboxtest.NewBackend(), Models: llmtest.Factory{M: llmtest.New()},
		MCPServers: func(_ context.Context, _ *config.Config, st harness.Store) (harness.MCPServersFunc, func(), error) {
			gotStore = st
			return func(context.Context, string, string) ([]config.MCPServer, error) { return nil, nil }, func() { released++ }, nil
		},
	}

	// Act
	b, err := buildHarness(context.Background(), cfg, o, newMetrics())
	if err != nil {
		t.Fatal(err)
	}
	whileRunning := released
	b.close()

	// Assert
	if gotStore == nil || gotStore != b.h.Store {
		t.Errorf("the source got store %v, the harness uses %v", gotStore, b.h.Store)
	}
	if whileRunning != 0 || released != 1 {
		t.Errorf("released %d while running, %d after close; want 0 and 1", whileRunning, released)
	}

	// A source that fails to build stops the worker, and what it opened is still released.
	released = 0
	o.MCPServers = func(context.Context, *config.Config, harness.Store) (harness.MCPServersFunc, func(), error) {
		return nil, func() { released++ }, errors.New("no database")
	}
	if _, err := buildHarness(context.Background(), cfg, o, newMetrics()); err == nil || released != 1 {
		t.Errorf("a failing source: err %v, released %d", err, released)
	}
}

func TestWithoutFeaturesTheWorkerBuildsTheProfileOfTheConfiguration(t *testing.T) {
	// Arrange
	quietEnv(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	// Act
	b, err := buildHarness(context.Background(), cfg, Options{Standalone: true, Backend: sandboxtest.NewBackend(),
		Models: llmtest.Factory{M: llmtest.New()}}, newMetrics())
	if err != nil {
		t.Fatal(err)
	}
	defer b.close()

	// Assert
	if b.h.FactoryOf() != nil {
		t.Error("the factory is installed without the Features option")
	}
}

func TestAFeaturesFailureIsReportedAndEverythingOpenedSoFarIsReleased(t *testing.T) {
	// Arrange
	quietEnv(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	released := 0
	boom := errors.New("no factory tables")
	o := Options{
		Standalone: true, Backend: sandboxtest.NewBackend(), Models: llmtest.Factory{M: llmtest.New()},
		Features: func(context.Context, *config.Config, harness.Store) ([]harness.Feature, func(), error) {
			return nil, func() { released++ }, boom
		},
	}

	// Act
	_, err = buildHarness(context.Background(), cfg, o, newMetrics())

	// Assert: wrapped, and the release the failed builder returned still ran
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "build the features") {
		t.Errorf("err = %v", err)
	}
	if released != 1 {
		t.Errorf("released %d times, want 1", released)
	}
}

func TestRunStopsAFeaturesCleanupAfterTheWorkerHasDrained(t *testing.T) {
	// Arrange
	quietEnv(t)
	released := make(chan struct{})
	o := Options{
		Addr: "127.0.0.1:0", Standalone: true, Once: true, Backend: sandboxtest.NewBackend(), Models: llmtest.Factory{M: llmtest.New()},
		Features: func(context.Context, *config.Config, harness.Store) ([]harness.Feature, func(), error) {
			return harness.ProfileMinimal(), func() { close(released) }, nil
		},
	}

	// Act
	err := Run(context.Background(), o)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-released:
	default:
		t.Error("the cleanup of the features did not run when Run returned")
	}
}
