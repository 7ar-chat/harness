package memstore

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/GhaziBenDahmane/harness/store"
)

// catalog mimics migration 0008: skill versions (unique label per skill),
// global skills (unique name), command results and skill creation sessions.
// Lock order: extras.mu (private skills) before catalog.mu.
type catalog struct {
	mu       sync.Mutex
	versions map[string]*store.SkillVersion // by version id (with package)
	meta     map[string]*store.SkillMeta    // private skill id → bookkeeping
	globals  map[string]*store.GlobalSkill
	commands map[string]store.CommandResult // login|command|key
	sessions map[string]*store.SkillCreationSession
	market   *marketplace // migration 0015 (skills_marketplace.go)
}

func (s *Store) c() *catalog {
	s.catOnce.Do(func() {
		s.cat = &catalog{versions: map[string]*store.SkillVersion{}, meta: map[string]*store.SkillMeta{},
			globals: map[string]*store.GlobalSkill{}, commands: map[string]store.CommandResult{},
			sessions: map[string]*store.SkillCreationSession{}}
	})
	return s.cat
}

func copyVersion(v *store.SkillVersion, withPackage bool) *store.SkillVersion {
	cp := *v
	if withPackage {
		cp.Package = append([]byte(nil), v.Package...)
	} else {
		cp.Package = nil
	}
	return &cp
}

// insertVersionLocked adds v (new id); ErrConflict on a used label.
func (c *catalog) insertVersionLocked(v *store.SkillVersion, now time.Time) error {
	for _, cur := range c.versions {
		if cur.SkillID == v.SkillID && cur.Global == v.Global && cur.Version == v.Version {
			return store.ErrConflict
		}
	}
	v.ID, v.CreatedAt = uuid.NewString(), now
	if v.Status == "" {
		v.Status = store.SkillDraft
	}
	if v.SourceType == "" {
		v.SourceType = store.SkillSourceUpload
	}
	c.versions[v.ID] = copyVersion(v, true)
	return nil
}

// syncPublishedLocked keeps one published version per skill (Work: the skill's
// published_version pointer): publishedID is published, the others are drafts
// (catalog mu held).
func (c *catalog) syncPublishedLocked(skillID string, global bool, publishedID string) {
	for _, v := range c.versions {
		if v.SkillID != skillID || v.Global != global {
			continue
		}
		if v.ID == publishedID {
			v.Status = store.SkillPublished
		} else {
			v.Status = store.SkillDraft
		}
	}
}

// recordUploadVersion records a legacy upload as its published version (extras.mu held).
func (s *Store) recordUploadVersion(u *store.UserSkill) {
	c := s.c()
	c.mu.Lock()
	defer c.mu.Unlock()
	name := u.DisplayName
	if name == "" {
		name = u.Name
	}
	v := &store.SkillVersion{SkillID: u.ID, Version: strconv.Itoa(u.Version) + ".0.0", Status: store.SkillPublished,
		SourceType: store.SkillSourceUpload, Name: name, Description: u.Description, SHA256: u.SHA256,
		Package: u.Package, Files: u.Files, SizeBytes: u.SizeBytes, CreatedBy: u.UserLogin}
	if err := c.insertVersionLocked(v, s.now()); err != nil {
		v.Version += "-" + u.SHA256[:min(8, len(u.SHA256))]
		_ = c.insertVersionLocked(v, s.now())
	}
	m := c.meta[u.ID]
	if m == nil {
		m = &store.SkillMeta{SkillID: u.ID}
		c.meta[u.ID] = m
	}
	m.DisplayName, m.PublishedVersionID, m.LatestVersionID = name, v.ID, v.ID
	c.syncPublishedLocked(u.ID, false, v.ID)
}

// dropVersions removes a private skill's versions (extras.mu held).
func (s *Store) dropVersions(skillID string) {
	c := s.c()
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, v := range c.versions {
		if v.SkillID == skillID && !v.Global {
			delete(c.versions, id)
		}
	}
	delete(c.meta, skillID)
}

