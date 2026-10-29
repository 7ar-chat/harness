package teamstore

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GhaziBenDahmane/harness/store"
)

// PG is a TeamStore on Postgres (tables of migration 0450, applied by pgstore).
type PG struct{ pool *pgxpool.Pool }

// NewPG returns a TeamStore on pool.
func NewPG(pool *pgxpool.Pool) *PG { return &PG{pool: pool} }

var _ store.TeamStore = (*PG)(nil)

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ErrNotFound
	}
	return err
}

func (s *PG) UpsertUser(ctx context.Context, u store.User) (*store.User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var out store.User
	err = tx.QueryRow(ctx, `INSERT INTO work_users (login, email, name, avatar_url) VALUES ($1, $2, $3, $4)
		ON CONFLICT (login) DO UPDATE SET email = $2, name = $3, avatar_url = $4, last_seen = now()
		RETURNING login, email, name, avatar_url, created_at, last_seen`,
		u.Login, norm(u.Email), u.Name, u.AvatarURL).Scan(&out.Login, &out.Email, &out.Name, &out.AvatarURL, &out.CreatedAt, &out.LastSeen)
	if err != nil {
		return nil, err
	}
	if out.Email != "" {
		if _, err := tx.Exec(ctx, `WITH claimed AS (DELETE FROM work_project_invites WHERE email = $1 RETURNING project_id, role)
			INSERT INTO work_project_members (project_id, login, role) SELECT project_id, $2, role FROM claimed
			ON CONFLICT (project_id, login) DO UPDATE SET role = EXCLUDED.role`, out.Email, out.Login); err != nil {
			return nil, err
		}
	}
	return &out, tx.Commit(ctx)
}

const userCols = `login, email, name, avatar_url, created_at, last_seen`

func scanUser(r pgx.Row) (*store.User, error) {
	var u store.User
	if err := r.Scan(&u.Login, &u.Email, &u.Name, &u.AvatarURL, &u.CreatedAt, &u.LastSeen); err != nil {
		return nil, notFound(err)
	}
	return &u, nil
}

func (s *PG) GetUser(ctx context.Context, login string) (*store.User, error) {
	return scanUser(s.pool.QueryRow(ctx, `SELECT `+userCols+` FROM work_users WHERE login = $1`, login))
}

