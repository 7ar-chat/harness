package learning

import (
	"fmt"
	"hash/fnv"
	"math"
	"sort"
)

// Control is the name of the variant that adds nothing to the prompt: the baseline of an experiment.
const Control = "control"

// Experiment assigns each task one prompt variant, deterministically, so that the outcomes recorded for the variants
// can be compared. A variant is extra instructions appended to the role's; the control adds none.
type Experiment struct {
	// Variants maps a variant name to its extra instructions. Control is added with no text if missing.
	Variants map[string]string
}

// Names lists the variants, control first, the rest sorted.
func (e Experiment) Names() []string {
	names := []string{Control}
	var rest []string
	for n := range e.Variants {
		if n != Control {
			rest = append(rest, n)
		}
	}
	sort.Strings(rest)
	return append(names, rest...)
}

// Pick returns the variant of a task: a stable hash of its id, so retries and resumes keep their variant.
func (e Experiment) Pick(taskID string) string {
	names := e.Names()
	if len(names) == 1 {
		return Control
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(taskID))
	return names[int(h.Sum32())%len(names)]
}

// Instructions are the extra instructions of a variant ("" for the control or an unknown name).
func (e Experiment) Instructions(variant string) string {
	if variant == Control {
		return ""
	}
	return e.Variants[variant]
}

// Standing is how one variant did.
type Standing struct {
	Variant string  `json:"variant"`
	N       int     `json:"n"`
	Wins    int     `json:"wins"`
	Rate    float64 `json:"rate"`
	Lo      float64 `json:"lo"`
	Hi      float64 `json:"hi"`
	MeanUSD float64 `json:"meanUsd"`
}

// Decision is the verdict of an experiment.
type Decision struct {
	Standings []Standing `json:"standings"`
	// Winner is the variant to promote ("" when none beats the control with confidence).
	Winner string  `json:"winner,omitempty"`
	P      float64 `json:"p,omitempty"`
	Why    string  `json:"why"`
}

// Decide compares every variant with the control over the rows (blocked tasks are left out). A variant wins only
// when it has at least minN tasks, a higher success rate than the control, a one-sided p-value under alpha, and
// does not cost more than 1.5 times the control per task. Without a winner the control stays.
func Decide(rows []Row, minN int, alpha float64) Decision {
	if minN <= 0 {
		minN = 20
	}
	if alpha <= 0 {
		alpha = 0.05
	}
	by := map[string]*Standing{}
	usd := map[string]float64{}
	for _, r := range rows {
		if r.Result == ResultBlocked || r.Source == "delegate" {
			continue
		}
		v := r.PromptVersion
		if v == "" {
			v = Control
		}
		s := by[v]
		if s == nil {
			s = &Standing{Variant: v}
			by[v] = s
		}
		s.N++
		if r.Result == ResultSuccess {
			s.Wins++
		}
		usd[v] += r.USD
	}
	var d Decision
	for _, s := range by {
		s.Rate = float64(s.Wins) / float64(s.N)
		s.Lo, s.Hi = Wilson(s.Wins, s.N, 1.64)
		s.MeanUSD = usd[s.Variant] / float64(s.N)
		d.Standings = append(d.Standings, *s)
	}
	sort.Slice(d.Standings, func(i, j int) bool { return d.Standings[i].Variant < d.Standings[j].Variant })
	ctl, ok := by[Control]
	if !ok || ctl.N < minN {
		d.Why = fmt.Sprintf("the control has %d tasks, need %d", n(ctl), minN)
		return d
	}
	best := ""
	bestP := 1.0
	for _, s := range d.Standings {
		if s.Variant == Control || s.N < minN || s.Rate <= ctl.Rate {
			continue
		}
		if ctl.MeanUSD > 0 && s.MeanUSD > 1.5*ctl.MeanUSD {
			continue
		}
		// one-sided p that the variant is NOT better: the control rate is "lower" in the swapped test
		if p := lowerP(s.Wins, s.N, ctl.Wins, ctl.N); p < alpha && p < bestP {
			best, bestP = s.Variant, p
		}
	}
	if best == "" {
		d.Why = "no variant beats the control with confidence"
		return d
	}
	d.Winner, d.P = best, bestP
	d.Why = fmt.Sprintf("%s beats the control (p=%.3f)", best, bestP)
	return d
}

func n(s *Standing) int {
	if s == nil {
		return 0
	}
	return s.N
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
	return 0.5 * math.Erfc(-((b-a)/se)/math.Sqrt2)
}
