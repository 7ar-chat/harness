# bench

`bench/` runs coding tasks through the real harness (local sandbox, coding profile) and reports pass/fail,
steps, tool calls, tokens, prompt-cache hits, wall time and memory. Each task has hidden tests restored
before verification and a reference solution; `go test ./bench` proves every task fails untouched and
passes with its solution, and exercises the runner with a scripted model (no network).

```
bench -list
bench -tasks go-fix-bugs,py-fix-bugs -n 3 -out run.json     # model from LLM_* env
bench -tier hard -n 5 -parallel 4 -out hard.json             # the hard tier, 5 repeats, 4 runs at once
bench -price-in 3 -price-cached 0.3 -price-out 15 ...        # USD per 1M tokens: adds an estimated cost
bench -compare before.json after.json                        # before/after a harness change
go test -tags live ./bench -run TestLive -v                  # same, as a test (BENCH_TASKS, BENCH_OUT)
```

Tasks have a tier: `easy` (six small tasks a small model solves in a few steps) and `hard` (multi-file features,
a bug far from its symptom, a data race checked with `go test -race`, a cross-package refactor, repository
conventions in AGENTS.md, a config format migration, a misleading README, a Node task; Python/Node/make tasks are
skipped when the program is missing). Use `-tier easy|hard` to filter and `bench -list` to see the tier.

Reports are meant for repeats (`-n`): the Markdown output has a per-task table with the pass rate (`pass 2/3`),
mean/median steps and tokens, and the JSON report keeps every run plus the per-task aggregates. Besides total
input tokens it reports uncached input per step, `(input - cached) / steps`, which is what prompt caching does
not absorb, and, when prices are given, an estimated cost; `-compare` shows both. `-parallel N` runs N tasks at
once (each run has its own working directory and state); wall time is per run, but peak RSS is process-wide, so
use `-parallel 1` for memory comparisons.