// refreshEffectiveLocked copies the effective version into the private skill (both locks held).
func (c *catalog) refreshEffectiveLocked(u *store.UserSkill, now time.Time) {
	m := c.meta[u.ID]
	if m == nil {
		return
	}
	id := m.PublishedVersionID
	if id == "" {
		id = m.LatestVersionID
	}
	v := c.versions[id]
	if v == nil {
		return
	}
	u.Package, u.SHA256, u.Files, u.SizeBytes, u.Description = append([]byte(nil), v.Package...), v.SHA256, v.Files, v.SizeBytes, v.Description
	u.DisplayName, m.DisplayName = v.Name, v.Name
	u.UpdatedAt = now
}

// SkillMetas implements store.SkillCatalogStore.
func (s *Store) SkillMetas(_ context.Context, login string) (map[string]store.SkillMeta, error) {
	x := s.x()
	x.mu.Lock()
	defer x.mu.Unlock()
	c := s.c()
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]store.SkillMeta{}
	for id, u := range x.skills {
		if u.UserLogin != login {
			continue
		}
		if m := c.meta[id]; m != nil {
			out[id] = *m
		} else {
			out[id] = store.SkillMeta{SkillID: id}
		}
	}
	return out, nil
}

// ListSkillVersions implements store.SkillCatalogStore.
func (s *Store) ListSkillVersions(_ context.Context, skillIDs []string) (map[string][]*store.SkillVersion, error) {
	c := s.c()
	c.mu.Lock()
	defer c.mu.Unlock()
	want := map[string]bool{}
	for _, id := range skillIDs {
		want[id] = true
	}
	out := map[string][]*store.SkillVersion{}
	for _, v := range c.versions {
		if want[v.SkillID] {
			out[v.SkillID] = append(out[v.SkillID], copyVersion(v, false))
		}
	}
	for _, list := range out {
		sort.SliceStable(list, func(i, j int) bool {
			if !list[i].CreatedAt.Equal(list[j].CreatedAt) {
				return list[i].CreatedAt.After(list[j].CreatedAt)
			}
			return list[i].ID > list[j].ID
		})
	}
	return out, nil
}

// GetSkillVersion implements store.SkillCatalogStore.
func (s *Store) GetSkillVersion(_ context.Context, versionID string) (*store.SkillVersion, error) {
	c := s.c()
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.versions[versionID]
	if !ok {
		return nil, store.ErrNotFound
	}
	return copyVersion(v, true), nil
}

// AddSkillVersion implements store.SkillCatalogStore.
func (s *Store) AddSkillVersion(_ context.Context, login string, v store.SkillVersion, publish bool) (*store.SkillVersion, error) {
	if badText(v.Name, v.Description, v.Version) {
		return nil, ErrInvalidText
	}
	x := s.x()
	x.mu.Lock()
	defer x.mu.Unlock()
	c := s.c()
	c.mu.Lock()
	defer c.mu.Unlock()
	now := s.now()
	var u *store.UserSkill
	var g *store.GlobalSkill
	if v.Global {
		if g = c.globals[v.SkillID]; g == nil {
			return nil, store.ErrNotFound
		}
	} else if u = x.skills[v.SkillID]; u == nil || u.UserLogin != login {
		return nil, store.ErrNotFound
	}
	v.Status = store.SkillDraft
	if publish {
		v.Status = store.SkillPublished
	}
	if err := c.insertVersionLocked(&v, now); err != nil {
		return nil, err
	}
	if g != nil {
		g.LatestVersionID, g.UpdatedAt = v.ID, now
		if publish {
			g.PublishedVersionID = v.ID
			c.syncPublishedLocked(g.ID, true, v.ID)
		}
	} else {
		m := c.meta[u.ID]
		if m == nil {
			m = &store.SkillMeta{SkillID: u.ID}
			c.meta[u.ID] = m
		}
		m.LatestVersionID = v.ID
		if publish {
			m.PublishedVersionID = v.ID
			c.syncPublishedLocked(u.ID, false, v.ID)
		}
		u.Version++
		c.refreshEffectiveLocked(u, now)
	}
	return copyVersion(&v, false), nil
}

