package learning

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/7ar-chat/harness/core/factory"
)

// Cluster is a failure that keeps happening: tasks that ended the same way.
type Cluster struct {
	// Signature is the factory.Fingerprint of the failure.
	Signature string   `json:"signature"`
	Count     int      `json:"count"`
	Tasks     []string `json:"tasks"`
	Repos     []string `json:"repos,omitempty"`
	Titles    []string `json:"titles,omitempty"`
	// Sample is the error text of the most recent member.
	Sample string `json:"sample"`
	// USD is what the members spent: the cost of not fixing it.
	USD       float64   `json:"usd"`
	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
}

// signatureOf is the failure signature of a row, "" when the row is not a learnable failure. Failed tasks always
// are. A blocked task is only when it blocked on a failure (it has failure fingerprints), not when it waits for a
// human answer or for a merge.
func signatureOf(r Row) string {
	switch r.Result {
	case ResultFailure:
	case ResultBlocked:
		if len(r.Fingerprints) == 0 {
			return ""
		}
	default:
		return ""
	}
	if n := len(r.Fingerprints); n > 0 && r.Fingerprints[n-1] != "" {
		return r.Fingerprints[n-1]
	}
	return factory.Fingerprint(firstNonEmpty(r.Error, r.BlockedReason))
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// Mine groups the failed rows by signature and returns the clusters of at least minCount tasks, the costliest and
// most frequent first. Delegated sub-tasks are skipped: their parent carries the failure.
func Mine(rows []Row, minCount int) []Cluster {
	if minCount < 1 {
		minCount = 2
	}
	by := map[string]*Cluster{}
	for _, r := range rows {
		if r.Source == factory.SourceDelegate {
			continue
		}
		sig := signatureOf(r)
		if sig == "" {
			continue
		}
		c := by[sig]
		if c == nil {
			c = &Cluster{Signature: sig, FirstSeen: r.CreatedAt}
			by[sig] = c
		}
		c.Count++
		c.Tasks = append(c.Tasks, r.TaskID)
		c.USD += r.USD
		if r.Repo != "" && !contains(c.Repos, r.Repo) {
			c.Repos = append(c.Repos, r.Repo)
		}
		if len(c.Titles) < 5 {
			c.Titles = append(c.Titles, r.Title)
		}
		if r.CreatedAt.Before(c.FirstSeen) {
			c.FirstSeen = r.CreatedAt
		}
		if !r.CreatedAt.Before(c.LastSeen) {
			c.LastSeen = r.CreatedAt
			c.Sample = firstNonEmpty(r.Error, r.BlockedReason)
		}
	}
	var out []Cluster
	for _, c := range by {
		if c.Count >= minCount {
			out = append(out, *c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if out[i].USD != out[j].USD {
			return out[i].USD > out[j].USD
		}
		return out[i].Signature < out[j].Signature
	})
	return out
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// Candidate is a regression case mined from a real failure: the outline of a bench task. It is not runnable (a
// bench task needs a fixture and hidden tests), so it is written as JSON for the improver, or a human, to flesh
// out. Every recurring failure leaves one, so the suite grows with what actually goes wrong.
type Candidate struct {
	Name      string   `json:"name"`
	Signature string   `json:"signature"`
	Seen      int      `json:"seen"`
	Repos     []string `json:"repos,omitempty"`
	Titles    []string `json:"titles,omitempty"`
	Failure   string   `json:"failure"`
	SourceIDs []string `json:"sourceTasks"`
}

// CandidatesOf builds the regression candidates of clusters.
func CandidatesOf(cs []Cluster) []Candidate {
	out := make([]Candidate, 0, len(cs))
	for _, c := range cs {
		out = append(out, Candidate{
			Name: "regress-" + c.Signature, Signature: c.Signature, Seen: c.Count, Repos: c.Repos,
			Titles: c.Titles, Failure: clip(c.Sample, 1500), SourceIDs: c.Tasks,
		})
	}
	return out
}

// WriteCandidates writes each candidate as <dir>/<name>.json unless the file exists (an existing candidate may have
// been fleshed out), and returns the paths written.
func WriteCandidates(dir string, cs []Candidate) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var written []string
	for _, c := range cs {
		p := filepath.Join(dir, c.Name+".json")
		if _, err := os.Stat(p); err == nil {
			continue
		}
		b, err := json.MarshalIndent(c, "", "  ")
		if err != nil {
			return written, err
		}
		if err := os.WriteFile(p, append(b, '\n'), 0o644); err != nil {
			return written, fmt.Errorf("learning: writing %s: %w", p, err)
		}
		written = append(written, p)
	}
	return written, nil
}
