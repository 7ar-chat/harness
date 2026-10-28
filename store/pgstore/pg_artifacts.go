package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/GhaziBenDahmane/harness/store"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func setRunSkills(ctx context.Context, tx pgx.Tx, runID string, skills []string) error {
	if len(skills) == 0 {
		return nil
	}
	clean := make([]string, len(skills))
	for i, s := range skills {
		clean[i] = store.CleanText(s)
	}
	_, err := tx.Exec(ctx, `UPDATE work_runs SET skills = $2 WHERE id = $1`, runID, clean)
	return err
}

// RunSkills implements SkillStore.
func (s *PG) RunSkills(ctx context.Context, runID string) ([]string, error) {
	if !validUUID(runID) {
		return nil, store.ErrNotFound
	}
	var out []string
	err := s.pool.QueryRow(ctx, `SELECT coalesce(skills, '{}') FROM work_runs WHERE id = $1`, runID).Scan(&out)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return out, err
}

const artifactCols = `id::text, thread_id::text, user_login, path, name, mime_type, kind, size_bytes, sha256, version,
	coalesce(run_id::text, ''), source_mtime, deleted_at IS NOT NULL, coalesce(sandbox_id, ''), restore_skipped_at, created_at, updated_at`

func scanArtifact(row pgx.Row) (*store.Artifact, error) {
	var a store.Artifact
	if err := row.Scan(&a.ID, &a.ThreadID, &a.UserLogin, &a.Path, &a.Name, &a.MimeType, &a.Kind, &a.SizeBytes, &a.SHA256,
		&a.Version, &a.RunID, &a.SourceMtime, &a.Deleted, &a.SandboxID, &a.RestoreSkippedAt, &a.CreatedAt, &a.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return &a, nil
}

func nullUUID(s string) *string {
	if s == "" || !validUUID(s) {
		return nil
	}
	return &s
}

// PutArtifactVersion implements ArtifactStore.
func (s *PG) PutArtifactVersion(ctx context.Context, in store.PutArtifact) (*store.Artifact, string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	cur, err := scanArtifact(tx.QueryRow(ctx, `SELECT `+artifactCols+` FROM work_artifacts
		WHERE thread_id = $1 AND path = $2 FOR UPDATE`, in.ThreadID, store.CleanText(in.Path)))
	runID := nullUUID(in.RunID)
	var out *store.Artifact
	change := ""
	switch {
	case errors.Is(err, store.ErrNotFound):
		id := uuid.NewString()
		out, err = scanArtifact(tx.QueryRow(ctx, `INSERT INTO work_artifacts
				(id, thread_id, user_login, path, name, mime_type, kind, size_bytes, sha256, version, run_id, source_mtime, sandbox_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 1, $10, $11, nullif($12, ''))
			ON CONFLICT (thread_id, path) DO NOTHING
			RETURNING `+artifactCols,
			id, in.ThreadID, in.UserLogin, store.CleanText(in.Path), store.CleanText(in.Name), in.MimeType, in.Kind, in.SizeBytes, in.SHA256, runID, in.Mtime,
			store.CleanText(in.SandboxID)))
		if errors.Is(err, store.ErrNotFound) {
			return nil, "", store.ErrConflict // concurrent creation of the same path; the caller retries
		}
		if err != nil {
			return nil, "", err
		}
		change = store.ChangeCreated
	case err != nil:
		return nil, "", err
	case cur.SHA256 == in.SHA256 && !cur.Deleted && !in.Force:
		out, err = scanArtifact(tx.QueryRow(ctx, `UPDATE work_artifacts SET source_mtime = $2,
				sandbox_id = coalesce(nullif($3, ''), sandbox_id),
				restore_skipped_at = CASE WHEN $3 = '' THEN restore_skipped_at END
			WHERE id = $1 RETURNING `+artifactCols, cur.ID, in.Mtime, store.CleanText(in.SandboxID)))
		if err != nil {
			return nil, "", err
		}
	default:
		// A new version is only in the sandbox it was read from or written to.
		out, err = scanArtifact(tx.QueryRow(ctx, `UPDATE work_artifacts SET version = version + 1, size_bytes = $2, sha256 = $3,
				run_id = $4, source_mtime = $5, mime_type = $6, deleted_at = NULL, updated_at = now(),
				sandbox_id = nullif($7, ''), restore_skipped_at = NULL
			WHERE id = $1 RETURNING `+artifactCols, cur.ID, in.SizeBytes, in.SHA256, runID, in.Mtime, in.MimeType, store.CleanText(in.SandboxID)))
		if err != nil {
			return nil, "", err
		}
		change = store.ChangeUpdated
	}
	if change != "" {
		// A new reference to a queued blob refreshes its last reference (blob GC grace period).
		if _, err := tx.Exec(ctx, `WITH referenced AS (UPDATE work_blob_gc SET last_ref_at = now() WHERE key = $3)
			INSERT INTO work_artifact_versions (artifact_id, version, sha256, size_bytes, run_id)
			VALUES ($1, $2, $3, $4, $5)`, out.ID, out.Version, out.SHA256, out.SizeBytes, runID); err != nil {
			return nil, "", err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, "", err
	}
	return out, change, nil
}

// MarkArtifactDeleted implements ArtifactStore.
func (s *PG) MarkArtifactDeleted(ctx context.Context, artifactID, runID, sandboxID string, unchangedSince time.Time) (*store.Artifact, error) {
	if !validUUID(artifactID) {
		return nil, store.ErrNotFound
	}
	a, err := scanArtifact(s.pool.QueryRow(ctx, `UPDATE work_artifacts SET deleted_at = now(),
			run_id = coalesce($2, run_id), updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL AND sandbox_id = $3 AND updated_at < $4 RETURNING `+artifactCols,
		artifactID, nullUUID(runID), sandboxID, unchangedSince))
	if errors.Is(err, store.ErrNotFound) {
		return nil, store.ErrConflict
	}
	return a, err
}

// MarkArtifactsMaterialized implements ArtifactStore.
func (s *PG) MarkArtifactsMaterialized(ctx context.Context, sandboxID string, artifactIDs []string) error {
	ids := validUUIDs(artifactIDs)
	if len(ids) == 0 || sandboxID == "" {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE work_artifacts SET sandbox_id = $1, restore_skipped_at = NULL WHERE id = ANY($2::uuid[])`,
		store.CleanText(sandboxID), ids)
	return err
}

// MarkArtifactsRestoreSkipped implements ArtifactStore.
func (s *PG) MarkArtifactsRestoreSkipped(ctx context.Context, artifactIDs []string) error {
	ids := validUUIDs(artifactIDs)
	if len(ids) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE work_artifacts SET restore_skipped_at = coalesce(restore_skipped_at, now()) WHERE id = ANY($1::uuid[])`, ids)
	return err
}

func validUUIDs(in []string) []string {
	out := make([]string, 0, len(in))
	for _, id := range in {
		if validUUID(id) {
			out = append(out, id)
		}
	}
	return out
}

// Clock implements ArtifactStore.
func (s *PG) Clock(ctx context.Context) (time.Time, error) {
	var t time.Time
	err := s.pool.QueryRow(ctx, `SELECT now()`).Scan(&t)
	return t, err
}

// ListArtifacts implements ArtifactStore.
func (s *PG) ListArtifacts(ctx context.Context, threadID string) ([]*store.Artifact, error) {
	if !validUUID(threadID) {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+artifactCols+` FROM work_artifacts WHERE thread_id = $1
		ORDER BY deleted_at IS NOT NULL, updated_at DESC, path`, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetArtifact implements ArtifactStore.
func (s *PG) GetArtifact(ctx context.Context, id string) (*store.Artifact, error) {
	if !validUUID(id) {
		return nil, store.ErrNotFound
	}
	return scanArtifact(s.pool.QueryRow(ctx, `SELECT `+artifactCols+` FROM work_artifacts a
		WHERE id = $1 AND EXISTS (SELECT 1 FROM work_threads t WHERE t.id = a.thread_id AND t.deleted_at IS NULL)`, id))
}

// ListArtifactVersions implements ArtifactStore (newest first).
func (s *PG) ListArtifactVersions(ctx context.Context, artifactID string) ([]store.ArtifactVersion, error) {
	if !validUUID(artifactID) {
		return nil, store.ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT version, size_bytes, sha256, coalesce(run_id::text, ''), created_at
		FROM work_artifact_versions WHERE artifact_id = $1 ORDER BY version DESC`, artifactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.ArtifactVersion
	for rows.Next() {
		var v store.ArtifactVersion
		if err := rows.Scan(&v.Version, &v.SizeBytes, &v.SHA256, &v.RunID, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetArtifactVersion implements ArtifactStore.
func (s *PG) GetArtifactVersion(ctx context.Context, artifactID string, version int) (*store.ArtifactVersion, error) {
	if !validUUID(artifactID) {
		return nil, store.ErrNotFound
	}
	var v store.ArtifactVersion
	err := s.pool.QueryRow(ctx, `SELECT version, size_bytes, sha256, coalesce(run_id::text, ''), created_at
		FROM work_artifact_versions WHERE artifact_id = $1 AND version = $2`, artifactID, version).
		Scan(&v.Version, &v.SizeBytes, &v.SHA256, &v.RunID, &v.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// PutBlob implements BlobStore.
func (s *PG) PutBlob(ctx context.Context, key string, data []byte) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO work_blobs (key, bytes, size) VALUES ($1, $2, $3) ON CONFLICT (key) DO NOTHING`,
		key, data, len(data))
	return err
}

// GetBlob implements BlobStore.
func (s *PG) GetBlob(ctx context.Context, key string) ([]byte, error) {
	var b []byte
	err := s.pool.QueryRow(ctx, `SELECT bytes FROM work_blobs WHERE key = $1`, key).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return b, err
}

// DeleteBlob implements BlobStore.
func (s *PG) DeleteBlob(ctx context.Context, key string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM work_blobs WHERE key = $1`, key)
	return err
}

// HasBlob implements BlobStore.
func (s *PG) HasBlob(ctx context.Context, key string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM work_blobs WHERE key = $1)`, key).Scan(&ok)
	return ok, err
}

const skillCols = `id::text, user_login, name, display_name, description, version, sha256, files, size_bytes, created_at, updated_at`

func scanSkill(row pgx.Row, withPackage bool) (*store.UserSkill, error) {
	var u store.UserSkill
	dest := []any{&u.ID, &u.UserLogin, &u.Name, &u.DisplayName, &u.Description, &u.Version, &u.SHA256, &u.Files, &u.SizeBytes, &u.CreatedAt, &u.UpdatedAt}
	if withPackage {
		dest = append(dest, &u.Package)
	}
	if err := row.Scan(dest...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return &u, nil
}

// UpsertUserSkill implements SkillStore. The upload is also recorded as the
// skill's published version "<n>.0.0" (work_skill_versions, migration 0008).
func (s *PG) UpsertUserSkill(ctx context.Context, u store.UserSkill) (*store.UserSkill, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	out, err := scanSkill(tx.QueryRow(ctx, `INSERT INTO work_skills (id, user_login, name, display_name, description, version, sha256, package, files, size_bytes)
		VALUES ($1, $2, $3, $9, $4, 1, $5, $6, $7, $8)
		ON CONFLICT (user_login, name) DO UPDATE SET description = EXCLUDED.description, version = work_skills.version + 1,
			sha256 = EXCLUDED.sha256, package = EXCLUDED.package, files = EXCLUDED.files, size_bytes = EXCLUDED.size_bytes,
			display_name = EXCLUDED.display_name, updated_at = now()
		RETURNING `+skillCols, uuid.NewString(), u.UserLogin, store.CleanText(u.Name), store.CleanText(u.Description), u.SHA256, u.Package,
		u.Files, u.SizeBytes, store.CleanText(displayName(u))), false)
	if err != nil {
		return nil, err
	}
	v := store.SkillVersion{SkillID: out.ID, Version: legacyVersionLabel(out.Version), Status: store.SkillPublished, SourceType: store.SkillSourceUpload,
		Name: displayName(u), Description: u.Description, SHA256: u.SHA256, Package: u.Package, Files: u.Files, SizeBytes: u.SizeBytes,
		CreatedBy: u.UserLogin}
	if _, err := tx.Exec(ctx, `SAVEPOINT version_label`); err != nil {
		return nil, err
	}
	if err := insertVersion(ctx, tx, &v); errors.Is(err, store.ErrConflict) {
		// The label is taken by an editor version: keep it unique.
		if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT version_label`); err != nil {
			return nil, err
		}
		v.Version += "-" + u.SHA256[:min(8, len(u.SHA256))]
		if err := insertVersion(ctx, tx, &v); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE work_skills SET published_version_id = $2, latest_version_id = $2 WHERE id = $1`, out.ID, v.ID); err != nil {
		return nil, err
	}
	if err := syncPublished(ctx, tx, out.ID, false); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

func displayName(u store.UserSkill) string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Name
}

// legacyVersionLabel is the version label of the n-th upload of a skill name.
func legacyVersionLabel(n int) string { return strconv.Itoa(n) + ".0.0" }

// ListUserSkills implements SkillStore.
func (s *PG) ListUserSkills(ctx context.Context, login string) ([]*store.UserSkill, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+skillCols+` FROM work_skills WHERE user_login = $1 ORDER BY name`, login)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.UserSkill
	for rows.Next() {
		u, err := scanSkill(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// GetUserSkill implements SkillStore.
func (s *PG) GetUserSkill(ctx context.Context, login, id string) (*store.UserSkill, error) {
	if !validUUID(id) {
		return nil, store.ErrNotFound
	}
	return scanSkill(s.pool.QueryRow(ctx, `SELECT `+skillCols+`, package FROM work_skills WHERE id = $1 AND user_login = $2`, id, login), true)
}

// DeleteUserSkill implements SkillStore.
func (s *PG) DeleteUserSkill(ctx context.Context, login, id string) error {
	if !validUUID(id) {
		return store.ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `DELETE FROM work_skills WHERE id = $1 AND user_login = $2`, id, login)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	if _, err := tx.Exec(ctx, `DELETE FROM work_skill_settings WHERE user_login = $1 AND skill_id = $2`, login, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SkillSettings implements SkillStore.
func (s *PG) SkillSettings(ctx context.Context, login string) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT skill_id, enabled FROM work_skill_settings WHERE user_login = $1`, login)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		var enabled bool
		if err := rows.Scan(&id, &enabled); err != nil {
			return nil, err
		}
		out[id] = enabled
	}
	return out, rows.Err()
}

// SetSkillEnabled implements SkillStore.
func (s *PG) SetSkillEnabled(ctx context.Context, login, skillID string, enabled bool) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO work_skill_settings (user_login, skill_id, enabled) VALUES ($1, $2, $3)
		ON CONFLICT (user_login, skill_id) DO UPDATE SET enabled = EXCLUDED.enabled`, login, skillID, enabled)
	return err
}

// AppendRunArtifacts implements ArtifactStore.
func (s *PG) AppendRunArtifacts(ctx context.Context, runID string, items []any) error {
	if !validUUID(runID) || len(items) == 0 {
		return nil
	}
	raw, err := json.Marshal(store.CleanValue(items))
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE work_messages m SET metadata = jsonb_set(coalesce(m.metadata, '{}'::jsonb), '{artifacts}',
			coalesce(m.metadata->'artifacts', '[]'::jsonb) || $2::jsonb)
		FROM (SELECT thread_id, id FROM work_messages WHERE run_id = $1 AND role = 'assistant' AND superseded_at IS NULL
			ORDER BY position DESC LIMIT 1) last
		WHERE m.thread_id = last.thread_id AND m.id = last.id`, runID, raw)
	if err != nil || tag.RowsAffected() > 0 {
		return err
	}
	// No assistant message (e.g. the model failed before answering): an empty
	// assistant message of the run carries the end-of-run changes.
	_, err = s.pool.Exec(ctx, `INSERT INTO work_messages (id, thread_id, run_id, role, content, metadata)
		SELECT $2, r.thread_id, r.id, 'assistant', '', jsonb_build_object('artifacts', $3::jsonb)
		FROM work_runs r JOIN work_threads t ON t.id = r.thread_id WHERE r.id = $1 AND t.deleted_at IS NULL`, runID, uuid.NewString(), raw)
	return err
}

// AppendToolVerification implements HarnessStore.
func (s *PG) AppendToolVerification(ctx context.Context, threadID, toolCallID string, items []any) error {
	if !validUUID(threadID) || toolCallID == "" || len(items) == 0 {
		return nil
	}
	raw, err := json.Marshal(store.CleanValue(items))
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `UPDATE work_messages m SET metadata = jsonb_set(coalesce(m.metadata, '{}'::jsonb), '{verification}',
			coalesce(m.metadata->'verification', '[]'::jsonb) || $3::jsonb)
		FROM (SELECT thread_id, id FROM work_messages WHERE thread_id = $1 AND tool_call_id = $2 AND role = 'tool' AND superseded_at IS NULL
			ORDER BY position DESC LIMIT 1) last
		WHERE m.thread_id = last.thread_id AND m.id = last.id`, threadID, toolCallID, raw)
	return err
}

// QueueBlob implements ArtifactStore.
func (s *PG) QueueBlob(ctx context.Context, key string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO work_blob_gc (key, last_ref_at) VALUES ($1, now())
		ON CONFLICT (key) DO UPDATE SET last_ref_at = now()`, key)
	return err
}