// PublishSkillVersion implements store.SkillCatalogStore.
func (s *Store) PublishSkillVersion(_ context.Context, login, skillID, versionID string, global bool) error {
	x := s.x()
	x.mu.Lock()
	defer x.mu.Unlock()
	c := s.c()
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.versions[versionID]
	if v == nil || v.SkillID != skillID || v.Global != global {
		return store.ErrNotFound
	}
	if global {
		g := c.globals[skillID]
		if g == nil {
			return store.ErrNotFound
		}
		g.PublishedVersionID, g.UpdatedAt = versionID, s.now()
		c.syncPublishedLocked(skillID, true, versionID)
		return nil
	}
	u := x.skills[skillID]
	if u == nil || u.UserLogin != login {
		return store.ErrNotFound
	}
	m := c.meta[skillID]
	if m == nil {
		m = &store.SkillMeta{SkillID: skillID}
		c.meta[skillID] = m
	}
	m.PublishedVersionID = versionID
	c.syncPublishedLocked(skillID, false, versionID)
	c.refreshEffectiveLocked(u, s.now())
	return nil
}

func commandKey(login, command, key string) string { return login + "|" + command + "|" + key }

// CreateSkillDrafts implements store.SkillCatalogStore.
func (s *Store) CreateSkillDrafts(_ context.Context, idem store.Idempotency, drafts []store.NewSkillDraft, response func([]store.DraftItem) []byte) ([]byte, bool, error) {
	x := s.x()
	x.mu.Lock()
	defer x.mu.Unlock()
	c := s.c()
	c.mu.Lock()
	defer c.mu.Unlock()
	k := commandKey(idem.Login, idem.Command, idem.Key)
	if r, ok := c.commands[k]; ok {
		if r.RequestHash != idem.RequestHash {
			return nil, false, store.ErrIdempotencyConflict
		}
		return r.Response, true, nil
	}
	used := map[string]bool{}
	for _, u := range x.skills {
		if u.UserLogin == idem.Login {
			used[u.Name] = true
		}
	}
	for _, d := range drafts {
		if used[d.Name] {
			return nil, false, store.ErrConflict
		}
		if badText(d.Name, d.DisplayName, d.Description) {
			return nil, false, ErrInvalidText
		}
		used[d.Name] = true
	}
	now := s.now()
	items := make([]store.DraftItem, 0, len(drafts))
	for _, d := range drafts {
		v := d.Version
		u := &store.UserSkill{ID: uuid.NewString(), UserLogin: idem.Login, Name: d.Name, DisplayName: d.DisplayName,
			Description: d.Description, Version: 1, SHA256: v.SHA256, Package: append([]byte(nil), v.Package...),
			Files: v.Files, SizeBytes: v.SizeBytes, CreatedAt: now, UpdatedAt: now}
		x.skills[u.ID] = u
		v.SkillID, v.Global = u.ID, false
		v.Status = store.SkillDraft
		if d.Publish {
			v.Status = store.SkillPublished
		}
		_ = c.insertVersionLocked(&v, now)
		m := &store.SkillMeta{SkillID: u.ID, DisplayName: d.DisplayName, LatestVersionID: v.ID}
		if d.Publish {
			m.PublishedVersionID = v.ID
		}
		c.meta[u.ID] = m
		items = append(items, store.DraftItem{SkillID: u.ID, VersionID: v.ID, Name: d.DisplayName})
	}
	body := response(items)
	c.commands[k] = store.CommandResult{RequestHash: idem.RequestHash, StatusCode: 201, Response: append([]byte(nil), body...)}
	return body, false, nil
}

func copyGlobal(g *store.GlobalSkill) *store.GlobalSkill {
	cp := *g
	cp.Tags = append([]string{}, g.Tags...)
	return &cp
}

