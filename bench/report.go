package bench

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// Prices are USD per million tokens; the zero value means "unpriced" (no cost is shown).
type Prices struct {
	In     float64 `json:"in"`
	Cached float64 `json:"cached"`
	Out    float64 `json:"out"`
}

// Set reports whether any price is given.
func (p Prices) Set() bool { return p.In > 0 || p.Cached > 0 || p.Out > 0 }

// Cost estimates the USD cost of a run: uncached input at In, cached input at Cached, output at Out.
func (p Prices) Cost(r Result) float64 {
	u, _ := uncached(r.InputTokens, r.CachedTokens, r.Steps)
	return (float64(u)*p.In + float64(r.CachedTokens)*p.Cached + float64(r.OutputTokens)*p.Out) / 1e6
}

// Summary aggregates the results of a benchmark.
type Summary struct {
	Runs            int     `json:"runs"`
	Passed          int     `json:"passed"`
	PassRate        float64 `json:"passRate"`
	Steps           int     `json:"steps"`
	ToolCalls       int     `json:"toolCalls"`
	ToolErrors      int     `json:"toolErrors"`
	InputTokens     int     `json:"inputTokens"`
	OutputTokens    int     `json:"outputTokens"`
	CachedTokens    int     `json:"cachedTokens"`
	CacheHitPct     float64 `json:"cacheHitPct"`
	UncachedTokens  int     `json:"uncachedTokens"`
	UncachedPerStep float64 `json:"uncachedPerStep"`
	// CostUSD is the estimated cost; zero unless prices were given.
	CostUSD     float64 `json:"costUSD,omitempty"`
	WallSeconds float64 `json:"wallSeconds"`
	MaxRSSMiB   float64 `json:"maxRSSMiB"`
}

// Summarize totals results (skipped runs are not counted by the caller).
func Summarize(rs []Result) Summary { return SummarizeWith(rs, Prices{}) }

// SummarizeWith is Summarize plus the estimated cost at p.
func SummarizeWith(rs []Result, p Prices) Summary {
	var s Summary
	for _, r := range rs {
		s.Runs++
		if r.Passed {
			s.Passed++
		}
		s.Steps += r.Steps
		s.ToolCalls += r.ToolCalls
		s.ToolErrors += r.ToolErrors
		s.InputTokens += r.InputTokens
		s.OutputTokens += r.OutputTokens
		s.CachedTokens += r.CachedTokens
		u, _ := uncached(r.InputTokens, r.CachedTokens, r.Steps)
		s.UncachedTokens += u
		s.CostUSD += p.Cost(r)
		s.WallSeconds += r.WallSeconds
		s.MaxRSSMiB = max(s.MaxRSSMiB, r.MaxRSSMiB)
	}
	if s.Runs > 0 {
		s.PassRate = float64(s.Passed) / float64(s.Runs)
	}
	if s.InputTokens > 0 {
		s.CacheHitPct = 100 * float64(s.CachedTokens) / float64(s.InputTokens)
	}
	if s.Steps > 0 {
		s.UncachedPerStep = float64(s.UncachedTokens) / float64(s.Steps)
	}
	return s
}

// TaskStats aggregates the repeated runs of one task.
type TaskStats struct {
	Task   string `json:"task"`
	Runs   int    `json:"runs"`
	Passed int    `json:"passed"`
	// PassRate is Passed/Runs.
	PassRate     float64 `json:"passRate"`
	MeanSteps    float64 `json:"meanSteps"`
	MedianSteps  float64 `json:"medianSteps"`
	MeanTokens   float64 `json:"meanTokens"`   // input + output
	MedianTokens float64 `json:"medianTokens"` // input + output
	MeanUncached float64 `json:"meanUncachedPerStep"`
	MeanCostUSD  float64 `json:"meanCostUSD,omitempty"`
}

