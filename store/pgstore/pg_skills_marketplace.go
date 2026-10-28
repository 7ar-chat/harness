package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/GhaziBenDahmane/harness/store"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Skills marketplace sync (migration 0015).

const marketplaceSourceCols = `global_skill_id::text, repository_url, branch, source_path, namespace, commit_sha,
	declared_version, deprecated, last_synced_at`

// MarketplaceSources implements SkillMarketplaceStore.
func (s *PG) MarketplaceSources(ctx context.Context) ([]*store.MarketplaceSource, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+marketplaceSourceCols+` FROM work_skill_marketplace_sources ORDER BY source_path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.MarketplaceSource
	for rows.Next() {
		var m store.MarketplaceSource
		if err := rows.Scan(&m.GlobalSkillID, &m.RepositoryURL, &m.Branch, &m.SourcePath, &m.Namespace, &m.CommitSHA,
			&m.DeclaredVersion, &m.Deprecated, &m.LastSyncedAt); err != nil {
			return nil, err
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

// UpsertMarketplaceSource implements SkillMarketplaceStore.
func (s *PG) UpsertMarketplaceSource(ctx context.Context, m store.MarketplaceSource) error {
	if !validUUID(m.GlobalSkillID) {
		return store.ErrNotFound
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO work_skill_marketplace_sources (global_skill_id, repository_url, branch,
			source_path, namespace, commit_sha, declared_version, deprecated, last_synced_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())
		ON CONFLICT (global_skill_id) DO UPDATE SET repository_url = EXCLUDED.repository_url, branch = EXCLUDED.branch,
			source_path = EXCLUDED.source_path, namespace = EXCLUDED.namespace, commit_sha = EXCLUDED.commit_sha,
			declared_version = EXCLUDED.declared_version, deprecated = EXCLUDED.deprecated, last_synced_at = now()`,
		m.GlobalSkillID, store.CleanText(m.RepositoryURL), store.CleanText(m.Branch), store.CleanText(m.SourcePath), store.CleanText(m.Namespace),
		store.CleanText(m.CommitSHA), store.CleanText(m.DeclaredVersion), m.Deprecated)
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) && pgErr.SQLState() == "23503" { // foreign_key_violation: no such global skill
		return store.ErrNotFound
	}
	return err
}

// UpdateGlobalSkillInfo implements SkillMarketplaceStore.
func (s *PG) UpdateGlobalSkillInfo(ctx context.Context, id, displayName, description string) error {
	return s.execGlobal(ctx, `UPDATE work_global_skills SET display_name = $2, description = $3, updated_at = now()
		WHERE id = $1`, id, store.CleanText(displayName), store.CleanText(description))
}

// CreateMarketplaceRun implements SkillMarketplaceStore.
func (s *PG) CreateMarketplaceRun(ctx context.Context, r *store.MarketplaceRun) error {
	r.ID = uuid.NewString()
	if r.Status == "" {
		r.Status = store.MarketplaceRunRunning
	}
	return s.pool.QueryRow(ctx, `INSERT INTO work_skill_marketplace_runs (id, trigger, status, repository_url, branch, commit_sha)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING started_at`,
		r.ID, r.Trigger, r.Status, store.CleanText(r.RepositoryURL), store.CleanText(r.Branch), store.CleanText(r.CommitSHA)).Scan(&r.StartedAt)
}

func issuesJSON(in []store.MarketplaceIssue) string {
	if in == nil {
		in = []store.MarketplaceIssue{}
	}
	for i := range in {
		in[i].Path, in[i].Message = store.CleanText(in[i].Path), store.CleanText(in[i].Message)
	}
	b, _ := json.Marshal(in)
	return string(b)
}

// FinishMarketplaceRun implements SkillMarketplaceStore.
func (s *PG) FinishMarketplaceRun(ctx context.Context, r *store.MarketplaceRun) error {
	if !validUUID(r.ID) {
		return store.ErrNotFound
	}
	var finished *time.Time
	err := s.pool.QueryRow(ctx, `UPDATE work_skill_marketplace_runs SET status = $2, commit_sha = $3, discovered_count = $4,
			created_count = $5, updated_count = $6, unchanged_count = $7, skipped_count = $8, deprecated_count = $9,
			failed_count = $10, warnings = $11::jsonb, errors = $12::jsonb, finished_at = now()
		WHERE id = $1 RETURNING finished_at`,
		r.ID, r.Status, store.CleanText(r.CommitSHA), r.Discovered, r.Created, r.Updated, r.Unchanged, r.Skipped, r.Deprecated,
		r.Failed, issuesJSON(r.Warnings), issuesJSON(r.Errors)).Scan(&finished)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ErrNotFound
	}
	if err == nil {
		r.FinishedAt = finished
	}
	return err
}

// ListMarketplaceRuns implements SkillMarketplaceStore.
func (s *PG) ListMarketplaceRuns(ctx context.Context, limit int) ([]*store.MarketplaceRun, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text, trigger, status, repository_url, branch, commit_sha, discovered_count,
			created_count, updated_count, unchanged_count, skipped_count, deprecated_count, failed_count, warnings, errors,
			started_at, finished_at
		FROM work_skill_marketplace_runs ORDER BY started_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.MarketplaceRun
	for rows.Next() {
		var r store.MarketplaceRun
		var warnings, errs []byte
		if err := rows.Scan(&r.ID, &r.Trigger, &r.Status, &r.RepositoryURL, &r.Branch, &r.CommitSHA, &r.Discovered,
			&r.Created, &r.Updated, &r.Unchanged, &r.Skipped, &r.Deprecated, &r.Failed, &warnings, &errs,
			&r.StartedAt, &r.FinishedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(warnings, &r.Warnings)
		_ = json.Unmarshal(errs, &r.Errors)
		out = append(out, &r)
	}
	return out, rows.Err()
}

var _ store.SkillMarketplaceStore = (*PG)(nil)