// ListGlobalSkills implements store.SkillCatalogStore.
func (s *Store) ListGlobalSkills(_ context.Context) ([]*store.GlobalSkill, error) {
	c := s.c()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*store.GlobalSkill
	for _, g := range c.globals {
		out = append(out, copyGlobal(g))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// GetGlobalSkill implements store.SkillCatalogStore.
func (s *Store) GetGlobalSkill(_ context.Context, id string) (*store.GlobalSkill, error) {
	c := s.c()
	c.mu.Lock()
	defer c.mu.Unlock()
	g, ok := c.globals[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return copyGlobal(g), nil
}

// CreateGlobalSkill implements store.SkillCatalogStore.
func (s *Store) CreateGlobalSkill(_ context.Context, g store.GlobalSkill, v store.SkillVersion, publish bool) (*store.GlobalSkill, *store.SkillVersion, error) {
	if badText(g.Name, g.DisplayName, g.Description) {
		return nil, nil, ErrInvalidText
	}
	c := s.c()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cur := range c.globals {
		if cur.Name == g.Name {
			return nil, nil, store.ErrConflict
		}
	}
	now := s.now()
	n := g
	n.ID, n.CreatedAt, n.UpdatedAt = uuid.NewString(), now, now
	n.Tags = append([]string{}, g.Tags...)
	v.SkillID, v.Global = n.ID, true
	v.Status = store.SkillDraft
	if publish {
		v.Status = store.SkillPublished
	}
	_ = c.insertVersionLocked(&v, now)
	n.LatestVersionID = v.ID
	if publish {
		n.PublishedVersionID = v.ID
	}
	c.globals[n.ID] = &n
	return copyGlobal(&n), copyVersion(&v, false), nil
}

func (s *Store) updateGlobal(id string, fn func(g *store.GlobalSkill)) error {
	c := s.c()
	c.mu.Lock()
	defer c.mu.Unlock()
	g, ok := c.globals[id]
	if !ok {
		return store.ErrNotFound
	}
	fn(g)
	g.UpdatedAt = s.now()
	return nil
}

// UnpublishGlobalSkill implements store.SkillCatalogStore.
func (s *Store) UnpublishGlobalSkill(_ context.Context, id string) error {
	c := s.c()
	return s.updateGlobal(id, func(g *store.GlobalSkill) {
		g.PublishedVersionID = ""
		c.syncPublishedLocked(g.ID, true, "")
	})
}

// SetGlobalSkillTags implements store.SkillCatalogStore.
func (s *Store) SetGlobalSkillTags(_ context.Context, id string, tags []string) error {
	return s.updateGlobal(id, func(g *store.GlobalSkill) { g.Tags = append([]string{}, tags...) })
}

// DeleteGlobalSkill implements store.SkillCatalogStore.
func (s *Store) DeleteGlobalSkill(_ context.Context, id string) error {
	c := s.c()
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.globals[id]; !ok {
		return store.ErrNotFound
	}
	delete(c.globals, id)
	for vid, v := range c.versions {
		if v.Global && v.SkillID == id {
			delete(c.versions, vid)
		}
	}
	return nil
}

// CommandResult implements store.SkillCatalogStore.
func (s *Store) CommandResult(_ context.Context, login, command, key string) (*store.CommandResult, error) {
	c := s.c()
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.commands[commandKey(login, command, key)]
	if !ok {
		return nil, store.ErrNotFound
	}
	r.Response = append([]byte(nil), r.Response...)
	return &r, nil
}

// SaveCommandResult implements store.SkillCatalogStore.
func (s *Store) SaveCommandResult(_ context.Context, idem store.Idempotency, r store.CommandResult) error {
	c := s.c()
	c.mu.Lock()
	defer c.mu.Unlock()
	k := commandKey(idem.Login, idem.Command, idem.Key)
	if prev, ok := c.commands[k]; ok && (prev.StatusCode != 0 || prev.RequestHash != idem.RequestHash) {
		return store.ErrConflict
	}
	r.RequestHash = idem.RequestHash
	r.Response = append([]byte(nil), r.Response...)
	r.CreatedAt = s.now()
	if prev, ok := c.commands[k]; ok {
		r.CreatedAt = prev.CreatedAt
	}
	c.commands[k] = r
	return nil
}

// ClaimCommand implements store.SkillCatalogStore.
func (s *Store) ClaimCommand(_ context.Context, idem store.Idempotency) (bool, error) {
	c := s.c()
	c.mu.Lock()
	defer c.mu.Unlock()
	k := commandKey(idem.Login, idem.Command, idem.Key)
	now := s.now()
	if prev, ok := c.commands[k]; ok {
		stale := prev.StatusCode == 0 && now.Sub(prev.CreatedAt) > store.CommandClaimLease
		if !stale && now.Sub(prev.CreatedAt) <= store.CommandRetention {
			return false, nil
		}
	}
	c.commands[k] = store.CommandResult{RequestHash: idem.RequestHash, CreatedAt: now}
	return true, nil
}

// ReleaseCommand implements store.SkillCatalogStore.
func (s *Store) ReleaseCommand(_ context.Context, idem store.Idempotency) error {
	c := s.c()
	c.mu.Lock()
	defer c.mu.Unlock()
	k := commandKey(idem.Login, idem.Command, idem.Key)
	if r, ok := c.commands[k]; ok && r.StatusCode == 0 {
		delete(c.commands, k)
	}
	return nil
}

// CreateSkillCreationSession implements store.SkillCatalogStore.
func (s *Store) CreateSkillCreationSession(_ context.Context, cs store.SkillCreationSession) error {
	if !s.threadAlive(cs.ThreadID) {
		return store.ErrNotFound
	}
	c := s.c()
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.sessions[cs.ThreadID]; ok {
		return store.ErrConflict
	}
	if cs.Mode == "" {
		cs.Mode = "skill_creation"
	}
	if cs.Action == "" {
		cs.Action = "create"
	}
	cs.CreatedAt = s.now()
	c.sessions[cs.ThreadID] = &cs
	return nil
}

func (s *Store) threadTitle(threadID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.threads[threadID]
	if !ok || t.deleted {
		return "", false
	}
	return t.Title, true
}

// GetSkillCreationSession implements store.SkillCatalogStore.
func (s *Store) GetSkillCreationSession(_ context.Context, login, threadID string) (*store.SkillCreationSession, error) {
	title, alive := s.threadTitle(threadID)
	c := s.c()
	c.mu.Lock()
	defer c.mu.Unlock()
	cs, ok := c.sessions[threadID]
	if !ok || !alive || (login != "" && cs.UserLogin != login) {
		return nil, store.ErrNotFound
	}
	cp := *cs
	cp.Title = title
	return &cp, nil
}

// ListSkillCreationSessions implements store.SkillCatalogStore.
func (s *Store) ListSkillCreationSessions(_ context.Context, login string, limit, offset int) ([]*store.SkillCreationSession, int, error) {
	c := s.c()
	c.mu.Lock()
	var all []*store.SkillCreationSession
	for _, cs := range c.sessions {
		if cs.UserLogin == login {
			cp := *cs
			all = append(all, &cp)
		}
	}
	c.mu.Unlock()
	var live []*store.SkillCreationSession
	for _, cs := range all {
		if title, ok := s.threadTitle(cs.ThreadID); ok {
			cs.Title = title
			live = append(live, cs)
		}
	}
	sort.Slice(live, func(i, j int) bool {
		if !live[i].CreatedAt.Equal(live[j].CreatedAt) {
			return live[i].CreatedAt.After(live[j].CreatedAt)
		}
		return live[i].ThreadID > live[j].ThreadID
	})
	total := len(live)
	if offset > len(live) {
		offset = len(live)
	}
	live = live[offset:]
	if limit >= 0 && limit < len(live) {
		live = live[:limit]
	}
	return live, total, nil
}

var _ store.SkillCatalogStore = (*Store)(nil)
