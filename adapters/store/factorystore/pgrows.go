package factorystore

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/7ar-chat/harness/core/factory"
)

// ts maps the zero time to NULL: the contract uses zero for "unset".
func ts(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func unts(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// jsonOf encodes v for a jsonb column; nil slices and maps become empty
// containers so SQL can always treat them as arrays and objects.
func jsonOf(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode json column: %w", err)
	}
	if string(b) == "null" {
		switch v.(type) {
		case []string:
			return []byte("[]"), nil
		case map[string]string:
			return []byte("{}"), nil
		}
	}
	return b, nil
}

const taskCols = `id, project_id, owner, title, spec, role, source, state, blocked_reason, error, result,
	parent_id, depends_on, labels, priority, not_before, thread_id, run_id, model, repo, base_branch, branch,
	pr, auto_merge, attempts, max_attempts, policy, budget, spent_usd, spent_tokens, spent_runs, spent_unpriced,
	fingerprints, feedback, created_at, updated_at, finished_at`

func scanTask(r pgx.Row) (*factory.Task, error) {
	var t factory.Task
	var source, deps, labels, pr, budget, fps []byte
	var notBefore, finished *time.Time
	var state string
	if err := r.Scan(&t.ID, &t.ProjectID, &t.Owner, &t.Title, &t.Spec, &t.Role, &source, &state, &t.BlockedReason, &t.Error, &t.Result,
		&t.ParentID, &deps, &labels, &t.Priority, &notBefore, &t.ThreadID, &t.RunID, &t.Model, &t.Repo, &t.BaseBranch, &t.Branch,
		&pr, &t.AutoMerge, &t.Attempts, &t.MaxAttempts, &t.Policy, &budget, &t.Spent.USD, &t.Spent.Tokens, &t.Spent.Runs, &t.Spent.Unpriced,
		&fps, &t.Feedback, &t.CreatedAt, &t.UpdatedAt, &finished); err != nil {
		return nil, err
	}
	t.State = factory.State(state)
	t.NotBefore, t.FinishedAt = unts(notBefore), unts(finished)
	for _, f := range []struct {
		raw []byte
		dst any
	}{{source, &t.Source}, {deps, &t.DependsOn}, {labels, &t.Labels}, {pr, &t.PR}, {budget, &t.Budget}, {fps, &t.Fingerprints}} {
		if err := json.Unmarshal(f.raw, f.dst); err != nil {
			return nil, fmt.Errorf("decode task %s: %w", t.ID, err)
		}
	}
	if len(t.DependsOn) == 0 {
		t.DependsOn = nil
	}
	if len(t.Labels) == 0 {
		t.Labels = nil
	}
	if len(t.Fingerprints) == 0 {
		t.Fingerprints = nil
	}
	return &t, nil
}

// insertTaskSQL stores every column of taskCols plus source_key. Spend columns
// are written only here and by AddSpend.
const insertTaskSQL = `INSERT INTO work_factory_tasks (` + taskCols + `, source_key)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35,$36,$37,$38)
	ON CONFLICT DO NOTHING`

func taskInsertArgs(t *factory.Task) ([]any, error) {
	var enc [6][]byte
	for i, v := range []any{t.Source, t.DependsOn, t.Labels, t.PR, t.Budget, t.Fingerprints} {
		b, err := jsonOf(v)
		if err != nil {
			return nil, err
		}
		enc[i] = b
	}
	return []any{t.ID, t.ProjectID, t.Owner, t.Title, t.Spec, t.Role, enc[0], string(t.State), t.BlockedReason, t.Error, t.Result,
		t.ParentID, enc[1], enc[2], t.Priority, ts(t.NotBefore), t.ThreadID, t.RunID, t.Model, t.Repo, t.BaseBranch, t.Branch,
		enc[3], t.AutoMerge, t.Attempts, t.MaxAttempts, t.Policy, enc[4], t.Spent.USD, t.Spent.Tokens, t.Spent.Runs, t.Spent.Unpriced,
		enc[5], t.Feedback, t.CreatedAt, t.UpdatedAt, ts(t.FinishedAt), t.Source.Key}, nil
}

// updateTaskSQL writes what a Patch can change; the spend columns are left to AddSpend.
const updateTaskSQL = `UPDATE work_factory_tasks SET title=$2, spec=$3, state=$4, blocked_reason=$5, error=$6, result=$7,
	depends_on=$8, labels=$9, not_before=$10, thread_id=$11, run_id=$12, model=$13, branch=$14, pr=$15, auto_merge=$16,
	attempts=$17, max_attempts=$18, policy=$19, budget=$20, fingerprints=$21, feedback=$22, updated_at=$23, finished_at=$24
	WHERE id=$1`

func taskUpdateArgs(t *factory.Task) ([]any, error) {
	var enc [5][]byte
	for i, v := range []any{t.DependsOn, t.Labels, t.PR, t.Budget, t.Fingerprints} {
		b, err := jsonOf(v)
		if err != nil {
			return nil, err
		}
		enc[i] = b
	}
	return []any{t.ID, t.Title, t.Spec, string(t.State), t.BlockedReason, t.Error, t.Result,
		enc[0], enc[1], ts(t.NotBefore), t.ThreadID, t.RunID, t.Model, t.Branch, enc[2], t.AutoMerge,
		t.Attempts, t.MaxAttempts, t.Policy, enc[3], enc[4], t.Feedback, t.UpdatedAt, ts(t.FinishedAt)}, nil
}

const triggerCols = `id, project_id, owner, name, kind, schedule, event, match, template, enabled, next_fire_at, last_fired_at, created_at`

func scanTrigger(r pgx.Row) (*factory.Trigger, error) {
	var t factory.Trigger
	var match, tmpl []byte
	var next, last *time.Time
	if err := r.Scan(&t.ID, &t.ProjectID, &t.Owner, &t.Name, &t.Kind, &t.Schedule, &t.Event, &match, &tmpl, &t.Enabled, &next, &last, &t.CreatedAt); err != nil {
		return nil, err
	}
	t.NextFireAt, t.LastFiredAt = unts(next), unts(last)
	if err := json.Unmarshal(match, &t.Match); err != nil {
		return nil, fmt.Errorf("decode trigger %s match: %w", t.ID, err)
	}
	if len(t.Match) == 0 {
		t.Match = nil
	}
	if err := json.Unmarshal(tmpl, &t.Template); err != nil {
		return nil, fmt.Errorf("decode trigger %s template: %w", t.ID, err)
	}
	if len(t.Template.Labels) == 0 {
		t.Template.Labels = nil
	}
	return &t, nil
}
