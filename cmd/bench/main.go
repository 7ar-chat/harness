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
	flag.Parse()

	if *list {
		for _, t := range bench.Builtin() {
			fmt.Printf("%-16s %s\n", t.Name, firstLine(t.Prompt))
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
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	if cfg.LLMDefaultModel == "" {
		fatal(fmt.Errorf("set LLM_DEFAULT_MODEL (and LLM_MODELS)"))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var results []bench.Result
	for _, t := range selected {
		if m := bench.Missing(t); len(m) > 0 {
			fmt.Fprintf(os.Stderr, "skip %s: needs %v on PATH\n", t.Name, m)
			continue
		}
		for i := 0; i < *n && ctx.Err() == nil; i++ {
			fmt.Fprintf(os.Stderr, "run  %s (%d/%d) …\n", t.Name, i+1, *n)
			r := bench.Run(ctx, cfg, nil, t, bench.Options{Timeout: *timeout, MaxSteps: *steps, Keep: *keep, NoCacheKey: *noKey})
			fmt.Fprintf(os.Stderr, "     %s passed=%v status=%s steps=%d %.0fs\n", t.Name, r.Passed, r.Status, r.Steps, r.WallSeconds)
			results = append(results, r)
		}
	}
	bench.Markdown(os.Stdout, results)
	if *out != "" {
		if err := bench.WriteJSON(*out, cfg.LLMDefaultModel, results); err != nil {
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
