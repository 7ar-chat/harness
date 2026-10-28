package bench

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// Summary aggregates the results of a benchmark.
type Summary struct {
	Runs         int     `json:"runs"`
	Passed       int     `json:"passed"`
	PassRate     float64 `json:"passRate"`
	Steps        int     `json:"steps"`
	ToolCalls    int     `json:"toolCalls"`
	ToolErrors   int     `json:"toolErrors"`
	InputTokens  int     `json:"inputTokens"`
	OutputTokens int     `json:"outputTokens"`
	CachedTokens int     `json:"cachedTokens"`
	CacheHitPct  float64 `json:"cacheHitPct"`
	WallSeconds  float64 `json:"wallSeconds"`
	MaxRSSMiB    float64 `json:"maxRSSMiB"`
}

// Summarize totals results (skipped runs are not counted by the caller).
func Summarize(rs []Result) Summary {
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
		s.WallSeconds += r.WallSeconds
		s.MaxRSSMiB = max(s.MaxRSSMiB, r.MaxRSSMiB)
	}
	if s.Runs > 0 {
		s.PassRate = float64(s.Passed) / float64(s.Runs)
	}
	if s.InputTokens > 0 {
		s.CacheHitPct = 100 * float64(s.CachedTokens) / float64(s.InputTokens)
	}
	return s
}

// Markdown renders results as a table with a total row.
func Markdown(w io.Writer, rs []Result) {
	fmt.Fprintln(w, "| task | pass | status | steps | tools (err) | in tok | cached | out tok | time s | heap MiB |")
	fmt.Fprintln(w, "|---|---|---|--:|--:|--:|--:|--:|--:|--:|")
	for _, r := range rs {
		mark := "FAIL"
		if r.Passed {
			mark = "pass"
		}
		fmt.Fprintf(w, "| %s | %s | %s | %d | %d (%d) | %d | %.0f%% | %d | %.1f | %.0f |\n",
			r.Task, mark, r.Status, r.Steps, r.ToolCalls, r.ToolErrors, r.InputTokens, r.CacheHitPct, r.OutputTokens, r.WallSeconds, r.HeapMiB)
	}
	s := Summarize(rs)
	fmt.Fprintf(w, "| **total** | **%d/%d** | | %d | %d (%d) | %d | %.0f%% | %d | %.1f | peak RSS %.0f |\n",
		s.Passed, s.Runs, s.Steps, s.ToolCalls, s.ToolErrors, s.InputTokens, s.CacheHitPct, s.OutputTokens, s.WallSeconds, s.MaxRSSMiB)
}

// Report is the JSON written by WriteJSON.
type Report struct {
	Model   string   `json:"model"`
	Summary Summary  `json:"summary"`
	Results []Result `json:"results"`
}

// WriteJSON writes the report to path.
func WriteJSON(path, model string, rs []Result) error {
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].Task < rs[j].Task })
	b, err := json.MarshalIndent(Report{Model: model, Summary: Summarize(rs), Results: rs}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// Compare renders the change from a baseline report to a new one, per task and in total
// (pass, steps, input tokens, cache hit %, seconds), for before/after runs of a harness change.
func Compare(w io.Writer, before, after Report) {
	by := map[string]Result{}
	for _, r := range before.Results {
		by[r.Task] = r
	}
	fmt.Fprintln(w, "| task | pass | steps | in tok | cache % | time s |")
	fmt.Fprintln(w, "|---|---|--:|--:|--:|--:|")
	for _, a := range after.Results {
		b := by[a.Task]
		fmt.Fprintf(w, "| %s | %s→%s | %d→%d | %d→%d | %.0f→%.0f | %.0f→%.0f |\n", a.Task, passWord(b.Passed), passWord(a.Passed),
			b.Steps, a.Steps, b.InputTokens, a.InputTokens, b.CacheHitPct, a.CacheHitPct, b.WallSeconds, a.WallSeconds)
	}
	fmt.Fprintf(w, "| **total** | %d→%d | %d→%d | %d→%d | %.0f→%.0f | %.0f→%.0f |\n", before.Summary.Passed, after.Summary.Passed,
		before.Summary.Steps, after.Summary.Steps, before.Summary.InputTokens, after.Summary.InputTokens,
		before.Summary.CacheHitPct, after.Summary.CacheHitPct, before.Summary.WallSeconds, after.Summary.WallSeconds)
}

func passWord(p bool) string {
	if p {
		return "pass"
	}
	return "FAIL"
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
