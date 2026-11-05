package bench

import "testing"

func runs(task string, pass, n int) []Result {
	var out []Result
	for i := 0; i < n; i++ {
		out = append(out, Result{Task: task, Passed: i < pass, Steps: 10, InputTokens: 1000})
	}
	return out
}

func report(parts ...[]Result) Report {
	var rs []Result
	for _, p := range parts {
		rs = append(rs, p...)
	}
	return Report{Summary: Summarize(rs), Results: rs}
}

func TestGate(t *testing.T) {
	base := report(runs("a", 3, 5), runs("b", 2, 5))
	cases := []struct {
		name         string
		after        Report
		o            GateOptions
		accept, inco bool
	}{
		{"same", report(runs("a", 3, 5), runs("b", 2, 5)), GateOptions{}, true, false},
		{"better", report(runs("a", 5, 5), runs("b", 3, 5)), GateOptions{RequireGain: true}, true, false},
		{"same but gain required", report(runs("a", 3, 5), runs("b", 2, 5)), GateOptions{RequireGain: true}, false, false},
		{"one task regressed", report(runs("a", 5, 5), runs("b", 0, 5)), GateOptions{}, false, false},
		{"task removed", report(runs("a", 5, 5)), GateOptions{}, false, true},
		{"too few runs", report(runs("a", 3, 2), runs("b", 2, 2)), GateOptions{}, false, true},
	}
	for _, c := range cases {
		v := Gate(base, c.after, c.o)
		if v.Accept != c.accept || v.Inconclusive != c.inco {
			t.Errorf("%s: accept=%v inconclusive=%v reasons=%v, want accept=%v inconclusive=%v", c.name, v.Accept, v.Inconclusive, v.Reasons, c.accept, c.inco)
		}
	}
}

func TestGateSignificantOverallDrop(t *testing.T) {
	before := report(runs("a", 20, 20))
	after := report(runs("a", 12, 20))
	if v := Gate(before, after, GateOptions{MaxTaskDrop: 0.9}); v.Accept {
		t.Fatalf("a significant overall drop was accepted: %+v", v)
	}
}

func TestLowerP(t *testing.T) {
	if p := lowerP(10, 10, 10, 10); p != 1 {
		t.Errorf("equal perfect proportions: p=%v", p)
	}
	if p := lowerP(10, 10, 0, 10); p > 0.001 {
		t.Errorf("10/10 vs 0/10: p=%v", p)
	}
}
