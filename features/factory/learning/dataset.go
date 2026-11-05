package learning

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/7ar-chat/harness/core/factory"
)

// Outcome results.
const (
	ResultSuccess = "success" // the task is done
	ResultFailure = "failure" // the task failed
	ResultBlocked = "blocked" // the task needs a human (provisional: a human may unblock it)
)

// Row is one finished task, flattened for analysis: what it was, what its runs were given, what it cost and how it
// ended. It is the dataset the scorer, the miner and the experiments read, and what Handler exports.
type Row struct {
	TaskID    string `json:"taskId"`
	ProjectID string `json:"projectId,omitempty"`
	Owner     string `json:"owner,omitempty"`
	// Key is the memory key the task's lessons belong to (project:<id>, else the owner).
	Key    string `json:"key"`
	Repo   string `json:"repo,omitempty"`
	Title  string `json:"title"`
	Source string `json:"source"`
	Role   string `json:"role,omitempty"`
	Model  string `json:"model,omitempty"`
	// PromptVersion is the prompt variant the runs used ("" when none was recorded).
	PromptVersion string `json:"promptVersion,omitempty"`

	Result  string `json:"result"`
	State   string `json:"state"`
	Retried bool   `json:"retried,omitempty"`

	Attempts      int      `json:"attempts"`
	CIFailures    int      `json:"ciFailures,omitempty"`
	ReviewRejects int      `json:"reviewRejects,omitempty"`
	Escalations   int      `json:"escalations,omitempty"`
	Fingerprints  []string `json:"fingerprints,omitempty"`
	Error         string   `json:"error,omitempty"`
	BlockedReason string   `json:"blockedReason,omitempty"`

	USD         float64 `json:"usd"`
	Tokens      int64   `json:"tokens"`
	Runs        int     `json:"runs"`
	DurationSec float64 `json:"durationSec"`

	Memory []string `json:"memory,omitempty"`
	// MemoryKeys maps each recalled entry id to its memory key.
	MemoryKeys map[string]string `json:"memoryKeys,omitempty"`
	Skills     []string          `json:"skills,omitempty"`
	Labels     []string          `json:"labels,omitempty"`

	CreatedAt  time.Time `json:"createdAt"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
}

// KeyOf is the memory key of a task, the same default as the lessons feature.
func KeyOf(t factory.Task) string {
	if t.ProjectID != "" {
		return "project:" + t.ProjectID
	}
	return t.Owner
}

// ResultOf maps a task state to a result; ok is false for states that carry no outcome (running, cancelled…).
func ResultOf(s factory.State) (string, bool) {
	switch s {
	case factory.StateDone:
		return ResultSuccess, true
	case factory.StateFailed:
		return ResultFailure, true
	case factory.StateBlocked:
		return ResultBlocked, true
	}
	return "", false
}

// RowOf flattens a finished or blocked task and its history. ok is false for a task with no outcome.
func RowOf(t factory.Task, ev []factory.TaskEvent) (Row, bool) {
	res, ok := ResultOf(t.State)
	if !ok {
		return Row{}, false
	}
	r := Row{
		TaskID: t.ID, ProjectID: t.ProjectID, Owner: t.Owner, Key: KeyOf(t), Repo: t.Repo, Title: t.Title,
		Source: t.Source.Kind, Role: t.Role, Model: t.Model, Result: res, State: string(t.State),
		Attempts: t.Attempts, Retried: t.Attempts > 1, Fingerprints: t.Fingerprints,
		Error: clip(t.Error, 600), BlockedReason: clip(t.BlockedReason, 300),
		USD: t.Spent.USD, Tokens: t.Spent.Tokens, Runs: t.Spent.Runs, Labels: t.Labels,
		CreatedAt: t.CreatedAt, FinishedAt: t.FinishedAt,
	}
	end := t.FinishedAt
	if end.IsZero() {
		end = t.UpdatedAt
	}
	if !end.IsZero() && !t.CreatedAt.IsZero() {
		r.DurationSec = end.Sub(t.CreatedAt).Seconds()
	}
	for _, e := range ev {
		switch e.Kind {
		case factory.EventCI:
			if s, _ := e.Data["state"].(string); s == "failed" {
				r.CIFailures++
			}
		case factory.EventReview:
			if v, _ := e.Data["verdict"].(string); v != "" && v != "approve" {
				r.ReviewRejects++
			}
		case factory.EventEscalated:
			r.Escalations++
		}
	}
	mem := map[string]string{}
	skills := map[string]bool{}
	for _, c := range contextsOf(ev) {
		for _, id := range c.Memory {
			mem[id] = c.Keys[id]
		}
		for _, s := range c.Skills {
			skills[s] = true
		}
		if c.PromptVersion != "" {
			r.PromptVersion = c.PromptVersion
		}
		if r.Model == "" {
			r.Model = c.Model
		}
	}
	// Skills are also recorded on the outcome event, which knows what the finished thread used.
	for _, e := range ev {
		if e.Kind != factory.EventOutcome {
			continue
		}
		var c Context
		if fromData(e.Data, &c) {
			for _, s := range c.Skills {
				skills[s] = true
			}
			if c.PromptVersion != "" {
				r.PromptVersion = c.PromptVersion
			}
		}
	}
	for id := range mem {
		r.Memory = append(r.Memory, id)
	}
	sort.Strings(r.Memory)
	if len(r.Memory) > 0 {
		r.MemoryKeys = mem
	}
	for s := range skills {
		r.Skills = append(r.Skills, s)
	}
	sort.Strings(r.Skills)
	return r, true
}

// CollectLimit bounds how many tasks Collect reads.
const CollectLimit = 10000

// Collect builds the dataset: one Row per task of f that finished, failed or is blocked (cancelled tasks and tasks
// still moving carry no outcome). Delegated sub-tasks are included with Source "delegate"; filter on it to
// exclude them.
func Collect(ctx context.Context, st factory.Store, f factory.Filter) ([]Row, error) {
	if f.Limit <= 0 {
		f.Limit = CollectLimit
	}
	f.States = []factory.State{factory.StateDone, factory.StateFailed, factory.StateBlocked}
	ts, err := st.ListTasks(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("learning: listing tasks: %w", err)
	}
	rows := make([]Row, 0, len(ts))
	for _, t := range ts {
		ev, err := st.ListEvents(ctx, t.ID, 0)
		if err != nil {
			return nil, fmt.Errorf("learning: events of task %s: %w", t.ID, err)
		}
		if r, ok := RowOf(t, ev); ok {
			rows = append(rows, r)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].CreatedAt.Before(rows[j].CreatedAt) })
	return rows, nil
}

// WriteJSONL writes one JSON object per row.
func WriteJSONL(w io.Writer, rows []Row) error {
	enc := json.NewEncoder(w)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}

// Rate is the success rate of the rows that reached a verdict (success or failure; blocked rows are provisional
// and left out), and how many that was.
func Rate(rows []Row, keep func(Row) bool) (rate float64, n int) {
	wins := 0
	for _, r := range rows {
		if r.Result == ResultBlocked || keep != nil && !keep(r) {
			continue
		}
		n++
		if r.Result == ResultSuccess {
			wins++
		}
	}
	if n == 0 {
		return 0, 0
	}
	return float64(wins) / float64(n), n
}

// Handler serves the dataset as JSON lines (GET ?project=&owner=&limit=). It does no authentication: mount it
// behind the host's own.
func Handler(st factory.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		q := r.URL.Query()
		f := factory.Filter{ProjectID: q.Get("project"), Owner: q.Get("owner")}
		if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
			f.Limit = n
		}
		rows, err := Collect(r.Context(), st, f)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_ = WriteJSONL(w, rows)
	})
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
