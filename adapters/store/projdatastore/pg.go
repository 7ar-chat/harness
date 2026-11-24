package projdatastore

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/7ar-chat/harness/core/projdata"
)

// PG is a projdata.Store on Postgres (tables of migration 1400, applied by pgstore). Replicas share it; a slot claim
// is one INSERT ... ON CONFLICT DO NOTHING, so exactly one caller wins.
type PG struct{ pool *pgxpool.Pool }

var _ projdata.Store = (*PG)(nil)

// NewPG returns a store on pool, whose search_path names the schema the migrations ran in.
func NewPG(pool *pgxpool.Pool) *PG { return &PG{pool: pool} }

// GetDigestSettings implements projdata.Store.
func (p *PG) GetDigestSettings(ctx context.Context, project string) (projdata.DigestSettings, error) {
	var doc []byte
	err := p.pool.QueryRow(ctx, `SELECT doc FROM work_digest_settings WHERE project_id = $1`, project).Scan(&doc)
	if errors.Is(err, pgx.ErrNoRows) {
		return projdata.DigestSettings{ProjectID: project}, nil
	}
	if err != nil {
		return projdata.DigestSettings{}, err
	}
	var s projdata.DigestSettings
	return s, json.Unmarshal(doc, &s)
}

// PutDigestSettings implements projdata.Store.
func (p *PG) PutDigestSettings(ctx context.Context, s projdata.DigestSettings) error {
	doc, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx, `INSERT INTO work_digest_settings (project_id, doc, enabled) VALUES ($1, $2, $3)
		ON CONFLICT (project_id) DO UPDATE SET doc = EXCLUDED.doc, enabled = EXCLUDED.enabled`, s.ProjectID, doc, s.Enabled)
	return err
}

// EnabledDigests implements projdata.Store.
func (p *PG) EnabledDigests(ctx context.Context) ([]projdata.DigestSettings, error) {
	rows, err := p.pool.Query(ctx, `SELECT doc FROM work_digest_settings WHERE enabled ORDER BY project_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []projdata.DigestSettings
	for rows.Next() {
		var doc []byte
		var s projdata.DigestSettings
		if err := rows.Scan(&doc); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(doc, &s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ClaimSlot implements projdata.Store.
func (p *PG) ClaimSlot(ctx context.Context, project, slot string, at time.Time) (bool, error) {
	tag, err := p.pool.Exec(ctx, `INSERT INTO work_digest_slots (project_id, slot, claimed_at) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, project, slot, at)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ReleaseSlot implements projdata.Store.
func (p *PG) ReleaseSlot(ctx context.Context, project, slot string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM work_digest_slots WHERE project_id = $1 AND slot = $2`, project, slot)
	return err
}

// AddDigest implements projdata.Store.
func (p *PG) AddDigest(ctx context.Context, d projdata.Digest) error {
	html, report := d.HTML, d.JSON
	d.HTML, d.JSON = nil, nil
	doc, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if html == nil {
		html = []byte{}
	}
	if report == nil {
		report = []byte{}
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO work_digests (id, project_id, created_at, doc, html, report) VALUES ($1, $2, $3, $4, $5, $6)`,
		d.ID, d.ProjectID, d.CreatedAt, doc, html, report); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM work_digests WHERE project_id = $1 AND id NOT IN
		(SELECT id FROM work_digests WHERE project_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2)`, d.ProjectID, projdata.KeepDigests); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ListDigests implements projdata.Store.
func (p *PG) ListDigests(ctx context.Context, project string, limit int) ([]projdata.Digest, error) {
	if limit <= 0 {
		limit = projdata.KeepDigests
	}
	rows, err := p.pool.Query(ctx, `SELECT doc FROM work_digests WHERE project_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2`, project, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []projdata.Digest{}
	for rows.Next() {
		var doc []byte
		var d projdata.Digest
		if err := rows.Scan(&doc); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(doc, &d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetDigest implements projdata.Store.
func (p *PG) GetDigest(ctx context.Context, project, id string) (projdata.Digest, error) {
	var doc, html, report []byte
	err := p.pool.QueryRow(ctx, `SELECT doc, html, report FROM work_digests WHERE id = $1 AND project_id = $2`, id, project).Scan(&doc, &html, &report)
	if errors.Is(err, pgx.ErrNoRows) {
		return projdata.Digest{}, projdata.ErrNotFound
	}
	if err != nil {
		return projdata.Digest{}, err
	}
	var d projdata.Digest
	if err := json.Unmarshal(doc, &d); err != nil {
		return d, err
	}
	d.HTML, d.JSON = html, report
	return d, nil
}

// PutSecret implements projdata.Store.
func (p *PG) PutSecret(ctx context.Context, project, kind string, sealed []byte) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO work_project_secrets (project_id, kind, sealed) VALUES ($1, $2, $3)
		ON CONFLICT (project_id, kind) DO UPDATE SET sealed = EXCLUDED.sealed`, project, kind, sealed)
	return err
}

// GetSecret implements projdata.Store.
func (p *PG) GetSecret(ctx context.Context, project, kind string) ([]byte, error) {
	var b []byte
	err := p.pool.QueryRow(ctx, `SELECT sealed FROM work_project_secrets WHERE project_id = $1 AND kind = $2`, project, kind).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, projdata.ErrNotFound
	}
	return b, err
}

// DeleteSecret implements projdata.Store.
func (p *PG) DeleteSecret(ctx context.Context, project, kind string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM work_project_secrets WHERE project_id = $1 AND kind = $2`, project, kind)
	return err
}

// DeleteProject implements projdata.Store.
func (p *PG) DeleteProject(ctx context.Context, project string) error {
	for _, q := range []string{
		`DELETE FROM work_digest_settings WHERE project_id = $1`, `DELETE FROM work_digest_slots WHERE project_id = $1`,
		`DELETE FROM work_digests WHERE project_id = $1`, `DELETE FROM work_project_secrets WHERE project_id = $1`,
	} {
		if _, err := p.pool.Exec(ctx, q, project); err != nil {
			return err
		}
	}
	return nil
}
