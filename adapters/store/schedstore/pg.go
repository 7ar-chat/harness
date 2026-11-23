package schedstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/7ar-chat/harness/core/schedule"
)

// PG is a schedule.Store on Postgres (tables of migration 0580, applied by pgstore). Several processes may share it: a
// schedule that is due is claimed under a row lock (FOR UPDATE SKIP LOCKED) and a scheduled tick has a unique run, so
// two replicas never fire the same tick.
type PG struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

var _ schedule.Store = (*PG)(nil)

// NewPG returns a store on pool, whose search_path names the schema the migrations ran in. now (nil: time.Now) is the clock.
func NewPG(pool *pgxpool.Pool, now func() time.Time) *PG {
	if now == nil {
		now = time.Now
	}
	return &PG{pool: pool, now: now}
}

func (s *PG) tx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("schedstore: begin: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func ts(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

const schedCols = `id, project_id, owner, run_as, name, prompt, cron, timezone, model, policy, timeout_minutes, reuse_session, thread_id, enabled, next_run_at, last_run_at, last_status, created_at, updated_at`

func scanSchedule(row pgx.Row) (*schedule.Schedule, error) {
	var s schedule.Schedule
	var next, last *time.Time
	if err := row.Scan(&s.ID, &s.ProjectID, &s.Owner, &s.RunAs, &s.Name, &s.Prompt, &s.Cron, &s.Timezone, &s.Model, &s.Policy,
		&s.TimeoutMinutes, &s.ReuseSession, &s.ThreadID, &s.Enabled, &next, &last, &s.LastStatus, &s.CreatedAt, &s.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, schedule.ErrNotFound
		}
		return nil, err
	}
	if next != nil {
		s.NextRunAt = next.UTC()
	}
	if last != nil {
		s.LastRunAt = last.UTC()
	}
	s.CreatedAt, s.UpdatedAt = s.CreatedAt.UTC(), s.UpdatedAt.UTC()
	return &s, nil
}

const runCols = `id, schedule_id, fire_at, trigger, status, thread_id, agent_run_id, error, started_by, started_at, finished_at, deadline,
	input_tokens, output_tokens, cache_read, cache_write, cost_usd, unpriced`

func scanRun(row pgx.Row) (*schedule.Run, error) {
	var r schedule.Run
	var fin *time.Time
	if err := row.Scan(&r.ID, &r.ScheduleID, &r.FireAt, &r.Trigger, &r.Status, &r.ThreadID, &r.AgentRun, &r.Error, &r.StartedBy,
		&r.StartedAt, &fin, &r.Deadline, &r.Usage.InputTokens, &r.Usage.OutputTokens, &r.Usage.CacheReadTokens,
		&r.Usage.CacheWriteTokens, &r.Usage.CostUSD, &r.Usage.Unpriced); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, schedule.ErrNotFound
		}
		return nil, err
	}
	r.FireAt, r.StartedAt, r.Deadline = r.FireAt.UTC(), r.StartedAt.UTC(), r.Deadline.UTC()
	if fin != nil {
		r.FinishedAt = fin.UTC()
	}
	return &r, nil
}

// Create implements schedule.Store.
func (s *PG) Create(ctx context.Context, in schedule.Schedule) (*schedule.Schedule, error) {
	if err := in.Normalize(); err != nil {
		return nil, err
	}
	if in.ID == "" {
		in.ID = uuid.NewString()
	}
	now := s.now().UTC()
	row := s.pool.QueryRow(ctx, `INSERT INTO work_schedules (id, project_id, owner, run_as, name, prompt, cron, timezone, model, policy,
		timeout_minutes, reuse_session, thread_id, enabled, next_run_at, last_run_at, last_status, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'',$13,$14,NULL,'',$15,$15) RETURNING `+schedCols,
		in.ID, in.ProjectID, in.Owner, in.RunAs, in.Name, in.Prompt, in.Cron, in.Timezone, in.Model, in.Policy,
		in.TimeoutMinutes, in.ReuseSession, in.Enabled, ts(in.NextRunAt), now)
	out, err := scanSchedule(row)
	if err != nil {
		return nil, fmt.Errorf("schedstore: create: %w", err)
	}
	return out, nil
}

// Get implements schedule.Store.
func (s *PG) Get(ctx context.Context, id string) (*schedule.Schedule, error) {
	return scanSchedule(s.pool.QueryRow(ctx, `SELECT `+schedCols+` FROM work_schedules WHERE id = $1`, id))
}

