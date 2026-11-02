//go:build live

package bench_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/bench"
	"github.com/7ar-chat/harness/config"
)

// TestLive runs the built-in tasks against the model in the environment (config.Load: LLM_PROVIDER,
// LLM_BASE_URL, LLM_API_KEY, LLM_MODELS, LLM_DEFAULT_MODEL …). BENCH_TASKS selects tasks (comma
// separated), BENCH_OUT writes the JSON report, BENCH_MIN_PASS (default 0) is the pass count required.
//
//	go test -tags live ./bench -run TestLive -timeout 60m -v
func TestLive(t *testing.T) {
	cfg, err := config.Load()
	if err != nil || cfg.LLMDefaultModel == "" {
		t.Skipf("no model configured (%v)", err)
	}
	var names []string
	for _, n := range strings.Split(os.Getenv("BENCH_TASKS"), ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	tasks, missing := bench.Select(names)
	if len(names) == 0 {
		tasks = bench.FilterTier(tasks, os.Getenv("BENCH_TIER"))
	}
	if len(missing) > 0 {
		t.Fatalf("unknown tasks %v", missing)
	}
	var results []bench.Result
	for _, task := range tasks {
		if m := bench.Missing(task); len(m) > 0 {
			t.Logf("skip %s: needs %v", task.Name, m)
			continue
		}
		task := task
		t.Run(task.Name, func(t *testing.T) {
			r := bench.Run(context.Background(), cfg, nil, task, bench.Options{NoCacheKey: os.Getenv("BENCH_NO_CACHE_KEY") != ""})
			results = append(results, r)
			t.Logf("passed=%v status=%s steps=%d tools=%d(%d err) in=%d cached=%.0f%% out=%d %.0fs heap=%.0fMiB",
				r.Passed, r.Status, r.Steps, r.ToolCalls, r.ToolErrors, r.InputTokens, r.CacheHitPct, r.OutputTokens, r.WallSeconds, r.HeapMiB)
			if !r.Passed {
				t.Logf("verify: %s\nanswer: %s", r.VerifyOutput, r.FinalAnswer)
			}
		})
	}
	var sb strings.Builder
	bench.Markdown(&sb, results)
	t.Log("\n" + sb.String())
	if out := os.Getenv("BENCH_OUT"); out != "" {
		if err := bench.WriteJSON(out, cfg.LLMDefaultModel, results); err != nil {
			t.Fatal(err)
		}
	}
	min := 0
	if v := os.Getenv("BENCH_MIN_PASS"); v != "" {
		for _, c := range v {
			min = min*10 + int(c-'0')
		}
	}
	if s := bench.Summarize(results); s.Passed < min {
		t.Fatalf("%d tasks passed, want at least %d", s.Passed, min)
	}
}
