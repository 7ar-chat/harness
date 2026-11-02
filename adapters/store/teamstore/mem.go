// Package teamstore implements store.TeamStore: Mem (in memory, optionally
// persisted to a JSON file, for local and single-process hosts) and PG.
package teamstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/7ar-chat/harness/core/store"
)

// Mem is a TeamStore in memory. With a path, every change is written to it
// (atomically, by rename) and the file is read back by Open.
type Mem struct {
	mu   sync.Mutex
	path string
	d    data
}

type avatar struct {
	MIME string `json:"mime"`
	Data []byte `json:"data"`
}

type data struct {
	Users    map[string]*store.User                `json:"users"`
	Prefs    map[string]map[string]any             `json:"prefs"`
	Projects map[string]*store.Project             `json:"projects"`
	Members  map[string][]store.Member             `json:"members"` // by project
	Invites  map[string][]store.Invite             `json:"invites"` // by project
	Threads  map[string]*store.ThreadMeta          `json:"threads"`
	Audit    []store.AuditEntry                    `json:"audit"`
	NextAud  int64                                 `json:"nextAudit"`
	Avatars  map[string]avatar                     `json:"avatars"`
	Conns    map[string]map[string]store.Connector `json:"connectors"`
	Seen     map[string]map[string]time.Time       `json:"seen"` // login → thread id → last seen
}

// NewMem returns an empty in-memory store that is never persisted.
func NewMem() *Mem {
	m := &Mem{}
	m.reset()
	return m
}

// Open returns a Mem persisted to path ("" for none), loading it if it exists.
func Open(path string) (*Mem, error) {
	m := &Mem{path: path}
	m.reset()
	if path == "" {
		return m, nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &m.d); err != nil {
		return nil, err
	}
	m.fill()
	return m, nil
}

func (m *Mem) reset() { m.d = data{}; m.fill() }

func (m *Mem) fill() {
	if m.d.Users == nil {
		m.d.Users = map[string]*store.User{}
	}
	if m.d.Avatars == nil {
		m.d.Avatars = map[string]avatar{}
	}
	if m.d.Seen == nil {
		m.d.Seen = map[string]map[string]time.Time{}
	}
	if m.d.Conns == nil {
		m.d.Conns = map[string]map[string]store.Connector{}
	}
	if m.d.Projects == nil {
		m.d.Projects = map[string]*store.Project{}
	}
	if m.d.Members == nil {
		m.d.Members = map[string][]store.Member{}
	}
	if m.d.Invites == nil {
		m.d.Invites = map[string][]store.Invite{}
	}
	if m.d.Threads == nil {
		m.d.Threads = map[string]*store.ThreadMeta{}
	}
}

// save writes the state; the caller holds mu.
func (m *Mem) save() error {
	if m.path == "" {
		return nil
	}
	b, err := json.Marshal(&m.d)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}

func norm(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

func (m *Mem) UpsertUser(_ context.Context, u store.User) (*store.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	cur := m.d.Users[u.Login]
	if cur == nil {
		cur = &store.User{Login: u.Login, CreatedAt: now}
		m.d.Users[u.Login] = cur
	}
	cur.Email, cur.Name, cur.AvatarURL, cur.LastSeen = norm(u.Email), u.Name, u.AvatarURL, now
	if cur.Email != "" {
		for pid, list := range m.d.Invites {
			var keep []store.Invite
			for _, iv := range list {
				if iv.Email == cur.Email {
					m.setMember(pid, cur.Login, iv.Role)
					continue
				}
				keep = append(keep, iv)
			}
			m.d.Invites[pid] = keep
		}
	}
	out := *cur
	return &out, m.save()
}

func (m *Mem) ListUsers(_ context.Context) ([]store.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.User, 0, len(m.d.Users))
	for _, u := range m.d.Users {
		out = append(out, *u)
	}
	return out, nil
}

func (m *Mem) SetDisplayName(_ context.Context, login, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u := m.d.Users[login]
	if u == nil {
		return store.ErrNotFound
	}
	u.DisplayName = name
	return m.save()
}