// Update implements schedule.Store.
func (s *PG) Update(ctx context.Context, in schedule.Schedule) (*schedule.Schedule, error) {
	cur, err := s.Get(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	in.Owner, in.ProjectID = cur.Owner, cur.ProjectID
	if err := in.Normalize(); err != nil {
		return nil, err
	}
	return scanSchedule(s.pool.QueryRow(ctx, `UPDATE work_schedules SET run_as=$2, name=$3, prompt=$4, cron=$5, timezone=$6, model=$7, policy=$8,
		timeout_minutes=$9, reuse_session=$10, thread_id = CASE WHEN $10 THEN thread_id ELSE '' END, enabled=$11, next_run_at=$12, updated_at=$13
		WHERE id=$1 RETURNING `+schedCols,
		in.ID, in.RunAs, in.Name, in.Prompt, in.Cron, in.Timezone, in.Model, in.Policy, in.TimeoutMinutes, in.ReuseSession,
		in.Enabled, ts(in.NextRunAt), s.now().UTC()))
}

// Delete implements schedule.Store.
func (s *PG) Delete(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM work_schedules WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return schedule.ErrNotFound
	}
	return nil
}

// List implements schedule.Store.
func (s *PG) List(ctx context.Context, projectID, owner string) ([]schedule.Schedule, error) {
	q := `SELECT ` + schedCols + ` FROM work_schedules WHERE project_id = $1`
	args := []any{projectID}
	if projectID == "" {
		q += ` AND owner = $2`
		args = append(args, owner)
	}
	rows, err := s.pool.Query(ctx, q+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []schedule.Schedule{}
	for rows.Next() {
		sc, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sc)
	}
	return out, rows.Err()
}

// expireLost marks the runs past their deadline as failed.
func expireLost(ctx context.Context, tx pgx.Tx, now time.Time, scheduleID string) error {
	rows, err := tx.Query(ctx, `UPDATE work_schedule_runs SET status = 'failed', error = $2, finished_at = $1
		WHERE status = 'running' AND deadline < $1 AND ($3 = '' OR schedule_id = $3) RETURNING schedule_id`, now, LostError, scheduleID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.Exec(ctx, `UPDATE work_schedules SET last_status = 'failed' WHERE id = $1`, id); err != nil {
			return err
		}
	}
	return nil
}

func insertRun(ctx context.Context, tx pgx.Tx, r schedule.Run) (bool, error) {
	tag, err := tx.Exec(ctx, `INSERT INTO work_schedule_runs (id, schedule_id, fire_at, trigger, status, error, started_by, started_at, finished_at, deadline)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT DO NOTHING`,
		r.ID, r.ScheduleID, r.FireAt.UTC(), r.Trigger, r.Status, r.Error, r.StartedBy, r.StartedAt.UTC(), ts(r.FinishedAt), r.Deadline.UTC())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func hasRunning(ctx context.Context, tx pgx.Tx, scheduleID string) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM work_schedule_runs WHERE schedule_id = $1 AND status = 'running')`, scheduleID).Scan(&ok)
	return ok, err
}

// ClaimDue implements schedule.Store.
func (s *PG) ClaimDue(ctx context.Context, now time.Time, next func(schedule.Schedule, time.Time) time.Time) ([]schedule.Claim, error) {
	now = now.UTC()
	var claims []schedule.Claim
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if err := expireLost(ctx, tx, now, ""); err != nil {
			return fmt.Errorf("expire lost runs: %w", err)
		}
		rows, err := tx.Query(ctx, `SELECT `+schedCols+` FROM work_schedules
			WHERE enabled AND next_run_at IS NOT NULL AND next_run_at <= $1 ORDER BY id COLLATE "C" FOR UPDATE SKIP LOCKED`, now)
		if err != nil {
			return fmt.Errorf("select due schedules: %w", err)
		}
		var due []schedule.Schedule
		for rows.Next() {
			sc, err := scanSchedule(rows)
			if err != nil {
				rows.Close()
				return err
			}
			due = append(due, *sc)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, sc := range due {
			fireAt := sc.NextRunAt
			nextAt := next(sc, now).UTC()
			busy, err := hasRunning(ctx, tx, sc.ID)
			if err != nil {
				return err
			}
			r := newRun(sc, schedule.TriggerSchedule, fireAt, now, sc.RunAs)
			status := sc.LastStatus
			if busy {
				r.Status, r.FinishedAt, r.Error = schedule.StatusSkipped, now, "the previous run was still in progress"
			} else {
				status = schedule.StatusRunning
			}
			fresh, err := insertRun(ctx, tx, r)
			if err != nil {
				return fmt.Errorf("insert run: %w", err)
			}
			if _, err := tx.Exec(ctx, `UPDATE work_schedules SET last_run_at = $2, next_run_at = $3, last_status = $4 WHERE id = $1`,
				sc.ID, now, ts(nextAt), status); err != nil {
				return fmt.Errorf("advance schedule %s: %w", sc.ID, err)
			}
			if fresh && !busy {
				sc.LastRunAt, sc.NextRunAt, sc.LastStatus = now, nextAt, status
				claims = append(claims, schedule.Claim{Schedule: sc, Run: r})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claims, nil
}

// StartManual implements schedule.Store.
func (s *PG) StartManual(ctx context.Context, id, by string, now time.Time) (*schedule.Claim, error) {
	now = now.UTC()
	var claim *schedule.Claim
	err := s.tx(ctx, func(tx pgx.Tx) error {
		sc, err := scanSchedule(tx.QueryRow(ctx, `SELECT `+schedCols+` FROM work_schedules WHERE id = $1 FOR UPDATE`, id))
		if err != nil {
			return err
		}
		if err := expireLost(ctx, tx, now, id); err != nil {
			return err
		}
		busy, err := hasRunning(ctx, tx, id)
		if err != nil {
			return err
		}
		if busy {
			return schedule.ErrBusy
		}
		r := newRun(*sc, schedule.TriggerManual, now, now, by)
		if _, err := insertRun(ctx, tx, r); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE work_schedules SET last_run_at = $2, last_status = 'running' WHERE id = $1`, id, now); err != nil {
			return err
		}
		sc.LastRunAt, sc.LastStatus = now, schedule.StatusRunning
		claim = &schedule.Claim{Schedule: *sc, Run: r}
		return nil
	})
	return claim, err
}

