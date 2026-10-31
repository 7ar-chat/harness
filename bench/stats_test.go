package bench_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/ai/llm"
	"github.com/GhaziBenDahmane/harness/ai/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/bench"
	"github.com/GhaziBenDahmane/harness/config"
)

func TestTiersSplitTheBuiltinTasks(t *testing.T) {
	// Arrange
	all := bench.Builtin()

	// Act
	hard, easy := bench.FilterTier(all, bench.TierHard), bench.FilterTier(all, bench.TierEasy)

	// Assert
	if len(hard) < 8 || len(easy) < 6 || len(hard)+len(easy)+len(bench.FilterTier(all, bench.TierLong)) != len(all) {
		t.Fatalf("hard %d easy %d all %d", len(hard), len(easy), len(all))
	}
	for _, h := range hard {
		if h.Hidden == nil || h.Verify == "" || len(h.Solution) == 0 {
			t.Errorf("hard task %s needs hidden tests, a Verify command and a Solution", h.Name)
		}
	}
	if len(bench.FilterTier(all, "")) != len(hard)+len(easy) || len(bench.FilterTier(all, "all")) != len(hard)+len(easy) {
		t.Error("an empty tier or all keeps the easy and hard tasks, not the long ones")
	}
	if got := (bench.Task{}).TierOf(); got != bench.TierEasy {
		t.Errorf("default tier %q", got)
	}
}

func repeats() []bench.Result {
	return []bench.Result{
		{Task: "x", Passed: true, Steps: 4, InputTokens: 1000, CachedTokens: 600, OutputTokens: 100},
		{Task: "x", Passed: false, Steps: 10, InputTokens: 3000, CachedTokens: 0, OutputTokens: 300},
		{Task: "x", Passed: true, Steps: 6, InputTokens: 2000, CachedTokens: 1000, OutputTokens: 200},
		{Task: "y", Passed: true, Steps: 2, InputTokens: 100, CachedTokens: 100, OutputTokens: 10},
	}
}

func TestPerTaskStatsAcrossRepeats(t *testing.T) {
	// Act
	st := bench.PerTask(repeats(), bench.Prices{})

	// Assert
	if len(st) != 2 || st[0].Task != "x" {
		t.Fatalf("%+v", st)
	}
	x := st[0]
	if x.Runs != 3 || x.Passed != 2 || x.MeanSteps != 20.0/3 || x.MedianSteps != 6 {
		t.Fatalf("steps/pass %+v", x)
	}
	if x.MeanTokens != (1100+3300+2200)/3.0 || x.MedianTokens != 2200 {
		t.Fatalf("tokens %+v", x)
	}
	if got := bench.PerTask(repeats()[:2], bench.Prices{})[0]; got.MedianSteps != 7 {
		t.Fatalf("an even count takes the middle pair: %+v", got)
	}
}

func TestUncachedTokensAndCost(t *testing.T) {
	// Arrange
	p := bench.Prices{In: 3, Cached: 0.3, Out: 15}
	r := bench.Result{Steps: 4, InputTokens: 1000, CachedTokens: 600, OutputTokens: 100}

	// Act
	s := bench.SummarizeWith(repeats(), p)
	cost := p.Cost(r)

	// Assert
	if want := (400*3 + 600*0.3 + 100*15) / 1e6; cost != want {
		t.Fatalf("cost %v, want %v", cost, want)
	}
	if s.UncachedTokens != 400+3000+1000 || s.UncachedPerStep != 4400.0/22 || s.CostUSD <= 0 {
		t.Fatalf("%+v", s)
	}
	if (bench.Prices{}).Set() || bench.SummarizeWith(repeats(), bench.Prices{}).CostUSD != 0 {
		t.Fatal("no prices, no cost")
	}
}

func TestMarkdownShowsPassRateUncachedAndCost(t *testing.T) {
	// Act
	var plain, priced bytes.Buffer
	bench.Markdown(&plain, repeats())
	bench.MarkdownWith(&priced, repeats(), bench.Prices{In: 3, Cached: 0.3, Out: 15})

	// Assert
	for _, want := range []string{"pass 2/3", "pass 1/1", "uncached/step", "uncached input: 4400 tokens in total, 200 per step"} {
		if !strings.Contains(plain.String(), want) {
			t.Errorf("missing %q in:\n%s", want, plain.String())
		}
	}
	if strings.Contains(plain.String(), "cost USD") || !strings.Contains(priced.String(), "cost USD") {
		t.Errorf("the cost column belongs to priced reports only:\n%s", priced.String())
	}
}

func TestJSONKeepsEveryRunAndThePrices(t *testing.T) {
	// Arrange
	path := t.TempDir() + "/r.json"
	p := bench.Prices{In: 1, Out: 2}

	// Act
	err := bench.WriteJSONWith(path, "m", repeats(), p)
	rep, rerr := bench.ReadReport(path)

	// Assert
	if err != nil || rerr != nil || len(rep.Results) != 4 || len(rep.Tasks) != 2 || rep.Prices == nil || *rep.Prices != p {
		t.Fatalf("%v %v %+v", err, rerr, rep)
	}
	if rep.Tasks[0].Passed != 2 || rep.Summary.CostUSD <= 0 || rep.Summary.UncachedTokens != 4400 {
		t.Fatalf("%+v", rep)
	}
}