func (m *Mem) SetAvatar(_ context.Context, login, mime string, data []byte) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u := m.d.Users[login]
	if u == nil {
		return "", store.ErrNotFound
	}
	if len(data) == 0 {
		delete(m.d.Avatars, login)
		u.AvatarVersion = ""
		return "", m.save()
	}
	sum := sha256.Sum256(data)
	u.AvatarVersion = hex.EncodeToString(sum[:4])
	m.d.Avatars[login] = avatar{MIME: mime, Data: append([]byte(nil), data...)}
	return u.AvatarVersion, m.save()
}

func (m *Mem) GetAvatar(_ context.Context, login string) (string, []byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.d.Avatars[login]
	if !ok {
		return "", nil, store.ErrNotFound
	}
	return a.MIME, append([]byte(nil), a.Data...), nil
}

func (m *Mem) SetConnector(_ context.Context, c store.Connector) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.d.Users[c.Login] == nil {
		return store.ErrNotFound
	}
	// A provider account belongs to one user.
	for login, byKind := range m.d.Conns {
		if old, ok := byKind[c.Kind]; ok && old.Account == c.Account && login != c.Login {
			return store.ErrConflict
		}
	}
	if m.d.Conns[c.Login] == nil {
		m.d.Conns[c.Login] = map[string]store.Connector{}
	}
	c.CreatedAt = time.Now().UTC()
	m.d.Conns[c.Login][c.Kind] = c
	return m.save()
}

func (m *Mem) GetConnector(_ context.Context, login, kind string) (*store.Connector, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.d.Conns[login][kind]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &c, nil
}

func (m *Mem) ListConnectors(_ context.Context, login string) ([]store.Connector, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.Connector
	for _, c := range m.d.Conns[login] {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out, nil
}

func (m *Mem) DeleteConnector(_ context.Context, login, kind string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.d.Conns[login][kind]; !ok {
		return store.ErrNotFound
	}
	delete(m.d.Conns[login], kind)
	return m.save()
}

func (m *Mem) FindConnector(_ context.Context, kind, account string) (*store.Connector, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, byKind := range m.d.Conns {
		if c, ok := byKind[kind]; ok && c.Account == account {
			return &c, nil
		}
	}
	return nil, store.ErrNotFound
}

func (m *Mem) UserPrefs(_ context.Context, login string) (map[string]any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]any{}
	for k, v := range m.d.Prefs[login] {
		out[k] = v
	}
	return out, nil
}

func (m *Mem) SetUserPrefs(_ context.Context, login string, prefs map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.d.Prefs == nil {
		m.d.Prefs = map[string]map[string]any{}
	}
	cp := make(map[string]any, len(prefs))
	for k, v := range prefs {
		cp[k] = v
	}
	m.d.Prefs[login] = cp
	return m.save()
}

func (m *Mem) SeenThreads(_ context.Context, login string) (map[string]time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]time.Time, len(m.d.Seen[login]))
	for id, at := range m.d.Seen[login] {
		out[id] = at
	}
	return out, nil
}

func (m *Mem) SetThreadSeen(_ context.Context, login, threadID string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	at = at.UTC()
	if cur, ok := m.d.Seen[login][threadID]; ok && !at.After(cur) {
		return nil
	}
	if m.d.Seen[login] == nil {
		m.d.Seen[login] = map[string]time.Time{}
	}
	m.d.Seen[login][threadID] = at
	return m.save()
}

func (m *Mem) GetUser(_ context.Context, login string) (*store.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u := m.d.Users[login]
	if u == nil {
		return nil, store.ErrNotFound
	}
	out := *u
	return &out, nil
}

func (m *Mem) GetUsers(_ context.Context, logins []string) (map[string]*store.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]*store.User{}
	for _, l := range logins {
		if u := m.d.Users[l]; u != nil {
			c := *u
			out[l] = &c
		}
	}
	return out, nil
}