// FinishRun implements schedule.Store.
func (s *PG) FinishRun(ctx context.Context, runID string, f schedule.Finish, now time.Time) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		var scheduleID, thread string
		err := tx.QueryRow(ctx, `UPDATE work_schedule_runs SET status=$2, error=$3, finished_at=$4,
			thread_id = CASE WHEN $5 <> '' THEN $5 ELSE thread_id END, agent_run_id = CASE WHEN $6 <> '' THEN $6 ELSE agent_run_id END,
			input_tokens=$7, output_tokens=$8, cache_read=$9, cache_write=$10, cost_usd=$11, unpriced=$12
			WHERE id=$1 AND status='running' RETURNING schedule_id, thread_id`,
			runID, f.Status, f.Error, now.UTC(), f.ThreadID, f.AgentRun, f.Usage.InputTokens, f.Usage.OutputTokens,
			f.Usage.CacheReadTokens, f.Usage.CacheWriteTokens, f.Usage.CostUSD, f.Usage.Unpriced).Scan(&scheduleID, &thread)
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM work_schedule_runs WHERE id = $1)`, runID).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return schedule.ErrNotFound
			}
			return nil // finished already: the first outcome stands
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE work_schedules SET last_status = $2, thread_id = CASE WHEN reuse_session AND $3 <> '' THEN $3 ELSE thread_id END WHERE id = $1`,
			scheduleID, f.Status, thread)
		return err
	})
}

// SetThread implements schedule.Store.
func (s *PG) SetThread(ctx context.Context, runID, threadID, agentRunID string) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		var scheduleID string
		err := tx.QueryRow(ctx, `UPDATE work_schedule_runs SET thread_id = $2, agent_run_id = CASE WHEN $3 <> '' THEN $3 ELSE agent_run_id END
			WHERE id = $1 RETURNING schedule_id`, runID, threadID, agentRunID).Scan(&scheduleID)
		if errors.Is(err, pgx.ErrNoRows) {
			return schedule.ErrNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE work_schedules SET thread_id = $2 WHERE id = $1 AND reuse_session`, scheduleID, threadID)
		return err
	})
}

// Runs implements schedule.Store.
func (s *PG) Runs(ctx context.Context, scheduleID string, limit int) ([]schedule.Run, error) {
	if limit <= 0 {
		limit = 50
	}
	if _, err := s.Get(ctx, scheduleID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+runCols+` FROM work_schedule_runs WHERE schedule_id = $1 ORDER BY started_at DESC, id DESC LIMIT $2`, scheduleID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []schedule.Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// RunOfThread implements schedule.Store.
func (s *PG) RunOfThread(ctx context.Context, threadID string) (*schedule.Run, error) {
	if threadID == "" {
		return nil, schedule.ErrNotFound
	}
	return scanRun(s.pool.QueryRow(ctx, `SELECT `+runCols+` FROM work_schedule_runs WHERE thread_id = $1 ORDER BY started_at DESC LIMIT 1`, threadID))
}