func TestCompareAggregatesRepeatsAndShowsCost(t *testing.T) {
	// Arrange
	p := bench.Prices{In: 3, Out: 15}
	before := bench.Report{Prices: &p, Results: repeats(), Summary: bench.SummarizeWith(repeats(), p)}
	better := []bench.Result{{Task: "x", Passed: true, Steps: 3, InputTokens: 900, CachedTokens: 800, OutputTokens: 90}}
	after := bench.Report{Prices: &p, Results: better, Summary: bench.SummarizeWith(better, p)}

	// Act
	var out bytes.Buffer
	bench.Compare(&out, before, after)

	// Assert
	for _, want := range []string{"2/3→1/1", "uncached/step", "cost USD", "| **total** |"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
}

func TestRunRecordsUncachedTokens(t *testing.T) {
	// Arrange
	task := mustTask(t, "code-question")
	chunks := llmtest.Text(task.SolutionAnswer)
	last := len(chunks) - 1
	chunks[last] = llmtest.WithCachedUsage(chunks[last], 1000, 700, 50)
	model := llmtest.New(llmtest.Turn{Chunks: chunks}, llmtest.Turn{Chunks: llmtest.Text("T")})

	// Act
	res := bench.Run(context.Background(), testConfig(t), llmtest.Factory{M: model}, task, bench.Options{})

	// Assert
	if res.InputTokens < 1000 || res.CachedTokens < 700 || res.UncachedTokens != res.InputTokens-res.CachedTokens ||
		res.UncachedPerStep != float64(res.UncachedTokens)/float64(res.Steps) {
		t.Fatalf("%+v", res)
	}
}

func TestSuiteRunsRepeatsInParallelInOrder(t *testing.T) {
	// Arrange
	answering := mustTask(t, "code-question")
	failing := bench.Task{Name: "zz-fails", Prompt: "nothing", Answer: []string{"never matches"}}
	missing := bench.Task{Name: "zz-missing", Prompt: "x", Needs: []string{"no-such-program-for-bench"}}
	var mu sync.Mutex
	var skipped []string
	progress := 0
	suite := bench.Suite{
		Config:   testConfig(t),
		Parallel: 3,
		ModelsFor: func(bench.Task) llm.Factory {
			return llmtest.Factory{M: llmtest.NewFunc(func(llmtest.Call) llmtest.Turn {
				return llmtest.Turn{Chunks: llmtest.Text(answering.SolutionAnswer)}
			})}
		},
		Skip:     func(tk bench.Task, _ []string) { mu.Lock(); skipped = append(skipped, tk.Name); mu.Unlock() },
		Progress: func(bench.Task, int, int, bench.Result) { progress++ }, // serialized by the suite
	}

	// Act
	got := suite.Run(context.Background(), []bench.Task{answering, missing, failing}, 3)

	// Assert
	if len(got) != 6 || progress != 6 || len(skipped) != 1 || skipped[0] != "zz-missing" {
		t.Fatalf("results %d progress %d skipped %v", len(got), progress, skipped)
	}
	for i, r := range got {
		wantTask, wantPass := "code-question", true
		if i >= 3 {
			wantTask, wantPass = "zz-fails", false
		}
		if r.Task != wantTask || r.Passed != wantPass || r.Status != "finished" {
			t.Errorf("result %d: %+v", i, r)
		}
	}
	if s := bench.Summarize(got); s.Passed != 3 || s.Runs != 6 {
		t.Fatalf("%+v", s)
	}
}

func TestSuiteStopsOnACancelledContext(t *testing.T) {
	// Arrange
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Act
	got := bench.Suite{Config: testConfig(t)}.Run(ctx, []bench.Task{mustTask(t, "code-question")}, 2)

	// Assert
	if len(got) != 0 {
		t.Fatalf("a cancelled suite ran %d tasks", len(got))
	}
}

func TestAFailedRunReportsWhyItFailed(t *testing.T) {
	// Arrange: a provider that rejects every call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"Request is missing x-session"}}`, http.StatusBadRequest)
	}))
	defer srv.Close()
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_BASE_URL", srv.URL+"/v1")
	t.Setenv("LLM_API_KEY", "k")
	t.Setenv("LLM_MODELS", "m")
	t.Setenv("LLM_DEFAULT_MODEL", "m")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	task := mustTask(t, "code-question")

	// Act
	res := bench.Run(context.Background(), cfg, nil, task, bench.Options{Timeout: 30 * time.Second})

	// Assert
	if res.Passed || res.Status != "model_error" || !strings.Contains(res.Detail, "missing x-session") {
		t.Fatalf("status %q detail %q", res.Status, res.Detail)
	}
}
