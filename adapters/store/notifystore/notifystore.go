// Package notifystore stores notification rules, channels, preferences and the delivery log (core/notify): in memory,
// in a JSON file, and in Postgres (migration 1200).
package notifystore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/7ar-chat/harness/core/notify"
)

// NewMem returns an empty in-memory store.
func NewMem(now func() time.Time) notify.Store { return notify.NewMemStore(now) }

// Open returns a store persisted to a JSON file (0600, replaced atomically). One process per file.
func Open(path string, now func() time.Time) (notify.Store, error) {
	s := notify.NewMemStore(now)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("notifystore: create directory of %s: %w", path, err)
	}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("notifystore: read %s: %w", path, err)
	default:
		var sn notify.Snapshot
		if err := json.Unmarshal(b, &sn); err != nil || sn.Version != 1 {
			return nil, fmt.Errorf("notifystore: %s is not a version 1 notifications file", path)
		}
		s.Load(sn)
	}
	s.OnChange = func(sn notify.Snapshot) error {
		b, err := json.Marshal(&sn)
		if err != nil {
			return err
		}
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			return fmt.Errorf("notifystore: write %s: %w", tmp, err)
		}
		if err := os.Rename(tmp, path); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("notifystore: replace %s: %w", path, err)
		}
		return nil
	}
	return s, nil
}

// PG is a store on Postgres (tables of migration 1200, applied by pgstore).
type PG struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

var _ notify.Store = (*PG)(nil)

// NewPG returns a store on pool, whose search_path names the schema the migrations ran in.
func NewPG(pool *pgxpool.Pool, now func() time.Time) *PG {
	if now == nil {
		now = time.Now
	}
	return &PG{pool: pool, now: now}
}

// GetRules implements notify.Store.
func (p *PG) GetRules(ctx context.Context, project string) (notify.Rules, error) {
	var doc []byte
	err := p.pool.QueryRow(ctx, `SELECT doc FROM work_notify_rules WHERE project_id = $1`, project).Scan(&doc)
	if errors.Is(err, pgx.ErrNoRows) {
		return notify.DefaultRules(project), nil
	}
	if err != nil {
		return notify.Rules{}, err
	}
	var r notify.Rules
	if err := json.Unmarshal(doc, &r); err != nil {
		return notify.Rules{}, err
	}
	if r.Events == nil {
		r.Events = map[notify.Kind]notify.Rule{}
	}
	return r, nil
}

// PutRules implements notify.Store.
func (p *PG) PutRules(ctx context.Context, r notify.Rules) error {
	r.UpdatedAt = p.now()
	doc, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx, `INSERT INTO work_notify_rules (project_id, doc, updated_at) VALUES ($1, $2, $3)
		ON CONFLICT (project_id) DO UPDATE SET doc = EXCLUDED.doc, updated_at = EXCLUDED.updated_at`, r.ProjectID, doc, r.UpdatedAt)
	return err
}

func scanChannel(row pgx.Row) (notify.Channel, error) {
	var doc []byte
	var c notify.Channel
	if err := row.Scan(&doc); err != nil {
		return c, err
	}
	return c, json.Unmarshal(doc, &c)
}

// ListChannels implements notify.Store.
func (p *PG) ListChannels(ctx context.Context, project string) ([]notify.Channel, error) {
	rows, err := p.pool.Query(ctx, `SELECT doc FROM work_notify_channels WHERE project_id = $1`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []notify.Channel{}
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	notify.SortChannels(out)
	return out, rows.Err()
}

// GetChannel implements notify.Store.
func (p *PG) GetChannel(ctx context.Context, project, id string) (notify.Channel, error) {
	c, err := scanChannel(p.pool.QueryRow(ctx, `SELECT doc FROM work_notify_channels WHERE id = $1 AND project_id = $2`, id, project))
	if errors.Is(err, pgx.ErrNoRows) {
		return notify.Channel{}, notify.ErrNotFound
	}
	return c, err
}

// PutChannel implements notify.Store.
func (p *PG) PutChannel(ctx context.Context, c notify.Channel) error {
	if c.CreatedAt.IsZero() {
		c.CreatedAt = p.now()
	}
	doc, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx, `INSERT INTO work_notify_channels (id, project_id, name, doc) VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, doc = EXCLUDED.doc WHERE work_notify_channels.project_id = EXCLUDED.project_id`,
		c.ID, c.ProjectID, c.Name, doc)
	return err
}

// DeleteChannel implements notify.Store.
func (p *PG) DeleteChannel(ctx context.Context, project, id string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM work_notify_channels WHERE id = $1 AND project_id = $2`, id, project)
	return err
}

// GetPrefs implements notify.Store.
func (p *PG) GetPrefs(ctx context.Context, login string) (notify.Prefs, error) {
	var doc []byte
	err := p.pool.QueryRow(ctx, `SELECT doc FROM work_notify_prefs WHERE login = $1`, login).Scan(&doc)
	if errors.Is(err, pgx.ErrNoRows) {
		return notify.Prefs{Login: login}, nil
	}
	if err != nil {
		return notify.Prefs{}, err
	}
	var pr notify.Prefs
	return pr, json.Unmarshal(doc, &pr)
}

// PutPrefs implements notify.Store.
func (p *PG) PutPrefs(ctx context.Context, pr notify.Prefs) error {
	pr.UpdatedAt = p.now()
	doc, err := json.Marshal(pr)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx, `INSERT INTO work_notify_prefs (login, doc, updated_at) VALUES ($1, $2, $3)
		ON CONFLICT (login) DO UPDATE SET doc = EXCLUDED.doc, updated_at = EXCLUDED.updated_at`, pr.Login, doc, pr.UpdatedAt)
	return err
}

// AppendLog implements notify.Store.
func (p *PG) AppendLog(ctx context.Context, e notify.LogEntry) error {
	if e.At.IsZero() {
		e.At = p.now()
	}
	doc, err := json.Marshal(e)
	if err != nil {
		return err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO work_notify_log (project_id, login, doc) VALUES ($1, $2, $3)`, e.ProjectID, e.Login, doc); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM work_notify_log WHERE project_id = $1 AND seq <=
		(SELECT seq FROM work_notify_log WHERE project_id = $1 ORDER BY seq DESC OFFSET $2 LIMIT 1)`, e.ProjectID, notify.MaxLog); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ListLog implements notify.Store.
func (p *PG) ListLog(ctx context.Context, project, login string, limit int) ([]notify.LogEntry, error) {
	if limit <= 0 || limit > notify.MaxLog {
		limit = notify.MaxLog
	}
	rows, err := p.pool.Query(ctx, `SELECT doc FROM work_notify_log WHERE project_id = $1 AND ($2 = '' OR login = $2)
		ORDER BY seq DESC LIMIT $3`, project, login, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []notify.LogEntry{}
	for rows.Next() {
		var doc []byte
		if err := rows.Scan(&doc); err != nil {
			return nil, err
		}
		var e notify.LogEntry
		if err := json.Unmarshal(doc, &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteProject implements notify.Store.
func (p *PG) DeleteProject(ctx context.Context, project string) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, q := range []string{
		`DELETE FROM work_notify_rules WHERE project_id = $1`,
		`DELETE FROM work_notify_channels WHERE project_id = $1`,
		`DELETE FROM work_notify_log WHERE project_id = $1`,
	} {
		if _, err := tx.Exec(ctx, q, project); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
