package pgstore

import (
	"context"
	"errors"
	"github.com/GhaziBenDahmane/harness/store"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Skill catalog (migration 0008): versions, global skills, idempotent
// commands and skill creation sessions.

const versionCols = `id::text, coalesce(private_skill_id, global_skill_id)::text, global_skill_id IS NOT NULL, version, status,
	source_type, name, description, sha256, files, size_bytes, created_by, created_at`

func scanVersion(row pgx.Row, withPackage bool) (*store.SkillVersion, error) {
	var v store.SkillVersion
	dest := []any{&v.ID, &v.SkillID, &v.Global, &v.Version, &v.Status, &v.SourceType, &v.Name, &v.Description, &v.SHA256,
		&v.Files, &v.SizeBytes, &v.CreatedBy, &v.CreatedAt}
	if withPackage {
		dest = append(dest, &v.Package)
	}
	if err := row.Scan(dest...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return &v, nil
}

// insertVersion inserts v (new id) for a private or global skill.
func insertVersion(ctx context.Context, tx pgx.Tx, v *store.SkillVersion) error {
	v.ID = uuid.NewString()
	if v.Status == "" {
		v.Status = store.SkillDraft
	}
	if v.SourceType == "" {
		v.SourceType = store.SkillSourceUpload
	}
	var private, global *string
	if v.Global {
		global = &v.SkillID
	} else {
		private = &v.SkillID
	}
	err := tx.QueryRow(ctx, `INSERT INTO work_skill_versions (id, private_skill_id, global_skill_id, version, status, source_type,
			name, description, sha256, package, files, size_bytes, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) RETURNING created_at`,
		v.ID, private, global, store.CleanText(v.Version), v.Status, v.SourceType, store.CleanText(v.Name), store.CleanText(v.Description),
		v.SHA256, v.Package, v.Files, v.SizeBytes, v.CreatedBy).Scan(&v.CreatedAt)
	if isUniqueViolation(err) {
		return store.ErrConflict
	}
	return err
}

// refreshPrivateEffective copies the effective version (published, else latest)
// into work_skills (package, digest, description) so the skill reads like before.
func refreshPrivateEffective(ctx context.Context, tx pgx.Tx, skillID string) error {
	_, err := tx.Exec(ctx, `UPDATE work_skills s SET package = v.package, sha256 = v.sha256, files = v.files,
			size_bytes = v.size_bytes, description = v.description, display_name = v.name, updated_at = now()
		FROM work_skill_versions v
		WHERE s.id = $1 AND v.id = coalesce(s.published_version_id, s.latest_version_id)`, skillID)
	return err
}

// SkillMetas implements SkillCatalogStore.
func (s *PG) SkillMetas(ctx context.Context, login string) (map[string]store.SkillMeta, error) {
	rows, err := s.pool.Query(ctx, `SELECT id::text, display_name, coalesce(published_version_id::text, ''),
		coalesce(latest_version_id::text, '') FROM work_skills WHERE user_login = $1`, login)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]store.SkillMeta{}
	for rows.Next() {
		var m store.SkillMeta
		if err := rows.Scan(&m.SkillID, &m.DisplayName, &m.PublishedVersionID, &m.LatestVersionID); err != nil {
			return nil, err
		}
		out[m.SkillID] = m
	}
	return out, rows.Err()
}

// ListSkillVersions implements SkillCatalogStore.
func (s *PG) ListSkillVersions(ctx context.Context, skillIDs []string) (map[string][]*store.SkillVersion, error) {
	ids := validUUIDs(skillIDs)
	out := map[string][]*store.SkillVersion{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+versionCols+` FROM work_skill_versions
		WHERE private_skill_id = ANY($1::uuid[]) OR global_skill_id = ANY($1::uuid[])
		ORDER BY created_at DESC, id DESC`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		v, err := scanVersion(rows, false)
		if err != nil {
			return nil, err
		}
		out[v.SkillID] = append(out[v.SkillID], v)
	}
	return out, rows.Err()
}

// GetSkillVersion implements SkillCatalogStore.
func (s *PG) GetSkillVersion(ctx context.Context, versionID string) (*store.SkillVersion, error) {
	if !validUUID(versionID) {
		return nil, store.ErrNotFound
	}
	return scanVersion(s.pool.QueryRow(ctx, `SELECT `+versionCols+`, package FROM work_skill_versions WHERE id = $1`, versionID), true)
}

// lockSkill locks the private skill of login or the global skill.
func lockSkill(ctx context.Context, tx pgx.Tx, login, skillID string, global bool) error {
	if !validUUID(skillID) {
		return store.ErrNotFound
	}
	var err error
	var id string
	if global {
		err = tx.QueryRow(ctx, `SELECT id::text FROM work_global_skills WHERE id = $1 FOR UPDATE`, skillID).Scan(&id)
	} else {
		err = tx.QueryRow(ctx, `SELECT id::text FROM work_skills WHERE id = $1 AND user_login = $2 FOR UPDATE`, skillID, login).Scan(&id)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ErrNotFound
	}
	return err
}

// AddSkillVersion implements SkillCatalogStore.
func (s *PG) AddSkillVersion(ctx context.Context, login string, v store.SkillVersion, publish bool) (*store.SkillVersion, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockSkill(ctx, tx, login, v.SkillID, v.Global); err != nil {
		return nil, err
	}
	v.Status = store.SkillDraft
	if publish {
		v.Status = store.SkillPublished
	}
	if err := insertVersion(ctx, tx, &v); err != nil {
		return nil, err
	}
	if v.Global {
		_, err = tx.Exec(ctx, `UPDATE work_global_skills SET latest_version_id = $2::uuid,
				published_version_id = CASE WHEN $3 THEN $2 ELSE published_version_id END, updated_at = now() WHERE id = $1`,
			v.SkillID, v.ID, publish)
	} else {
		_, err = tx.Exec(ctx, `UPDATE work_skills SET latest_version_id = $2::uuid, version = version + 1,
				published_version_id = CASE WHEN $3 THEN $2 ELSE published_version_id END WHERE id = $1`,
			v.SkillID, v.ID, publish)
		if err == nil {
			err = refreshPrivateEffective(ctx, tx, v.SkillID)
		}
	}
	if err == nil && publish {
		err = syncPublished(ctx, tx, v.SkillID, v.Global)
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	v.Package = nil
	return &v, nil
}

// syncPublished keeps one published version per skill, like Work's single
// published_version pointer: the pointed version is published, the others drafts.
func syncPublished(ctx context.Context, tx pgx.Tx, skillID string, global bool) error {
	table, col := "work_skills", "private_skill_id"
	if global {
		table, col = "work_global_skills", "global_skill_id"
	}
	_, err := tx.Exec(ctx, `UPDATE work_skill_versions v SET status = CASE WHEN v.id = s.published_version_id THEN 'published' ELSE 'draft' END
		FROM `+table+` s WHERE s.id = $1 AND v.`+col+` = s.id
			AND v.status IS DISTINCT FROM CASE WHEN v.id = s.published_version_id THEN 'published' ELSE 'draft' END`, skillID)
	return err
}

// PublishSkillVersion implements SkillCatalogStore.
func (s *PG) PublishSkillVersion(ctx context.Context, login, skillID, versionID string, global bool) error {
	if !validUUID(versionID) {
		return store.ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockSkill(ctx, tx, login, skillID, global); err != nil {
		return err
	}
	col := "private_skill_id"
	if global {
		col = "global_skill_id"
	}
	tag, err := tx.Exec(ctx, `UPDATE work_skill_versions SET status = 'published' WHERE id = $1 AND `+col+` = $2`, versionID, skillID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	if global {
		_, err = tx.Exec(ctx, `UPDATE work_global_skills SET published_version_id = $2, updated_at = now() WHERE id = $1`, skillID, versionID)
	} else {
		_, err = tx.Exec(ctx, `UPDATE work_skills SET published_version_id = $2 WHERE id = $1`, skillID, versionID)
		if err == nil {
			err = refreshPrivateEffective(ctx, tx, skillID)
		}
	}
	if err == nil {
		err = syncPublished(ctx, tx, skillID, global)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CreateSkillDrafts implements SkillCatalogStore.
func (s *PG) CreateSkillDrafts(ctx context.Context, idem store.Idempotency, drafts []store.NewSkillDraft, response func([]store.DraftItem) []byte) ([]byte, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Claim the key first: a concurrent identical command waits on this row.
	tag, err := tx.Exec(ctx, `INSERT INTO work_skill_commands (user_login, command, idempotency_key, request_hash, status_code, response)
		VALUES ($1, $2, $3, $4, 201, '') ON CONFLICT DO NOTHING`, idem.Login, idem.Command, idem.Key, idem.RequestHash)
	if err != nil {
		return nil, false, err
	}
	if tag.RowsAffected() == 0 {
		_ = tx.Rollback(ctx)
		r, err := s.CommandResult(ctx, idem.Login, idem.Command, idem.Key)
		if err != nil {
			return nil, false, err
		}
		if r.RequestHash != idem.RequestHash {
			return nil, false, store.ErrIdempotencyConflict
		}
		return r.Response, true, nil
	}
	items := make([]store.DraftItem, 0, len(drafts))
	for _, d := range drafts {
		id := uuid.NewString()
		v := d.Version
		_, err := tx.Exec(ctx, `INSERT INTO work_skills (id, user_login, name, display_name, description, version, sha256, package, files, size_bytes)
			VALUES ($1, $2, $3, $4, $5, 1, $6, $7, $8, $9)`, id, idem.Login, store.CleanText(d.Name), store.CleanText(d.DisplayName),
			store.CleanText(d.Description), v.SHA256, v.Package, v.Files, v.SizeBytes)
		if isUniqueViolation(err) {
			return nil, false, store.ErrConflict
		}
		if err != nil {
			return nil, false, err
		}
		v.SkillID, v.Global = id, false
		v.Status = store.SkillDraft
		if d.Publish {
			v.Status = store.SkillPublished
		}
		if err := insertVersion(ctx, tx, &v); err != nil {
			return nil, false, err
		}
		if _, err := tx.Exec(ctx, `UPDATE work_skills SET latest_version_id = $2::uuid,
				published_version_id = CASE WHEN $3 THEN $2::uuid ELSE NULL END WHERE id = $1`, id, v.ID, d.Publish); err != nil {
			return nil, false, err
		}
		items = append(items, store.DraftItem{SkillID: id, VersionID: v.ID, Name: d.DisplayName})
	}
	body := response(items)
	if _, err := tx.Exec(ctx, `UPDATE work_skill_commands SET response = $4
		WHERE user_login = $1 AND command = $2 AND idempotency_key = $3`, idem.Login, idem.Command, idem.Key, string(body)); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return body, false, nil
}

const globalCols = `id::text, name, display_name, description, tags, coalesce(published_version_id::text, ''),
	coalesce(latest_version_id::text, ''), created_by, created_at, updated_at`

func scanGlobal(row pgx.Row) (*store.GlobalSkill, error) {
	var g store.GlobalSkill
	if err := row.Scan(&g.ID, &g.Name, &g.DisplayName, &g.Description, &g.Tags, &g.PublishedVersionID, &g.LatestVersionID,
		&g.CreatedBy, &g.CreatedAt, &g.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	if g.Tags == nil {
		g.Tags = []string{}
	}
	return &g, nil
}

// ListGlobalSkills implements SkillCatalogStore.
func (s *PG) ListGlobalSkills(ctx context.Context) ([]*store.GlobalSkill, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+globalCols+` FROM work_global_skills ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.GlobalSkill
	for rows.Next() {
		g, err := scanGlobal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// GetGlobalSkill implements SkillCatalogStore.
func (s *PG) GetGlobalSkill(ctx context.Context, id string) (*store.GlobalSkill, error) {
	if !validUUID(id) {
		return nil, store.ErrNotFound
	}
	return scanGlobal(s.pool.QueryRow(ctx, `SELECT `+globalCols+` FROM work_global_skills WHERE id = $1`, id))
}

// CreateGlobalSkill implements SkillCatalogStore.
func (s *PG) CreateGlobalSkill(ctx context.Context, g store.GlobalSkill, v store.SkillVersion, publish bool) (*store.GlobalSkill, *store.SkillVersion, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	g.ID = uuid.NewString()
	if g.Tags == nil {
		g.Tags = []string{}
	}
	_, err = tx.Exec(ctx, `INSERT INTO work_global_skills (id, name, display_name, description, tags, created_by)
		VALUES ($1, $2, $3, $4, $5, $6)`, g.ID, store.CleanText(g.Name), store.CleanText(g.DisplayName), store.CleanText(g.Description), g.Tags, g.CreatedBy)
	if isUniqueViolation(err) {
		return nil, nil, store.ErrConflict
	}
	if err != nil {
		return nil, nil, err
	}
	v.SkillID, v.Global = g.ID, true
	v.Status = store.SkillDraft
	if publish {
		v.Status = store.SkillPublished
	}
	if err := insertVersion(ctx, tx, &v); err != nil {
		return nil, nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE work_global_skills SET latest_version_id = $2::uuid,
			published_version_id = CASE WHEN $3 THEN $2::uuid ELSE NULL END WHERE id = $1`, g.ID, v.ID, publish); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	out, err := s.GetGlobalSkill(ctx, g.ID)
	v.Package = nil
	return out, &v, err
}

func (s *PG) execGlobal(ctx context.Context, sql string, args ...any) error {
	if !validUUID(args[0].(string)) {
		return store.ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// UnpublishGlobalSkill implements SkillCatalogStore.
func (s *PG) UnpublishGlobalSkill(ctx context.Context, id string) error {
	if !validUUID(id) {
		return store.ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE work_global_skills SET published_version_id = NULL, updated_at = now() WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	if err := syncPublished(ctx, tx, id, true); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SetGlobalSkillTags implements SkillCatalogStore.
func (s *PG) SetGlobalSkillTags(ctx context.Context, id string, tags []string) error {
	if tags == nil {
		tags = []string{}
	}
	return s.execGlobal(ctx, `UPDATE work_global_skills SET tags = $2, updated_at = now() WHERE id = $1`, id, tags)
}

// DeleteGlobalSkill implements SkillCatalogStore.
func (s *PG) DeleteGlobalSkill(ctx context.Context, id string) error {
	return s.execGlobal(ctx, `DELETE FROM work_global_skills WHERE id = $1`, id)
}

// CommandResult implements SkillCatalogStore.
func (s *PG) CommandResult(ctx context.Context, login, command, key string) (*store.CommandResult, error) {
	var r store.CommandResult
	var resp string
	err := s.pool.QueryRow(ctx, `SELECT request_hash, status_code, response FROM work_skill_commands
		WHERE user_login = $1 AND command = $2 AND idempotency_key = $3`, login, command, key).Scan(&r.RequestHash, &r.StatusCode, &resp)
	r.Response = []byte(resp)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// SaveCommandResult implements SkillCatalogStore.
func (s *PG) SaveCommandResult(ctx context.Context, idem store.Idempotency, r store.CommandResult) error {
	tag, err := s.pool.Exec(ctx, `INSERT INTO work_skill_commands (user_login, command, idempotency_key, request_hash, status_code, response)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (user_login, command, idempotency_key) DO UPDATE SET status_code = EXCLUDED.status_code, response = EXCLUDED.response
			WHERE work_skill_commands.status_code = 0 AND work_skill_commands.request_hash = EXCLUDED.request_hash`,
		idem.Login, idem.Command, idem.Key, idem.RequestHash, r.StatusCode, string(r.Response))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrConflict
	}
	return nil
}

// ClaimCommand implements SkillCatalogStore.
func (s *PG) ClaimCommand(ctx context.Context, idem store.Idempotency) (bool, error) {
	// A stale claim (its request died, B-18) or a result past its retention is taken over.
	tag, err := s.pool.Exec(ctx, `INSERT INTO work_skill_commands (user_login, command, idempotency_key, request_hash, status_code, response)
		VALUES ($1, $2, $3, $4, 0, '')
		ON CONFLICT (user_login, command, idempotency_key) DO UPDATE
			SET request_hash = EXCLUDED.request_hash, status_code = 0, response = '', created_at = now()
			WHERE (work_skill_commands.status_code = 0 AND work_skill_commands.created_at < now() - make_interval(secs => $5))
				OR work_skill_commands.created_at < now() - make_interval(secs => $6)`,
		idem.Login, idem.Command, idem.Key, idem.RequestHash, store.CommandClaimLease.Seconds(), store.CommandRetention.Seconds())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ReleaseCommand implements SkillCatalogStore.
func (s *PG) ReleaseCommand(ctx context.Context, idem store.Idempotency) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM work_skill_commands WHERE user_login = $1 AND command = $2 AND idempotency_key = $3
		AND status_code = 0`, idem.Login, idem.Command, idem.Key)
	return err
}

// CreateSkillCreationSession implements SkillCatalogStore.
func (s *PG) CreateSkillCreationSession(ctx context.Context, cs store.SkillCreationSession) error {
	if cs.Mode == "" {
		cs.Mode = "skill_creation"
	}
	if cs.Action == "" {
		cs.Action = "create"
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO work_skill_creation_sessions (thread_id, user_login, mode, action, package_root)
		VALUES ($1, $2, $3, $4, $5)`, cs.ThreadID, cs.UserLogin, cs.Mode, cs.Action, cs.PackageRoot)
	if isUniqueViolation(err) {
		return store.ErrConflict
	}
	return err
}

const sessionCols = `s.thread_id::text, s.user_login, s.mode, s.action, s.package_root, t.title, s.created_at`

func scanSession(row pgx.Row) (*store.SkillCreationSession, error) {
	var cs store.SkillCreationSession
	if err := row.Scan(&cs.ThreadID, &cs.UserLogin, &cs.Mode, &cs.Action, &cs.PackageRoot, &cs.Title, &cs.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return &cs, nil
}

// GetSkillCreationSession implements SkillCatalogStore.
func (s *PG) GetSkillCreationSession(ctx context.Context, login, threadID string) (*store.SkillCreationSession, error) {
	if !validUUID(threadID) {
		return nil, store.ErrNotFound
	}
	q := `SELECT ` + sessionCols + ` FROM work_skill_creation_sessions s JOIN work_threads t ON t.id = s.thread_id
		WHERE s.thread_id = $1 AND t.deleted_at IS NULL`
	args := []any{threadID}
	if login != "" {
		q += ` AND s.user_login = $2`
		args = append(args, login)
	}
	return scanSession(s.pool.QueryRow(ctx, q, args...))
}

// ListSkillCreationSessions implements SkillCatalogStore.
func (s *PG) ListSkillCreationSessions(ctx context.Context, login string, limit, offset int) ([]*store.SkillCreationSession, int, error) {
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM work_skill_creation_sessions s JOIN work_threads t ON t.id = s.thread_id
		WHERE s.user_login = $1 AND t.deleted_at IS NULL`, login).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+sessionCols+` FROM work_skill_creation_sessions s JOIN work_threads t ON t.id = s.thread_id
		WHERE s.user_login = $1 AND t.deleted_at IS NULL ORDER BY s.created_at DESC, s.thread_id DESC LIMIT $2 OFFSET $3`, login, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*store.SkillCreationSession
	for rows.Next() {
		cs, err := scanSession(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, cs)
	}
	return out, total, rows.Err()
}

var _ store.SkillCatalogStore = (*PG)(nil)
