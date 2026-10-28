package bench

import (
	"context"
	"sync"

	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/llm"
)

// Suite runs tasks, N times each, optionally several at once.
//
// Parallel runs share one process: wall time is still per run, but MaxRSSMiB (and to a lesser degree
// HeapMiB/SysMiB) are process-wide and cannot be attributed to a single run.
type Suite struct {
	Config *config.Config
	// ModelsFor returns the model factory of a run (nil: the real model from Config). It is called once
	// per run, possibly from several goroutines.
	ModelsFor func(Task) llm.Factory
	Options   Options
	// Parallel is the number of runs in flight (default 1).
	Parallel int
	// Skip is called (serialized) for each task skipped because a program it needs is missing.
	Skip func(t Task, missing []string)
	// Progress is called (serialized) when a run finishes; run is 1-based.
	Progress func(t Task, run, n int, r Result)
}

type job struct {
	idx, run int
	task     Task
}

// Run executes every task n times and returns the results in task order, then run order.
func (s Suite) Run(ctx context.Context, tasks []Task, n int) []Result {
	n = max(n, 1)
	var jobs []job
	for _, t := range tasks {
		if m := Missing(t); len(m) > 0 {
			if s.Skip != nil {
				s.Skip(t, m)
			}
			continue
		}
		for i := 0; i < n; i++ {
			jobs = append(jobs, job{idx: len(jobs), run: i + 1, task: t})
		}
	}
	slots := make([]*Result, len(jobs))
	var mu sync.Mutex // guards slots and serializes the callbacks
	ch := make(chan job)
	var wg sync.WaitGroup
	for w := 0; w < max(s.Parallel, 1); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ch {
				var f llm.Factory
				if s.ModelsFor != nil {
					f = s.ModelsFor(j.task)
				}
				r := Run(ctx, s.Config, f, j.task, s.Options)
				mu.Lock()
				slots[j.idx] = &r
				if s.Progress != nil {
					s.Progress(j.task, j.run, n, r)
				}
				mu.Unlock()
			}
		}()
	}
	for _, j := range jobs {
		if ctx.Err() != nil {
			break
		}
		ch <- j
	}
	close(ch)
	wg.Wait()
	var out []Result
	for _, r := range slots {
		if r != nil {
			out = append(out, *r)
		}
	}
	return out
}