func (m *Mem) GetUserByEmail(_ context.Context, email string) (*store.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	email = norm(email)
	for _, u := range m.d.Users {
		if u.Email == email && email != "" {
			c := *u
			return &c, nil
		}
	}
	return nil, store.ErrNotFound
}

func (m *Mem) CreateProject(_ context.Context, p store.Project, owner string) (*store.Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.ID == "" {
		p.ID = uuid.NewString()
	}
	if _, dup := m.d.Projects[p.ID]; dup {
		return nil, store.ErrConflict
	}
	now := time.Now().UTC()
	p.CreatedBy, p.CreatedAt, p.UpdatedAt = owner, now, now
	m.d.Projects[p.ID] = &p
	m.setMember(p.ID, owner, store.RoleOwner)
	out := p
	return &out, m.save()
}

func (m *Mem) GetProject(_ context.Context, id string) (*store.Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.d.Projects[id]
	if p == nil {
		return nil, store.ErrNotFound
	}
	out := *p
	return &out, nil
}

func (m *Mem) UpdateProject(_ context.Context, id string, patch store.ProjectPatch) (*store.Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.d.Projects[id]
	if p == nil {
		return nil, store.ErrNotFound
	}
	if patch.Name != nil {
		p.Name = *patch.Name
	}
	if patch.Description != nil {
		p.Description = *patch.Description
	}
	if patch.RepoURL != nil {
		p.RepoURL = *patch.RepoURL
	}
	if patch.DefaultBranch != nil {
		p.DefaultBranch = *patch.DefaultBranch
	}
	if patch.GitHubInstallation != nil {
		p.GitHubInstallation = *patch.GitHubInstallation
	}
	p.UpdatedAt = time.Now().UTC()
	out := *p
	return &out, m.save()
}

func (m *Mem) DeleteProject(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.d.Projects[id] == nil {
		return store.ErrNotFound
	}
	delete(m.d.Projects, id)
	delete(m.d.Members, id)
	delete(m.d.Invites, id)
	for tid, t := range m.d.Threads {
		if t.ProjectID == id {
			delete(m.d.Threads, tid)
			for _, seen := range m.d.Seen {
				delete(seen, tid)
			}
		}
	}
	return m.save()
}

func (m *Mem) ListProjects(_ context.Context, login string) ([]store.ProjectRole, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.ProjectRole
	for pid, list := range m.d.Members {
		for _, mb := range list {
			if mb.Login == login && m.d.Projects[pid] != nil {
				out = append(out, store.ProjectRole{Project: *m.d.Projects[pid], Role: mb.Role})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *Mem) MemberRole(_ context.Context, projectID, login string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mb := range m.d.Members[projectID] {
		if mb.Login == login {
			return mb.Role, nil
		}
	}
	return "", store.ErrNotFound
}

func (m *Mem) ListMembers(_ context.Context, projectID string) ([]store.Member, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]store.Member(nil), m.d.Members[projectID]...), nil
}

// setMember adds or updates; the caller holds mu.
func (m *Mem) setMember(pid, login, role string) {
	list := m.d.Members[pid]
	for i := range list {
		if list[i].Login == login {
			list[i].Role = role
			return
		}
	}
	m.d.Members[pid] = append(list, store.Member{ProjectID: pid, Login: login, Role: role, AddedAt: time.Now().UTC()})
}

func (m *Mem) owners(pid string) int {
	n := 0
	for _, mb := range m.d.Members[pid] {
		if mb.Role == store.RoleOwner {
			n++
		}
	}
	return n
}

func (m *Mem) SetMember(_ context.Context, projectID, login, role string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.d.Projects[projectID] == nil {
		return store.ErrNotFound
	}
	if !store.ValidRole(role) {
		return store.ErrConflict
	}
	for _, mb := range m.d.Members[projectID] {
		if mb.Login == login && mb.Role == store.RoleOwner && role != store.RoleOwner && m.owners(projectID) == 1 {
			return store.ErrLastOwner
		}
	}
	m.setMember(projectID, login, role)
	return m.save()
}

func (m *Mem) RemoveMember(_ context.Context, projectID, login string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := m.d.Members[projectID]
	for i, mb := range list {
		if mb.Login != login {
			continue
		}
		if mb.Role == store.RoleOwner && m.owners(projectID) == 1 {
			return store.ErrLastOwner
		}
		m.d.Members[projectID] = append(list[:i:i], list[i+1:]...)
		return m.save()
	}
	return store.ErrNotFound
}

func (m *Mem) CreateInvite(_ context.Context, in store.Invite) (*store.Invite, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.d.Projects[in.ProjectID] == nil {
		return nil, store.ErrNotFound
	}
	if !store.ValidRole(in.Role) {
		return nil, store.ErrConflict
	}
	in.Email = norm(in.Email)
	for _, u := range m.d.Users {
		if u.Email == in.Email {
			m.setMember(in.ProjectID, u.Login, in.Role)
			return nil, m.save()
		}
	}
	in.CreatedAt = time.Now().UTC()
	list := m.d.Invites[in.ProjectID]
	for i := range list {
		if list[i].Email == in.Email {
			list[i] = in
			return &in, m.save()
		}
	}
	m.d.Invites[in.ProjectID] = append(list, in)
	return &in, m.save()
}

func (m *Mem) ListInvites(_ context.Context, projectID string) ([]store.Invite, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]store.Invite(nil), m.d.Invites[projectID]...), nil
}