// PerTask groups results by task (sorted by name) and returns the pass rate, mean and median steps and
// tokens of the runs of each.
func PerTask(rs []Result, p Prices) []TaskStats {
	groups := map[string][]Result{}
	for _, r := range rs {
		groups[r.Task] = append(groups[r.Task], r)
	}
	var out []TaskStats
	for name, g := range groups {
		st := TaskStats{Task: name, Runs: len(g)}
		var steps, toks, unc []float64
		var cost float64
		for _, r := range g {
			if r.Passed {
				st.Passed++
			}
			steps = append(steps, float64(r.Steps))
			toks = append(toks, float64(r.InputTokens+r.OutputTokens))
			_, per := uncached(r.InputTokens, r.CachedTokens, r.Steps)
			unc = append(unc, per)
			cost += p.Cost(r)
		}
		st.PassRate = float64(st.Passed) / float64(st.Runs)
		st.MeanSteps, st.MedianSteps = mean(steps), median(steps)
		st.MeanTokens, st.MedianTokens = mean(toks), median(toks)
		st.MeanUncached = mean(unc)
		st.MeanCostUSD = cost / float64(st.Runs)
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Task < out[j].Task })
	return out
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	c := append([]float64(nil), xs...)
	sort.Float64s(c)
	if n := len(c); n%2 == 0 {
		return (c[n/2-1] + c[n/2]) / 2
	}
	return c[len(c)/2]
}

// Markdown renders results as a table with a total row, then a per-task table of pass rates.
func Markdown(w io.Writer, rs []Result) { MarkdownWith(w, rs, Prices{}) }

// MarkdownWith is Markdown with the estimated cost at p (shown when any price is set).
func MarkdownWith(w io.Writer, rs []Result, p Prices) {
	cost := p.Set()
	head, sep := "| task | pass | status | steps | tools (err) | in tok | cached | uncached/step | out tok | time s | heap MiB |", "|---|---|---|--:|--:|--:|--:|--:|--:|--:|--:|"
	if cost {
		head, sep = head+" cost USD |", sep+"--:|"
	}
	fmt.Fprintln(w, head)
	fmt.Fprintln(w, sep)
	for _, r := range rs {
		mark := "FAIL"
		if r.Passed {
			mark = "pass"
		}
		_, per := uncached(r.InputTokens, r.CachedTokens, r.Steps)
		fmt.Fprintf(w, "| %s | %s | %s | %d | %d (%d) | %d | %.0f%% | %.0f | %d | %.1f | %.0f |", r.Task, mark, r.Status, r.Steps,
			r.ToolCalls, r.ToolErrors, r.InputTokens, r.CacheHitPct, per, r.OutputTokens, r.WallSeconds, r.HeapMiB)
		if cost {
			fmt.Fprintf(w, " %.4f |", p.Cost(r))
		}
		fmt.Fprintln(w)
	}
	s := SummarizeWith(rs, p)
	fmt.Fprintf(w, "| **total** | **%d/%d** | | %d | %d (%d) | %d | %.0f%% | %.0f | %d | %.1f | peak RSS %.0f |",
		s.Passed, s.Runs, s.Steps, s.ToolCalls, s.ToolErrors, s.InputTokens, s.CacheHitPct, s.UncachedPerStep, s.OutputTokens, s.WallSeconds, s.MaxRSSMiB)
	if cost {
		fmt.Fprintf(w, " **%.4f** |", s.CostUSD)
	}
	fmt.Fprintf(w, "\n\nuncached input: %d tokens in total, %.0f per step\n", s.UncachedTokens, s.UncachedPerStep)

	fmt.Fprintln(w, "\n| task | pass rate | steps mean/median | tokens mean/median | uncached/step |")
	fmt.Fprintln(w, "|---|--:|--:|--:|--:|")
	for _, t := range PerTask(rs, p) {
		fmt.Fprintf(w, "| %s | pass %d/%d | %.1f / %.1f | %.0f / %.0f | %.0f |\n", t.Task, t.Passed, t.Runs, t.MeanSteps, t.MedianSteps, t.MeanTokens, t.MedianTokens, t.MeanUncached)
	}
}

// Report is the JSON written by WriteJSON. Results keeps every run (repeats included); Tasks is the
// per-task aggregate.
type Report struct {
	Model   string      `json:"model"`
	Prices  *Prices     `json:"prices,omitempty"`
	Summary Summary     `json:"summary"`
	Tasks   []TaskStats `json:"tasks,omitempty"`
	Results []Result    `json:"results"`
}

