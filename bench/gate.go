package bench

import (
	"fmt"
	"io"
	"math"
	"sort"
)

// GateOptions tune Gate. The zero value is a sensible default: at least 3 runs per task, a task regresses when its
// pass rate drops by more than a third, the overall pass rate may not be significantly lower (one-sided, 10 %).
type GateOptions struct {
	// MinRuns is the fewest runs a task needs in both reports for its pass rate to count (default 3).
	MinRuns int
	// MaxTaskDrop is the per-task pass-rate drop (0..1) that counts as a regression (default 0.34).
	MaxTaskDrop float64
	// Alpha is the significance of the overall-regression test (default 0.1). Smaller is more tolerant.
	Alpha float64
	// RequireGain accepts only a change that is measurably better: a higher overall pass rate, or the same pass
	// rate with at least MinSavings fewer uncached input tokens per run. Without it, "not worse" is accepted.
	RequireGain bool
	// MinSavings is the fraction of uncached tokens per run that counts as a gain at equal pass rate
	// (default 0.05).
	MinSavings float64
}

func (o GateOptions) norm() GateOptions {
	if o.MinRuns <= 0 {
		o.MinRuns = 3
	}
	if o.MaxTaskDrop <= 0 {
		o.MaxTaskDrop = 0.34
	}
	if o.Alpha <= 0 {
		o.Alpha = 0.1
	}
	if o.MinSavings <= 0 {
		o.MinSavings = 0.05
	}
	return o
}

// Verdict is the outcome of Gate.
type Verdict struct {
	Accept bool `json:"accept"`
	// Inconclusive is set when the reports cannot support a decision (too few runs, tasks removed): the change
	// is not accepted, but it is not proven bad either.
	Inconclusive bool     `json:"inconclusive,omitempty"`
	Reasons      []string `json:"reasons"`
	Before       float64  `json:"before"`
	After        float64  `json:"after"`
	Delta        float64  `json:"delta"`
	// P is the one-sided p-value that the new pass rate is lower than the old one.
	P            float64  `json:"p"`
	Regressions  []string `json:"regressions,omitempty"`
	Improvements []string `json:"improvements,omitempty"`
}

// Gate decides whether the change that produced after may replace the one that produced before. It is deliberately
// conservative: removing or skipping a task of the baseline, too few runs, a regressed task or a significantly lower
// overall pass rate all reject. Both reports must come from the same suite; a baseline task missing from the new
// report is rejected because shrinking the suite is the easiest way to "improve" a score.
func Gate(before, after Report, o GateOptions) Verdict {
	o = o.norm()
	v := Verdict{Before: passRate(before), After: passRate(after)}
	v.Delta = v.After - v.Before

	bt, at := map[string]TaskStats{}, map[string]TaskStats{}
	for _, s := range PerTask(before.Results, Prices{}) {
		bt[s.Task] = s
	}
	for _, s := range PerTask(after.Results, Prices{}) {
		at[s.Task] = s
	}
	if len(bt) == 0 || len(at) == 0 {
		v.Inconclusive = true
		v.Reasons = append(v.Reasons, "a report has no results")
		return v
	}
	var names []string
	for n := range bt {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		b := bt[n]
		a, ok := at[n]
		switch {
		case !ok:
			v.Inconclusive = true
			v.Reasons = append(v.Reasons, fmt.Sprintf("task %s is in the baseline but missing from the new report", n))
			continue
		case b.Runs < o.MinRuns || a.Runs < o.MinRuns:
			v.Inconclusive = true
			v.Reasons = append(v.Reasons, fmt.Sprintf("task %s has %d/%d runs, need %d in both", n, b.Runs, a.Runs, o.MinRuns))
			continue
		}
		switch d := a.PassRate - b.PassRate; {
		case -d > o.MaxTaskDrop:
			v.Regressions = append(v.Regressions, fmt.Sprintf("%s %d/%d→%d/%d", n, b.Passed, b.Runs, a.Passed, a.Runs))
		case d > 0:
			v.Improvements = append(v.Improvements, fmt.Sprintf("%s %d/%d→%d/%d", n, b.Passed, b.Runs, a.Passed, a.Runs))
		}
	}

	bs, as := before.Summary, after.Summary
	if bs.Runs == 0 {
		bs = Summarize(before.Results)
	}
	if as.Runs == 0 {
		as = Summarize(after.Results)
	}
	v.P = lowerP(bs.Passed, bs.Runs, as.Passed, as.Runs)

	if len(v.Regressions) > 0 {
		v.Reasons = append(v.Reasons, fmt.Sprintf("%d task(s) regressed: %v", len(v.Regressions), v.Regressions))
	}
	if v.Delta < 0 && v.P < o.Alpha {
		v.Reasons = append(v.Reasons, fmt.Sprintf("overall pass rate fell %.1f%%→%.1f%% (p=%.3f)", 100*v.Before, 100*v.After, v.P))
		v.Regressions = append(v.Regressions, "overall")
	}
	if len(v.Regressions) > 0 {
		return v // a regression is a rejection, not an inconclusive result
	}
	if v.Inconclusive {
		return v
	}

	if o.RequireGain {
		saved := 0.0
		if bu := perRun(bs.UncachedTokens, bs.Runs); bu > 0 {
			saved = 1 - perRun(as.UncachedTokens, as.Runs)/bu
		}
		gain := v.Delta > 0 || (v.Delta == 0 && saved >= o.MinSavings)
		if !gain {
			v.Reasons = append(v.Reasons, fmt.Sprintf("no measurable gain (pass rate %+.1f points, uncached tokens %+.1f%%)", 100*v.Delta, -100*saved))
			return v
		}
	}
	v.Accept = true
	v.Reasons = append(v.Reasons, fmt.Sprintf("pass rate %.1f%%→%.1f%%, no regression", 100*v.Before, 100*v.After))
	return v
}

// WriteVerdict renders v for a log or a pull request comment.
func WriteVerdict(w io.Writer, v Verdict) {
	state := "REJECT"
	switch {
	case v.Accept:
		state = "ACCEPT"
	case v.Inconclusive:
		state = "INCONCLUSIVE"
	}
	fmt.Fprintf(w, "gate: %s (pass rate %.1f%% → %.1f%%, p(worse)=%.3f)\n", state, 100*v.Before, 100*v.After, v.P)
	for _, r := range v.Reasons {
		fmt.Fprintf(w, "  - %s\n", r)
	}
	if len(v.Improvements) > 0 {
		fmt.Fprintf(w, "  improved: %v\n", v.Improvements)
	}
}

func passRate(r Report) float64 {
	if r.Summary.Runs > 0 {
		return float64(r.Summary.Passed) / float64(r.Summary.Runs)
	}
	return Summarize(r.Results).PassRate
}

func perRun(n, runs int) float64 {
	if runs == 0 {
		return 0
	}
	return float64(n) / float64(runs)
}

// lowerP is the one-sided p-value of "the second proportion is lower than the first" (two-proportion z-test).
func lowerP(p1, n1, p2, n2 int) float64 {
	if n1 == 0 || n2 == 0 {
		return 1
	}
	a, b := float64(p1)/float64(n1), float64(p2)/float64(n2)
	pool := float64(p1+p2) / float64(n1+n2)
	se := math.Sqrt(pool * (1 - pool) * (1/float64(n1) + 1/float64(n2)))
	if se == 0 {
		if b < a {
			return 0
		}
		return 1
	}
	z := (b - a) / se
	return 0.5 * math.Erfc(-z/math.Sqrt2)
}
