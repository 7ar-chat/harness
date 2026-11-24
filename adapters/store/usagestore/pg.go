package usagestore

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/7ar-chat/harness/core/usage"
)

// PG is a usage.Store on Postgres (tables of migration 0580, applied by pgstore). Several processes may share it: a
// run is booked once by the primary key (run id, model), never by a read followed by a write.
type PG struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

var _ usage.Store = (*PG)(nil)

// NewPG returns a store on pool, whose search_path names the schema the migrations ran in. now (nil: time.Now) is the clock.
func NewPG(pool *pgxpool.Pool, now func() time.Time) *PG {
	if now == nil {
		now = time.Now
	}
	return &PG{pool: pool, now: now}
}

// Record implements usage.Store. The first writer of a run wins: a run that already has entries is left alone.
func (s *PG) Record(ctx context.Context, entries []usage.Entry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("usagestore: begin: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	booked := map[string]bool{}
	for _, e := range entries {
		if e.RunID == "" {
			return fmt.Errorf("%w: entry without a run id", usage.ErrInvalid)
		}
		if _, ok := booked[e.RunID]; !ok {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM work_usage WHERE run_id = $1)`, e.RunID).Scan(&exists); err != nil {
				return fmt.Errorf("usagestore: check run: %w", err)
			}
			booked[e.RunID] = exists
		}
		if booked[e.RunID] {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO work_usage
			(run_id, model, thread_id, project_id, usr, source, source_id, input_tokens, output_tokens, cache_read, cache_write, cost_usd, unpriced, at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
			ON CONFLICT (run_id, model) DO UPDATE SET
			  input_tokens = work_usage.input_tokens + EXCLUDED.input_tokens,
			  output_tokens = work_usage.output_tokens + EXCLUDED.output_tokens,
			  cache_read = work_usage.cache_read + EXCLUDED.cache_read,
			  cache_write = work_usage.cache_write + EXCLUDED.cache_write,
			  cost_usd = work_usage.cost_usd + EXCLUDED.cost_usd`,
			e.RunID, e.Model, e.ThreadID, e.ProjectID, e.User, e.Source, e.SourceID,
			e.InputTokens, e.OutputTokens, e.CacheReadTokens, e.CacheWriteTokens, e.CostUSD, e.Unpriced, e.At.UTC()); err != nil {
			return fmt.Errorf("usagestore: insert: %w", err)
		}
	}
	return tx.Commit(ctx)
}

const selectCols = `run_id, thread_id, project_id, usr, source, source_id, model, input_tokens, output_tokens, cache_read, cache_write, cost_usd, unpriced, at`

func where(q usage.Query) (string, []any) {
	cond, args := "TRUE", []any{}
	add := func(sql string, v any) {
		args = append(args, v)
		cond += fmt.Sprintf(" AND "+sql, len(args))
	}
	if q.ProjectID != "" {
		add("project_id = $%d", q.ProjectID)
	}
	if q.User != "" {
		add("usr = $%d", q.User)
	}
	if q.ThreadID != "" {
		add("thread_id = $%d", q.ThreadID)
	}
	if !q.From.IsZero() {
		add("at >= $%d", q.From.UTC())
	}
	if !q.To.IsZero() {
		add("at < $%d", q.To.UTC())
	}
	return cond, args
}

// Report implements usage.Store. The aggregation runs over the selected rows in Go, with the same code as Mem, so the
// stores cannot disagree about how runs are counted.
func (s *PG) Report(ctx context.Context, q usage.Query) (*usage.Report, error) {
	if !usage.ValidGroup(q.Group) {
		return nil, fmt.Errorf("%w: group %q", usage.ErrInvalid, q.Group)
	}
	cond, args := where(q)
	rows, err := s.pool.Query(ctx, `SELECT `+selectCols+` FROM work_usage WHERE `+cond+` ORDER BY at, run_id, model`, args...)
	if err != nil {
		return nil, fmt.Errorf("usagestore: query: %w", err)
	}
	defer rows.Close()
	var entries []usage.Entry
	for rows.Next() {
		var e usage.Entry
		if err := rows.Scan(&e.RunID, &e.ThreadID, &e.ProjectID, &e.User, &e.Source, &e.SourceID, &e.Model,
			&e.InputTokens, &e.OutputTokens, &e.CacheReadTokens, &e.CacheWriteTokens, &e.CostUSD, &e.Unpriced, &e.At); err != nil {
			return nil, fmt.Errorf("usagestore: scan: %w", err)
		}
		e.At = e.At.UTC()
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return usage.Aggregate(entries, q), nil
}

// Spent implements usage.Store.
func (s *PG) Spent(ctx context.Context, projectID, user string, from time.Time) (float64, error) {
	cond, args := where(usage.Query{ProjectID: projectID, User: user, From: from})
	var sum float64
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(SUM(cost_usd), 0) FROM work_usage WHERE `+cond, args...).Scan(&sum); err != nil {
		return 0, fmt.Errorf("usagestore: spent: %w", err)
	}
	return sum, nil
}

// SetCap implements usage.Store.
func (s *PG) SetCap(ctx context.Context, scope, id string, usd float64) error {
	if err := checkCap(scope, id, usd); err != nil {
		return err
	}
	if usd == 0 {
		_, err := s.pool.Exec(ctx, `DELETE FROM work_spend_caps WHERE scope = $1 AND scope_id = $2`, scope, id)
		return err
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO work_spend_caps (scope, scope_id, monthly_usd, updated_at) VALUES ($1,$2,$3,$4)
		ON CONFLICT (scope, scope_id) DO UPDATE SET monthly_usd = EXCLUDED.monthly_usd, updated_at = EXCLUDED.updated_at`,
		scope, id, usd, s.now().UTC())
	return err
}

// Cap implements usage.Store.
func (s *PG) Cap(ctx context.Context, scope, id string) (float64, error) {
	var usd float64
	err := s.pool.QueryRow(ctx, `SELECT monthly_usd FROM work_spend_caps WHERE scope = $1 AND scope_id = $2`, scope, id).Scan(&usd)
	if err == pgx.ErrNoRows {
		return 0, nil
	}
	return usd, err
}
