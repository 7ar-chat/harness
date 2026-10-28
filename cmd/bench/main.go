// Command bench runs the coding benchmark (package bench) against the model in the environment
// (LLM_PROVIDER, LLM_BASE_URL, LLM_API_KEY, LLM_MODELS, LLM_DEFAULT_MODEL …, as config.Load reads them).
//
//	bench -list
//	bench -tasks go-fix-bugs,py-fix-bugs -n 3 -out run.json
//	bench -compare before.json after.json
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/GhaziBenDahmane/harness/bench"
	"github.com/GhaziBenDahmane/harness/config"
)

func main() {
	tasks := flag.String("tasks", "", "comma-separated task names (default: all)")
	n := flag.Int("n", 1, "runs per task")
	out := flag.String("out", "", "write the JSON report here")
	list := flag.Bool("list", false, "list the tasks")
	compare := flag.Bool("compare", false, "compare two JSON reports given as arguments (before after)")
	steps := flag.Int("steps", 0, "override AGENT_MAX_STEPS")
	timeout := flag.Duration("timeout", 10*time.Minute, "per-task timeout")
	keep := flag.Bool("keep", false, "keep the working directories")
	noKey := flag.Bool("no-cache-key", false, "send no prompt_cache_key")
	tier := flag.String("tier", "", "only tasks of this tier: easy, hard or long (default: easy and hard)")
	parallel := flag.Int("parallel", 1, "runs in flight at once (peak RSS is process-wide)")
	priceIn := flag.Float64("price-in", 0, "USD per 1M uncached input tokens (enables the cost estimate)")
	priceCached := flag.Float64("price-cached", 0, "USD per 1M cached input tokens")
	priceOut := flag.Float64("price-out", 0, "USD per 1M output tokens")
	flag.Parse()
	if *tier != "" && *tier != "all" && *tier != bench.TierEasy && *tier != bench.TierHard && *tier != bench.TierLong {
		fatal(fmt.Errorf("-tier must be easy, hard, long or all"))
	}

	if *list {
		for _, t := range bench.FilterTier(bench.Builtin(), *tier) {
			fmt.Printf("%-20s %-5s %s\n", t.Name, t.TierOf(), firstLine(t.Prompt))
		}
		return
	}
	if *compare {
		if flag.NArg() != 2 {
			fatal(fmt.Errorf("-compare needs two report files"))
		}
		a, err1 := bench.ReadReport(flag.Arg(0))
		b, err2 := bench.ReadReport(flag.Arg(1))
		if err1 != nil || err2 != nil {
			fatal(fmt.Errorf("%v %v", err1, err2))
		}
		bench.Compare(os.Stdout, a, b)
		return
	}

	var names []string
	for _, s := range strings.Split(*tasks, ",") {
		if s = strings.TrimSpace(s); s != "" {
			names = append(names, s)
		}
	}
	selected, missing := bench.Select(names)
	if len(missing) > 0 {
		fatal(fmt.Errorf("unknown tasks %v (have: %s)", missing, bench.Names()))
	}
	selected = bench.FilterTier(selected, *tier)
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	if cfg.LLMDefaultModel == "" {
		fatal(fmt.Errorf("set LLM_DEFAULT_MODEL (and LLM_MODELS)"))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	prices := bench.Prices{In: *priceIn, Cached: *priceCached, Out: *priceOut}
	suite := bench.Suite{
		Config:   cfg,
		Options:  bench.Options{Timeout: *timeout, MaxSteps: *steps, Keep: *keep, NoCacheKey: *noKey},
		Parallel: *parallel,
		Skip:     func(t bench.Task, m []string) { fmt.Fprintf(os.Stderr, "skip %s: needs %v on PATH\n", t.Name, m) },
		Progress: func(t bench.Task, run, n int, r bench.Result) {
			fmt.Fprintf(os.Stderr, "done %s (%d/%d) passed=%v status=%s steps=%d %.0fs\n", t.Name, run, n, r.Passed, r.Status, r.Steps, r.WallSeconds)
			if r.Detail != "" {
				fmt.Fprintf(os.Stderr, "     detail: %s\n", r.Detail)
			}
		},
	}
	results := suite.Run(ctx, selected, *n)
	bench.MarkdownWith(os.Stdout, results, prices)
	if *out != "" {
		if err := bench.WriteJSONWith(*out, cfg.LLMDefaultModel, results, prices); err != nil {
			fatal(err)
		}
	}
	for _, r := range results {
		if !r.Passed {
			os.Exit(1)
		}
	}
}

func firstLine(s string) string {
	if len(s) > 90 {
		return s[:90] + "…"
	}
	return s
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "bench:", err)
	os.Exit(2)
}