func (m *Mem) DeleteInvite(_ context.Context, projectID, email string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	email = norm(email)
	list := m.d.Invites[projectID]
	for i, iv := range list {
		if iv.Email == email {
			m.d.Invites[projectID] = append(list[:i:i], list[i+1:]...)
			return m.save()
		}
	}
	return store.ErrNotFound
}

func (m *Mem) SetThreadMeta(_ context.Context, t store.ThreadMeta) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t.Kind == "" {
		t.Kind = store.ThreadChat
	}
	if t.CreatedAt.IsZero() {
		// Linking again keeps the thread's age (Postgres does too).
		if cur := m.d.Threads[t.ThreadID]; cur != nil {
			t.CreatedAt = cur.CreatedAt
		} else {
			t.CreatedAt = time.Now().UTC()
		}
	}
	m.d.Threads[t.ThreadID] = &t
	return m.save()
}

func (m *Mem) GetThreadMeta(_ context.Context, threadID string) (*store.ThreadMeta, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.d.Threads[threadID]
	if t == nil {
		return nil, store.ErrNotFound
	}
	out := *t
	return &out, nil
}

func (m *Mem) ListProjectThreads(_ context.Context, projectID string) ([]store.ThreadMeta, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.ThreadMeta
	for _, t := range m.d.Threads {
		if t.ProjectID == projectID {
			out = append(out, *t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (m *Mem) SupervisorThread(_ context.Context, login string) (*store.ThreadMeta, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.d.Threads {
		if t.Kind == store.ThreadSupervisor && t.Owner == login {
			out := *t
			return &out, nil
		}
	}
	return nil, store.ErrNotFound
}

func (m *Mem) DeleteThreadMeta(_ context.Context, threadID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.d.Threads, threadID)
	for _, seen := range m.d.Seen {
		delete(seen, threadID)
	}
	return m.save()
}

func (m *Mem) Audit(_ context.Context, e store.AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.d.NextAud++
	e.ID, e.At = m.d.NextAud, time.Now().UTC()
	m.d.Audit = append(m.d.Audit, e)
	if len(m.d.Audit) > 5000 {
		m.d.Audit = m.d.Audit[len(m.d.Audit)-5000:]
	}
	return m.save()
}

func (m *Mem) ListAudit(_ context.Context, projectID string, limit int) ([]store.AuditEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 {
		limit = 100
	}
	var out []store.AuditEntry
	for i := len(m.d.Audit) - 1; i >= 0 && len(out) < limit; i-- {
		if m.d.Audit[i].ProjectID == projectID {
			out = append(out, m.d.Audit[i])
		}
	}
	return out, nil
}

var _ store.TeamStore = (*Mem)(nil)
