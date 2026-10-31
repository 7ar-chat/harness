package redisinfra_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/redisx"
	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/engine/queue"
	"github.com/7ar-chat/harness/redisinfra"
)

func TestInfraGivesTheHarnessAQueueAndEventLogThatLiveInRedis(t *testing.T) {
	// Arrange
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "m")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	st := memstore.New()
	infra := redisinfra.Infra(rdb, st)
	h, err := harness.New(harness.Options{
		Config: cfg, Store: st, Infra: &infra, Backend: sandboxtest.NewBackend(),
		Models: llmtest.Factory{M: llmtest.New(llmtest.Turn{Chunks: llmtest.Text("hi")}, llmtest.Turn{Chunks: llmtest.Text("title")})},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go func() { _ = h.Run(ctx) }()

	// Act
	th, _ := h.NewThread(ctx, "alice", "")
	run, err := h.Send(ctx, "alice", th.ID, "Say hi", harness.RunOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	last := ""
	for ev := range h.Events(ctx, run.ID, 0) {
		last = ev.Type
	}

	// Assert
	if last != agui.RunFinished {
		t.Fatalf("last event %q", last)
	}
	if !mr.Exists(redisx.EventKey(run.ID)) || !mr.Exists(queue.Stream) {
		t.Fatal("the run's events and queue entry are not in Redis")
	}
}

func TestNewClientBuildsAStandaloneClientFromTheConfiguration(t *testing.T) {
	mr := miniredis.RunT(t)
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "m")
	t.Setenv("REDIS_HOST", mr.Host())
	t.Setenv("REDIS_PORT", mr.Port())
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	rdb := redisinfra.NewClient(cfg)
	defer rdb.Close()

	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("ping through the client built from REDIS_HOST/REDIS_PORT: %v", err)
	}
}