// WriteJSON writes the report to path.
func WriteJSON(path, model string, rs []Result) error {
	return WriteJSONWith(path, model, rs, Prices{})
}

// WriteJSONWith is WriteJSON recording the prices and the estimated cost.
func WriteJSONWith(path, model string, rs []Result, p Prices) error {
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].Task < rs[j].Task })
	rep := Report{Model: model, Summary: SummarizeWith(rs, p), Tasks: PerTask(rs, p), Results: rs}
	if p.Set() {
		rep.Prices = &p
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// Compare renders the change from a baseline report to a new one, per task and in total (pass rate,
// mean steps, mean input tokens, cache hit %, uncached tokens per step, seconds, and cost when either
// report has prices), for before/after runs of a harness change.
func Compare(w io.Writer, before, after Report) {
	bp, ap := Prices{}, Prices{}
	if before.Prices != nil {
		bp = *before.Prices
	}
	if after.Prices != nil {
		ap = *after.Prices
	}
	cost := bp.Set() || ap.Set()
	bs, as := byTask(before.Results, bp), byTask(after.Results, ap)
	head, sep := "| task | pass | steps | in tok | cache % | uncached/step | time s |", "|---|---|--:|--:|--:|--:|--:|"
	if cost {
		head, sep = head+" cost USD |", sep+"--:|"
	}
	fmt.Fprintln(w, head)
	fmt.Fprintln(w, sep)
	for _, a := range PerTask(after.Results, ap) {
		b := bs[a.Task]
		ar := as[a.Task]
		fmt.Fprintf(w, "| %s | %d/%d→%d/%d | %.0f→%.0f | %.0f→%.0f | %.0f→%.0f | %.0f→%.0f | %.0f→%.0f |", a.Task, b.stats.Passed, b.stats.Runs, a.Passed, a.Runs,
			b.stats.MeanSteps, a.MeanSteps, b.in, ar.in, b.cache, ar.cache, b.stats.MeanUncached, a.MeanUncached, b.secs, ar.secs)
		if cost {
			fmt.Fprintf(w, " %.4f→%.4f |", b.stats.MeanCostUSD, a.MeanCostUSD)
		}
		fmt.Fprintln(w)
	}
	b, a := before.Summary, after.Summary
	fmt.Fprintf(w, "| **total** | %d/%d→%d/%d | %d→%d | %d→%d | %.0f→%.0f | %.0f→%.0f | %.0f→%.0f |", b.Passed, b.Runs, a.Passed, a.Runs, b.Steps, a.Steps,
		b.InputTokens, a.InputTokens, b.CacheHitPct, a.CacheHitPct, b.UncachedPerStep, a.UncachedPerStep, b.WallSeconds, a.WallSeconds)
	if cost {
		fmt.Fprintf(w, " %.4f→%.4f |", b.CostUSD, a.CostUSD)
	}
	fmt.Fprintln(w)
}

// taskAgg is the per-task means Compare needs beyond TaskStats.
type taskAgg struct {
	stats           TaskStats
	in, cache, secs float64
}

func byTask(rs []Result, p Prices) map[string]taskAgg {
	out := map[string]taskAgg{}
	for _, st := range PerTask(rs, p) {
		var in, cache, secs float64
		n := 0
		for _, r := range rs {
			if r.Task == st.Task {
				in += float64(r.InputTokens)
				cache += r.CacheHitPct
				secs += r.WallSeconds
				n++
			}
		}
		out[st.Task] = taskAgg{stats: st, in: in / float64(n), cache: cache / float64(n), secs: secs / float64(n)}
	}
	return out
}

// ReadReport loads a report written by WriteJSON.
func ReadReport(path string) (Report, error) {
	var r Report
	b, err := os.ReadFile(path)
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(b, &r)
}

// Names returns the task names of the built-in set, comma separated (for flag help).
func Names() string {
	var ns []string
	for _, t := range Builtin() {
		ns = append(ns, t.Name)
	}
	return strings.Join(ns, ", ")
}
