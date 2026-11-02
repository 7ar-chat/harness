package factorystore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/7ar-chat/harness/core/factory"
)

const (
	// Delivery ids are kept for the last pgDeliveryKeep deliveries and pgDeliveryAge, whichever
	// bound is hit first; redeliveries come minutes to days later, never months.
	pgDeliveryKeep = 10000
	pgDeliveryAge  = 30 * 24 * time.Hour
	// pgPruneEvery runs the delivery prune on every n-th insert instead of on each one.
	pgPruneEvery = 256
)

// PG is a factory.Store on Postgres (tables of migration 0490, applied by
// pgstore). Several processes may share it: every read-modify-write runs in one
// transaction under a row lock.
type PG struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewPG returns a store on pool, whose search_path names the schema the
// migrations ran in. now (nil: time.Now) is the clock.
func NewPG(pool *pgxpool.Pool, now func() time.Time) *PG {
	if now == nil {
		now = time.Now
	}
	return &PG{pool: pool, now: now}
}

var _ factory.Store = (*PG)(nil)

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return factory.ErrNotFound
	}
	return err
}

func pgCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

// tx runs fn in a transaction and commits when it returns nil.
func (s *PG) tx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// lockDeps share-locks the dependencies of a task and returns the ones that are not done. A
// dependency that finishes concurrently waits for the lock, so its release of Pending tasks
// runs after this transaction committed and cannot miss the task being written.
func lockDeps(ctx context.Context, tx pgx.Tx, self string, deps []string) (unmet bool, err error) {
	if len(deps) == 0 {
		return false, nil
	}
	uniq := slices.Clone(deps)
	slices.Sort(uniq)
	uniq = slices.Compact(uniq)
	if slices.Contains(uniq, self) {
		return false, fmt.Errorf("%w: task depends on itself", factory.ErrInvalid)
	}
	rows, err := tx.Query(ctx, `SELECT id, state FROM work_factory_tasks WHERE id = ANY($1) ORDER BY id FOR SHARE`, uniq)
	if err != nil {
		return false, fmt.Errorf("lock dependencies: %w", err)
	}
	defer rows.Close()
	found := map[string]bool{}
	for rows.Next() {
		var id, state string
		if err := rows.Scan(&id, &state); err != nil {
			return false, err
		}
		found[id] = true
		if factory.State(state) != factory.StateDone {
			unmet = true
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, d := range uniq {
		if !found[d] {
			return false, fmt.Errorf("%w: dependency %s: %w", factory.ErrInvalid, d, factory.ErrNotFound)
		}
	}
	return unmet, nil
}

// CreateTask implements factory.Store.
func (s *PG) CreateTask(ctx context.Context, t factory.Task) (*factory.Task, bool, error) {
	if t.ID == "" {
		t.ID = uuid.NewString()
	}
	var out *factory.Task
	created := false
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if t.Source.Key != "" {
			// Fast path for redelivery; the unique index below settles the race.
			ex, err := scanTask(tx.QueryRow(ctx, `SELECT `+taskCols+` FROM work_factory_tasks WHERE project_id = $1 AND source_key = $2`, t.ProjectID, t.Source.Key))
			if err == nil {
				out = ex
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		unmet, err := lockDeps(ctx, tx, t.ID, t.DependsOn)
		if err != nil {
			return err
		}
		now := s.now()
		if t.CreatedAt.IsZero() {
			t.CreatedAt = now
		}
		t.UpdatedAt = now
		t.State = factory.StateReady
		if unmet {
			t.State = factory.StatePending
		}
		args, err := taskInsertArgs(&t)
		if err != nil {
			return err
		}
		got, err := scanTask(tx.QueryRow(ctx, insertTaskSQL+` RETURNING `+taskCols, args...))
		switch {
		case err == nil:
			out, created = got, true
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("insert task: %w", err)
		}
		// Nothing inserted: the key or the id is taken. The key's owner committed (the unique
		// index waited for it), so it is visible to this statement.
		if t.Source.Key != "" {
			ex, err := scanTask(tx.QueryRow(ctx, `SELECT `+taskCols+` FROM work_factory_tasks WHERE project_id = $1 AND source_key = $2`, t.ProjectID, t.Source.Key))
			if err == nil {
				out = ex
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		return fmt.Errorf("%w: task %s exists", factory.ErrInvalid, t.ID)
	})
	if err != nil {
		return nil, false, err
	}
	return out, created, nil
}

// GetTask implements factory.Store.
func (s *PG) GetTask(ctx context.Context, id string) (*factory.Task, error) {
	t, err := scanTask(s.pool.QueryRow(ctx, `SELECT `+taskCols+` FROM work_factory_tasks WHERE id = $1`, id))
	return t, notFound(err)
}

// releaseSQL moves Pending tasks whose dependencies are all done to Ready. The candidates are
// locked in id order so that two releasers never wait for each other.
const releaseSQL = `UPDATE work_factory_tasks SET state = 'ready', updated_at = $1
	WHERE id IN (
		SELECT p.id FROM work_factory_tasks p
		WHERE p.state = 'pending' AND NOT EXISTS (
			SELECT 1 FROM jsonb_array_elements_text(p.depends_on) d(id)
			WHERE NOT EXISTS (SELECT 1 FROM work_factory_tasks x WHERE x.id = d.id AND x.state = 'done'))
		ORDER BY p.id FOR UPDATE)`

// UpdateTask implements factory.Store.
func (s *PG) UpdateTask(ctx context.Context, id string, p factory.Patch) (*factory.Task, error) {
	var out *factory.Task
	err := s.tx(ctx, func(tx pgx.Tx) error {
		// Updates are serialized (before any row lock, so no lock order cycle): two tasks that
		// finish together must each see the other's commit when they release dependents, or a
		// task waiting on both would stay Pending. UpdateTask is a control-plane write, not a hot path.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('work_factory_update', 0))`); err != nil {
			return fmt.Errorf("serialize update: %w", err)
		}
		t, err := scanTask(tx.QueryRow(ctx, `SELECT `+taskCols+` FROM work_factory_tasks WHERE id = $1 FOR UPDATE`, id))
		if err != nil {
			return notFound(err)
		}
		if len(p.IfState) > 0 && !slices.Contains(p.IfState, t.State) {
			return factory.ErrConflict
		}
		if err := checkPatch(p); err != nil {
			return err
		}
		if p.DependsOn != nil {
			if _, err := lockDeps(ctx, tx, id, *p.DependsOn); err != nil {
				return err
			}
		}
		now := s.now()
		applyPatch(t, p, now)
		args, err := taskUpdateArgs(t)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, updateTaskSQL, args...); err != nil {
			return fmt.Errorf("update task: %w", err)
		}
		if _, err := tx.Exec(ctx, releaseSQL, now); err != nil {
			return fmt.Errorf("release dependents: %w", err)
		}
		out, err = scanTask(tx.QueryRow(ctx, `SELECT `+taskCols+` FROM work_factory_tasks WHERE id = $1`, id))
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListTasks implements factory.Store: highest priority first, then oldest first.
func (s *PG) ListTasks(ctx context.Context, f factory.Filter) ([]factory.Task, error) {
	var where []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, strings.ReplaceAll(cond, "?", "$"+strconv.Itoa(len(args))))
	}
	if f.ProjectID != "" {
		add("project_id = ?", f.ProjectID)
	}
	if f.Owner != "" {
		add("owner = ?", f.Owner)
	}
	if f.ParentID != "" {
		add("parent_id = ?", f.ParentID)
	}
	if f.ThreadID != "" {
		add("thread_id = ?", f.ThreadID)
	}
	if f.SourceKey != "" {
		add("source_key = ?", f.SourceKey)
	}
	if len(f.States) > 0 {
		states := make([]string, len(f.States))
		for i, st := range f.States {
			states[i] = string(st)
		}
		add("state = ANY(?)", states)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	args = append(args, limit)
	q := `SELECT ` + taskCols + ` FROM work_factory_tasks`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	q += ` ORDER BY priority DESC, created_at, id COLLATE "C" LIMIT $` + strconv.Itoa(len(args))
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()
	var out []factory.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// AddSpend implements factory.Store: the task's counters are incremented in SQL, so concurrent
// callers never lose an update, and the same amounts go to the ledger SpendSince reads.
func (s *PG) AddSpend(ctx context.Context, id string, d factory.Spend) (*factory.Task, error) {
	var out *factory.Task
	err := s.tx(ctx, func(tx pgx.Tx) error {
		now := s.now()
		t, err := scanTask(tx.QueryRow(ctx, `UPDATE work_factory_tasks SET spent_usd = spent_usd + $2, spent_tokens = spent_tokens + $3,
			spent_runs = spent_runs + $4, spent_unpriced = spent_unpriced + $5, updated_at = $6 WHERE id = $1 RETURNING `+taskCols,
			id, d.USD, d.Tokens, d.Runs, d.Unpriced, now))
		if err != nil {
			return notFound(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO work_factory_spend (project_id, at, usd, tokens, runs, unpriced) VALUES ($1, $2, $3, $4, $5, $6)`,
			t.ProjectID, now, d.USD, d.Tokens, d.Runs, d.Unpriced); err != nil {
			return fmt.Errorf("record spend: %w", err)
		}
		out = t
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SpendSince implements factory.Store.
func (s *PG) SpendSince(ctx context.Context, projectID string, since time.Time) (factory.Spend, error) {
	var sp factory.Spend
	err := s.pool.QueryRow(ctx, `SELECT coalesce(sum(usd), 0), coalesce(sum(tokens), 0)::bigint, coalesce(sum(runs), 0)::int, coalesce(sum(unpriced), 0)::int
		FROM work_factory_spend WHERE ($1 = '' OR project_id = $1) AND at >= $2`, projectID, since).Scan(&sp.USD, &sp.Tokens, &sp.Runs, &sp.Unpriced)
	if err != nil {
		return factory.Spend{}, fmt.Errorf("sum spend: %w", err)
	}
	return sp, nil
}

// AppendEvent implements factory.Store.
func (s *PG) AppendEvent(ctx context.Context, e factory.TaskEvent) error {
	var data []byte
	if e.Data != nil {
		b, err := json.Marshal(e.Data)
		if err != nil {
			return fmt.Errorf("%w: event data: %v", factory.ErrInvalid, err)
		}
		data = b
	}
	if e.At.IsZero() {
		e.At = s.now()
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO work_factory_events (task_id, kind, text, data, at) VALUES ($1, $2, $3, $4, $5)`, e.TaskID, e.Kind, e.Text, data, e.At)
	if pgCode(err, "23503") { // foreign_key_violation: no such task
		return factory.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("append event: %w", err)
	}
	return nil
}

// ListEvents implements factory.Store: oldest first; with a limit, the newest limit events.
func (s *PG) ListEvents(ctx context.Context, taskID string, limit int) ([]factory.TaskEvent, error) {
	q := `SELECT id, task_id, kind, text, data, at FROM work_factory_events WHERE task_id = $1 ORDER BY id`
	args := []any{taskID}
	if limit > 0 {
		q = `SELECT * FROM (SELECT id, task_id, kind, text, data, at FROM work_factory_events WHERE task_id = $1 ORDER BY id DESC LIMIT $2) newest ORDER BY id`
		args = append(args, limit)
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()
	var out []factory.TaskEvent
	for rows.Next() {
		var e factory.TaskEvent
		var data []byte
		if err := rows.Scan(&e.ID, &e.TaskID, &e.Kind, &e.Text, &data, &e.At); err != nil {
			return nil, err
		}
		if data != nil {
			if err := json.Unmarshal(data, &e.Data); err != nil {
				return nil, fmt.Errorf("decode event %d: %w", e.ID, err)
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PutTrigger implements factory.Store (insert or replace by ID).
func (s *PG) PutTrigger(ctx context.Context, t factory.Trigger) (*factory.Trigger, error) {
	if t.ID == "" {
		t.ID = uuid.NewString()
	}
	if t.Kind != factory.TriggerCron && t.Kind != factory.TriggerWebhook {
		return nil, fmt.Errorf("%w: trigger kind %q", factory.ErrInvalid, t.Kind)
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = s.now()
	}
	match, err := jsonOf(t.Match)
	if err != nil {
		return nil, err
	}
	tmpl, err := jsonOf(t.Template)
	if err != nil {
		return nil, err
	}
	out, err := scanTrigger(s.pool.QueryRow(ctx, `INSERT INTO work_factory_triggers (`+triggerCols+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (id) DO UPDATE SET project_id = EXCLUDED.project_id, owner = EXCLUDED.owner, name = EXCLUDED.name, kind = EXCLUDED.kind,
			schedule = EXCLUDED.schedule, event = EXCLUDED.event, match = EXCLUDED.match, template = EXCLUDED.template, enabled = EXCLUDED.enabled,
			next_fire_at = EXCLUDED.next_fire_at, last_fired_at = EXCLUDED.last_fired_at, created_at = EXCLUDED.created_at
		RETURNING `+triggerCols,
		t.ID, t.ProjectID, t.Owner, t.Name, t.Kind, t.Schedule, t.Event, match, tmpl, t.Enabled, ts(t.NextFireAt), ts(t.LastFiredAt), t.CreatedAt))
	if err != nil {
		return nil, fmt.Errorf("put trigger: %w", err)
	}
	return out, nil
}

// GetTrigger implements factory.Store.
func (s *PG) GetTrigger(ctx context.Context, id string) (*factory.Trigger, error) {
	t, err := scanTrigger(s.pool.QueryRow(ctx, `SELECT `+triggerCols+` FROM work_factory_triggers WHERE id = $1`, id))
	return t, notFound(err)
}

// ListTriggers implements factory.Store ("" project: all), by creation time.
func (s *PG) ListTriggers(ctx context.Context, projectID string) ([]factory.Trigger, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+triggerCols+` FROM work_factory_triggers WHERE ($1 = '' OR project_id = $1) ORDER BY created_at, id COLLATE "C"`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list triggers: %w", err)
	}
	defer rows.Close()
	var out []factory.Trigger
	for rows.Next() {
		t, err := scanTrigger(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// DeleteTrigger implements factory.Store.
func (s *PG) DeleteTrigger(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM work_factory_triggers WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete trigger: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return factory.ErrNotFound
	}
	return nil
}

// ClaimDueTriggers implements factory.Store. The due rows are locked with SKIP LOCKED, so a
// second process claiming at the same moment skips them, and one that arrives after the
// commit no longer finds them due.
func (s *PG) ClaimDueTriggers(ctx context.Context, now time.Time, next func(factory.Trigger) time.Time) ([]factory.Trigger, error) {
	var due []factory.Trigger
	err := s.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+triggerCols+` FROM work_factory_triggers
			WHERE kind = 'cron' AND enabled AND next_fire_at IS NOT NULL AND next_fire_at <= $1
			ORDER BY id COLLATE "C" FOR UPDATE SKIP LOCKED`, now)
		if err != nil {
			return fmt.Errorf("select due triggers: %w", err)
		}
		var locked []factory.Trigger
		for rows.Next() {
			t, err := scanTrigger(rows)
			if err != nil {
				rows.Close()
				return err
			}
			locked = append(locked, *t)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, claimed := range locked {
			advanced := claimed
			advanced.LastFiredAt = claimed.NextFireAt
			advanced.NextFireAt = next(advanced)
			if _, err := tx.Exec(ctx, `UPDATE work_factory_triggers SET last_fired_at = $2, next_fire_at = $3 WHERE id = $1`,
				claimed.ID, ts(advanced.LastFiredAt), ts(advanced.NextFireAt)); err != nil {
				return fmt.Errorf("advance trigger %s: %w", claimed.ID, err)
			}
		}
		due = locked
		return nil
	})
	if err != nil {
		return nil, err
	}
	return due, nil
}

// SeenDelivery implements factory.Store.
func (s *PG) SeenDelivery(ctx context.Context, id string) (bool, error) {
	if id == "" {
		return false, nil
	}
	now := s.now()
	var seq int64
	err := s.pool.QueryRow(ctx, `INSERT INTO work_factory_deliveries (id, seen_at) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING RETURNING seq`, id, now).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("record delivery: %w", err)
	}
	if seq%pgPruneEvery == 0 {
		// The delivery is recorded; a failed prune only delays retention, so it does not fail the caller.
		if _, err := s.pool.Exec(ctx, `DELETE FROM work_factory_deliveries WHERE seen_at < $1
			OR seq <= (SELECT seq FROM work_factory_deliveries ORDER BY seq DESC OFFSET $2 LIMIT 1)`, now.Add(-pgDeliveryAge), pgDeliveryKeep); err != nil {
			slog.WarnContext(ctx, "factory: prune webhook deliveries", "err", err)
		}
	}
	return false, nil
}

// SetThreadInfo implements factory.Store.
func (s *PG) SetThreadInfo(ctx context.Context, ti factory.ThreadInfo) error {
	if ti.ThreadID == "" {
		return fmt.Errorf("%w: thread id", factory.ErrInvalid)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO work_factory_threads (thread_id, login, task_id, role, instructions, policy, parent_thread_id, read_only, repo, base_branch, branch, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (thread_id) DO UPDATE SET login = EXCLUDED.login, task_id = EXCLUDED.task_id, role = EXCLUDED.role, instructions = EXCLUDED.instructions,
			policy = EXCLUDED.policy, parent_thread_id = EXCLUDED.parent_thread_id, read_only = EXCLUDED.read_only, repo = EXCLUDED.repo,
			base_branch = EXCLUDED.base_branch, branch = EXCLUDED.branch, updated_at = EXCLUDED.updated_at`,
		ti.ThreadID, ti.Login, ti.TaskID, ti.Role, ti.Instructions, ti.Policy, ti.ParentThreadID, ti.ReadOnly, ti.Repo, ti.BaseBranch, ti.Branch, s.now()); err != nil {
		return fmt.Errorf("set thread info: %w", err)
	}
	return nil
}

// GetThreadInfo implements factory.Store.
func (s *PG) GetThreadInfo(ctx context.Context, threadID string) (*factory.ThreadInfo, error) {
	var ti factory.ThreadInfo
	err := s.pool.QueryRow(ctx, `SELECT thread_id, login, task_id, role, instructions, policy, parent_thread_id, read_only, repo, base_branch, branch
		FROM work_factory_threads WHERE thread_id = $1`, threadID).Scan(&ti.ThreadID, &ti.Login, &ti.TaskID, &ti.Role, &ti.Instructions, &ti.Policy,
		&ti.ParentThreadID, &ti.ReadOnly, &ti.Repo, &ti.BaseBranch, &ti.Branch)
	if err != nil {
		return nil, notFound(err)
	}
	return &ti, nil
}