func (s *PG) ListUsers(ctx context.Context) ([]store.User, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+userCols+` FROM work_users ORDER BY login`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

func (s *PG) UserPrefs(ctx context.Context, login string) (map[string]any, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT prefs FROM work_users WHERE login = $1`, login).Scan(&raw)
	out := map[string]any{}
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out, nil
}

func (s *PG) SetUserPrefs(ctx context.Context, login string, prefs map[string]any) error {
	b, _ := json.Marshal(prefs)
	_, err := s.pool.Exec(ctx, `UPDATE work_users SET prefs = $2 WHERE login = $1`, login, b)
	return err
}

func (s *PG) GetUserByEmail(ctx context.Context, email string) (*store.User, error) {
	return scanUser(s.pool.QueryRow(ctx, `SELECT `+userCols+` FROM work_users WHERE email = $1 AND email <> '' LIMIT 1`, norm(email)))
}

func (s *PG) GetUsers(ctx context.Context, logins []string) (map[string]*store.User, error) {
	out := map[string]*store.User{}
	if len(logins) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+userCols+` FROM work_users WHERE login = ANY($1)`, logins)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out[u.Login] = u
	}
	return out, rows.Err()
}

const projectCols = `id, name, description, repo_url, default_branch, github_installation, created_by, created_at, updated_at`

func scanProject(r pgx.Row) (*store.Project, error) {
	var p store.Project
	if err := r.Scan(&p.ID, &p.Name, &p.Description, &p.RepoURL, &p.DefaultBranch, &p.GitHubInstallation, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, notFound(err)
	}
	return &p, nil
}

func (s *PG) CreateProject(ctx context.Context, p store.Project, owner string) (*store.Project, error) {
	if p.ID == "" {
		p.ID = uuid.NewString()
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	out, err := scanProject(tx.QueryRow(ctx, `INSERT INTO work_projects (id, name, description, repo_url, default_branch, github_installation, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING `+projectCols,
		p.ID, p.Name, p.Description, p.RepoURL, p.DefaultBranch, p.GitHubInstallation, owner))
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO work_project_members (project_id, login, role) VALUES ($1, $2, 'owner')`, p.ID, owner); err != nil {
		return nil, err
	}
	return out, tx.Commit(ctx)
}

func (s *PG) GetProject(ctx context.Context, id string) (*store.Project, error) {
	return scanProject(s.pool.QueryRow(ctx, `SELECT `+projectCols+` FROM work_projects WHERE id = $1`, id))
}

func (s *PG) UpdateProject(ctx context.Context, id string, patch store.ProjectPatch) (*store.Project, error) {
	return scanProject(s.pool.QueryRow(ctx, `UPDATE work_projects SET
			name = coalesce($2, name), description = coalesce($3, description), repo_url = coalesce($4, repo_url),
			default_branch = coalesce($5, default_branch), github_installation = coalesce($6, github_installation), updated_at = now()
		WHERE id = $1 RETURNING `+projectCols,
		id, patch.Name, patch.Description, patch.RepoURL, patch.DefaultBranch, patch.GitHubInstallation))
}

func (s *PG) DeleteProject(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM work_projects WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *PG) ListProjects(ctx context.Context, login string) ([]store.ProjectRole, error) {
	rows, err := s.pool.Query(ctx, `SELECT p.id, p.name, p.description, p.repo_url, p.default_branch, p.github_installation,
			p.created_by, p.created_at, p.updated_at, m.role
		FROM work_projects p JOIN work_project_members m ON m.project_id = p.id WHERE m.login = $1 ORDER BY p.name`, login)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.ProjectRole
	for rows.Next() {
		var pr store.ProjectRole
		if err := rows.Scan(&pr.ID, &pr.Name, &pr.Description, &pr.RepoURL, &pr.DefaultBranch, &pr.GitHubInstallation,
			&pr.CreatedBy, &pr.CreatedAt, &pr.UpdatedAt, &pr.Role); err != nil {
			return nil, err
		}
		out = append(out, pr)
	}
	return out, rows.Err()
}

func (s *PG) MemberRole(ctx context.Context, projectID, login string) (string, error) {
	var role string
	err := s.pool.QueryRow(ctx, `SELECT role FROM work_project_members WHERE project_id = $1 AND login = $2`, projectID, login).Scan(&role)
	return role, notFound(err)
}

func (s *PG) ListMembers(ctx context.Context, projectID string) ([]store.Member, error) {
	rows, err := s.pool.Query(ctx, `SELECT project_id, login, role, added_at FROM work_project_members WHERE project_id = $1 ORDER BY added_at`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Member
	for rows.Next() {
		var m store.Member
		if err := rows.Scan(&m.ProjectID, &m.Login, &m.Role, &m.AddedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// lastOwner locks the project's owners and reports whether login is the only one.
func lastOwner(ctx context.Context, tx pgx.Tx, projectID, login string) (bool, error) {
	rows, err := tx.Query(ctx, `SELECT login FROM work_project_members WHERE project_id = $1 AND role = 'owner' FOR UPDATE`, projectID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	var owners []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return false, err
		}
		owners = append(owners, l)
	}
	return len(owners) == 1 && owners[0] == login, rows.Err()
}

func (s *PG) SetMember(ctx context.Context, projectID, login, role string) error {
	if !store.ValidRole(role) {
		return store.ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if role != store.RoleOwner {
		if last, err := lastOwner(ctx, tx, projectID, login); err != nil {
			return err
		} else if last {
			return store.ErrLastOwner
		}
	}
	if _, err := s.projectExists(ctx, tx, projectID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO work_project_members (project_id, login, role) VALUES ($1, $2, $3)
		ON CONFLICT (project_id, login) DO UPDATE SET role = $3`, projectID, login, role); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PG) projectExists(ctx context.Context, tx pgx.Tx, id string) (bool, error) {
	var one int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM work_projects WHERE id = $1`, id).Scan(&one); err != nil {
		return false, notFound(err)
	}
	return true, nil
}

func (s *PG) RemoveMember(ctx context.Context, projectID, login string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if last, err := lastOwner(ctx, tx, projectID, login); err != nil {
		return err
	} else if last {
		return store.ErrLastOwner
	}
	tag, err := tx.Exec(ctx, `DELETE FROM work_project_members WHERE project_id = $1 AND login = $2`, projectID, login)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return tx.Commit(ctx)
}

func (s *PG) CreateInvite(ctx context.Context, in store.Invite) (*store.Invite, error) {
	if !store.ValidRole(in.Role) {
		return nil, store.ErrConflict
	}
	in.Email = norm(in.Email)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := s.projectExists(ctx, tx, in.ProjectID); err != nil {
		return nil, err
	}
	var login string
	err = tx.QueryRow(ctx, `SELECT login FROM work_users WHERE email = $1 LIMIT 1`, in.Email).Scan(&login)
	switch {
	case err == nil:
		if _, err := tx.Exec(ctx, `INSERT INTO work_project_members (project_id, login, role) VALUES ($1, $2, $3)
			ON CONFLICT (project_id, login) DO UPDATE SET role = $3`, in.ProjectID, login, in.Role); err != nil {
			return nil, err
		}
		return nil, tx.Commit(ctx)
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, err
	}
	err = tx.QueryRow(ctx, `INSERT INTO work_project_invites (project_id, email, role, invited_by) VALUES ($1, $2, $3, $4)
		ON CONFLICT (project_id, email) DO UPDATE SET role = $3, invited_by = $4, created_at = now()
		RETURNING created_at`, in.ProjectID, in.Email, in.Role, in.InvitedBy).Scan(&in.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &in, tx.Commit(ctx)
}

func (s *PG) ListInvites(ctx context.Context, projectID string) ([]store.Invite, error) {
	rows, err := s.pool.Query(ctx, `SELECT project_id, email, role, invited_by, created_at FROM work_project_invites WHERE project_id = $1 ORDER BY created_at`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Invite
	for rows.Next() {
		var iv store.Invite
		if err := rows.Scan(&iv.ProjectID, &iv.Email, &iv.Role, &iv.InvitedBy, &iv.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, iv)
	}
	return out, rows.Err()
}

func (s *PG) DeleteInvite(ctx context.Context, projectID, email string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM work_project_invites WHERE project_id = $1 AND email = $2`, projectID, norm(email))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *PG) SetThreadMeta(ctx context.Context, t store.ThreadMeta) error {
	if t.Kind == "" {
		t.Kind = store.ThreadChat
	}
	var project *string
	if t.ProjectID != "" {
		project = &t.ProjectID
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO work_thread_meta (thread_id, project_id, owner, created_by, kind) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (thread_id) DO UPDATE SET project_id = $2, owner = $3, created_by = $4, kind = $5`,
		t.ThreadID, project, t.Owner, t.CreatedBy, t.Kind)
	return err
}

const metaCols = `thread_id, coalesce(project_id, ''), owner, created_by, kind, created_at`

func scanMeta(r pgx.Row) (*store.ThreadMeta, error) {
	var t store.ThreadMeta
	if err := r.Scan(&t.ThreadID, &t.ProjectID, &t.Owner, &t.CreatedBy, &t.Kind, &t.CreatedAt); err != nil {
		return nil, notFound(err)
	}
	return &t, nil
}

func (s *PG) GetThreadMeta(ctx context.Context, threadID string) (*store.ThreadMeta, error) {
	return scanMeta(s.pool.QueryRow(ctx, `SELECT `+metaCols+` FROM work_thread_meta WHERE thread_id = $1`, threadID))
}

func (s *PG) ListProjectThreads(ctx context.Context, projectID string) ([]store.ThreadMeta, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+metaCols+` FROM work_thread_meta WHERE project_id = $1 ORDER BY created_at DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.ThreadMeta
	for rows.Next() {
		t, err := scanMeta(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func (s *PG) SupervisorThread(ctx context.Context, login string) (*store.ThreadMeta, error) {
	return scanMeta(s.pool.QueryRow(ctx, `SELECT `+metaCols+` FROM work_thread_meta WHERE owner = $1 AND kind = 'supervisor' LIMIT 1`, login))
}

func (s *PG) DeleteThreadMeta(ctx context.Context, threadID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM work_thread_meta WHERE thread_id = $1`, threadID)
	return err
}

func (s *PG) Audit(ctx context.Context, e store.AuditEntry) error {
	var detail []byte
	if len(e.Detail) > 0 {
		detail, _ = json.Marshal(e.Detail)
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO work_audit (project_id, thread_id, actor, on_behalf_of, action, detail) VALUES ($1, $2, $3, $4, $5, $6)`,
		e.ProjectID, e.ThreadID, e.Actor, e.OnBehalfOf, e.Action, detail)
	return err
}

func (s *PG) ListAudit(ctx context.Context, projectID string, limit int) ([]store.AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT id, project_id, thread_id, actor, on_behalf_of, action, detail, at
		FROM work_audit WHERE project_id = $1 ORDER BY id DESC LIMIT $2`, projectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.AuditEntry
	for rows.Next() {
		var e store.AuditEntry
		var detail []byte
		var at time.Time
		if err := rows.Scan(&e.ID, &e.ProjectID, &e.ThreadID, &e.Actor, &e.OnBehalfOf, &e.Action, &detail, &at); err != nil {
			return nil, err
		}
		e.At = at
		if len(detail) > 0 {
			_ = json.Unmarshal(detail, &e.Detail)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
