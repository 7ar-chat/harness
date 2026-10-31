package harness_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness"
	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/ai/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/core/agui"
	"github.com/GhaziBenDahmane/harness/core/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/agent"
	"github.com/GhaziBenDahmane/harness/engine/tools"
	"github.com/GhaziBenDahmane/harness/longtest"
)

func featureConfig(t *testing.T, env map[string]string) *config.Config {
	t.Helper()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	for k, v := range env {
		t.Setenv(k, v)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func toolNamesOf(t *testing.T, h *harness.Harness) []string {
	t.Helper()
	list, cleanup, err := h.Agent.Tools.Tools(context.Background(), tools.RunContext{ThreadID: "t", RunID: "r", Login: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	var names []string
	for _, tl := range list {
		info, err := tl.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, info.Name)
	}
	return names
}

// pingHook records the tools it saw run.
type pingHook struct {
	mu   sync.Mutex
	seen []string
}

func (p *pingHook) AfterTool(_ context.Context, _ *store.Run, _, toolName string) map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen = append(p.seen, toolName)
	return nil
}

func TestACustomFeatureAddsAToolAPromptSectionAndAHook(t *testing.T) {
	// Arrange
	cfg := featureConfig(t, nil)
	hook := &pingHook{}
	var pinged bool
	ping := harness.FeatureFunc("ping", func(b *harness.Builder) error {
		b.AddTools(longtest.ExtraTool("ping", func(context.Context, string) (string, error) { pinged = true; return "pong", nil }))
		b.Section(func(context.Context, agent.Input) string { return "PING-SECTION: use ping to check the line." })
		b.ToolHook(hook)
		return nil
	})
	model := llmtest.New(
		llmtest.Turn{Chunks: []*ai.Message{llmtest.ToolCall(0, "c1", "ping", "{}")}},
		llmtest.Turn{Chunks: llmtest.Text("all good")},
		llmtest.Turn{Chunks: llmtest.Text("A title")},
	)
	h, err := harness.New(harness.Options{
		Config: cfg, Models: llmtest.Factory{M: model}, Backend: sandboxtest.NewBackend(),
		Features: append(harness.ProfileMinimal(), ping),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go func() { _ = h.Run(ctx) }()
	th, _ := h.NewThread(ctx, "alice", "")

	// Act
	run, err := h.Send(ctx, "alice", th.ID, "ping please", harness.RunOptions{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	var last string
	for ev := range h.Events(ctx, run.ID, 0) {
		last = ev.Type
	}

	// Assert
	if last != agui.RunFinished {
		t.Fatalf("last event %q", last)
	}
	if !contains(toolNamesOf(t, h), "ping") {
		t.Fatalf("tools %v lack ping", toolNamesOf(t, h))
	}
	if !pinged {
		t.Fatal("the custom tool did not run")
	}
	hook.mu.Lock()
	seen := append([]string(nil), hook.seen...)
	hook.mu.Unlock()
	if len(seen) != 1 || seen[0] != "ping" {
		t.Fatalf("the tool hook saw %v, want [ping]", seen)
	}
	var system strings.Builder
	for _, m := range model.Calls()[0].Messages {
		system.WriteString(m.Content)
	}
	if !strings.Contains(system.String(), "PING-SECTION") {
		t.Fatal("the custom prompt section is not in the first request")
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestProfilesSelectTheFeatureSets(t *testing.T) {
	// Arrange
	build := func(env map[string]string, features []harness.Feature) []string {
		h, err := harness.New(harness.Options{Config: featureConfig(t, env), Models: llmtest.Factory{M: llmtest.New()},
			Backend: sandboxtest.NewBackend(), Features: features})
		if err != nil {
			t.Fatal(err)
		}
		return toolNamesOf(t, h)
	}

	// Act
	minimal := build(nil, harness.ProfileMinimal())
	byEnv := build(map[string]string{"AGENT_PROFILE": "minimal"}, nil)
	work := build(map[string]string{"AGENT_PROFILE": "work"}, nil)
	coding := build(map[string]string{"AGENT_PROFILE": "coding"}, nil)
	codingByList := build(map[string]string{"AGENT_PROFILE": "work"}, harness.ProfileCoding())

	// Assert
	if strings.Join(minimal, ",") != strings.Join(byEnv, ",") {
		t.Fatalf("AGENT_PROFILE=minimal gives %v, ProfileMinimal gives %v", byEnv, minimal)
	}
	if contains(minimal, "memory_search") || contains(minimal, "load_skill") || !contains(minimal, "execute_command") || !contains(minimal, "update_plan") {
		t.Fatalf("minimal tools %v", minimal)
	}
	if !contains(work, "memory_search") || !contains(work, "load_skill") || contains(work, "explore") {
		t.Fatalf("work tools %v", work)
	}
	if !contains(coding, "explore") || contains(coding, "memory_search") {
		t.Fatalf("coding tools %v", coding)
	}
	if strings.Join(coding, ",") != strings.Join(codingByList, ",") {
		t.Fatalf("AGENT_PROFILE=coding gives %v, ProfileCoding gives %v", coding, codingByList)
	}
}

func TestFeaturesReportMissingCapabilitiesAndMisuse(t *testing.T) {
	// Arrange
	needsSandbox := harness.FeatureFunc("needs-sandbox", func(b *harness.Builder) error { return b.RequireSandbox() })
	cases := []struct {
		name     string
		backend  bool
		features []harness.Feature
		want     string
	}{
		{"workspace needs artifacts", true, []harness.Feature{harness.Workspace()}, `feature "workspace" requires feature "artifacts"`},
		{"explore needs the sandbox feature", true, []harness.Feature{harness.Explore()}, `feature "explore" requires feature "sandbox"`},
		{"duplicate feature", true, []harness.Feature{harness.Plan(), harness.Plan()}, `feature "plan" is listed twice`},
		{"needs a sandbox backend", false, []harness.Feature{needsSandbox}, `feature "needs-sandbox" needs a sandbox`},
		{"a feature's own error", true, []harness.Feature{harness.FeatureFunc("bad", func(*harness.Builder) error { return context.Canceled })}, `feature "bad"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts := harness.Options{Config: featureConfig(t, nil), Models: llmtest.Factory{M: llmtest.New()}, Features: c.features}
			if c.backend {
				opts.Backend = sandboxtest.NewBackend()
			}

			// Act
			_, err := harness.New(opts)

			// Assert
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %v, want it to contain %q", err, c.want)
			}
		})
	}
}
