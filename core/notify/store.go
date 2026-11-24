package notify

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// Store persists rules, channels, preferences and the delivery log.
type Store interface {
	// GetRules returns the project's rules, DefaultRules when it set none.
	GetRules(ctx context.Context, project string) (Rules, error)
	PutRules(ctx context.Context, r Rules) error
	// ListChannels returns the project's channels ordered by name.
	ListChannels(ctx context.Context, project string) ([]Channel, error)
	// GetChannel returns the channel or ErrNotFound (also when it belongs to another project).
	GetChannel(ctx context.Context, project, id string) (Channel, error)
	PutChannel(ctx context.Context, c Channel) error
	// DeleteChannel removes the channel (no error when absent).
	DeleteChannel(ctx context.Context, project, id string) error
	// GetPrefs returns the person's preferences, an empty Prefs{Login} when they chose nothing.
	GetPrefs(ctx context.Context, login string) (Prefs, error)
	PutPrefs(ctx context.Context, p Prefs) error
	// AppendLog records a delivery; the store keeps the last MaxLog entries per project.
	AppendLog(ctx context.Context, e LogEntry) error
	// ListLog returns up to limit entries, newest first, of a project ("" for those without one), only the entries of
	// login when it is not empty.
	ListLog(ctx context.Context, project, login string, limit int) ([]LogEntry, error)
	// DeleteProject removes everything the project owns.
	DeleteProject(ctx context.Context, project string) error
}

// Snapshot is the whole state of a MemStore, for the stores that persist it.
type Snapshot struct {
	Version  int                   `json:"version"`
	Rules    map[string]Rules      `json:"rules"`
	Channels []Channel             `json:"channels"`
	Prefs    map[string]Prefs      `json:"prefs"`
	Log      map[string][]LogEntry `json:"log"`
}

// MemStore is a Store in memory. With OnChange set (a file store does) every change is handed to it before it is
// applied, and one that fails is refused.
type MemStore struct {
	mu       sync.Mutex
	rules    map[string]Rules
	channels map[string]Channel // by id
	prefs    map[string]Prefs
	log      map[string][]LogEntry // newest last
	now      func() time.Time
	// OnChange persists the state after a change; set it before use.
	OnChange func(Snapshot) error
}

var _ Store = (*MemStore)(nil)

// NewMemStore returns an empty store.
func NewMemStore(now func() time.Time) *MemStore {
	if now == nil {
		now = time.Now
	}
	return &MemStore{rules: map[string]Rules{}, channels: map[string]Channel{}, prefs: map[string]Prefs{}, log: map[string][]LogEntry{}, now: now}
}

// Load replaces the state with a snapshot.
func (s *MemStore) Load(sn Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules, s.prefs, s.log = map[string]Rules{}, map[string]Prefs{}, map[string][]LogEntry{}
	s.channels = map[string]Channel{}
	for k, v := range sn.Rules {
		s.rules[k] = v
	}
	for _, c := range sn.Channels {
		s.channels[c.ID] = c
	}
	for k, v := range sn.Prefs {
		s.prefs[k] = v
	}
	for k, v := range sn.Log {
		s.log[k] = append([]LogEntry(nil), v...)
	}
}

func (s *MemStore) snapshot() Snapshot {
	sn := Snapshot{Version: 1, Rules: map[string]Rules{}, Prefs: map[string]Prefs{}, Log: map[string][]LogEntry{}}
	for k, v := range s.rules {
		sn.Rules[k] = v
	}
	for _, c := range s.channels {
		sn.Channels = append(sn.Channels, c)
	}
	SortChannels(sn.Channels)
	for k, v := range s.prefs {
		sn.Prefs[k] = v
	}
	for k, v := range s.log {
		sn.Log[k] = append([]LogEntry(nil), v...)
	}
	return sn
}

// commit runs change, persists, and undoes the change when persisting fails.
func (s *MemStore) commit(change func() (undo func())) error {
	undo := change()
	if s.OnChange == nil {
		return nil
	}
	if err := s.OnChange(s.snapshot()); err != nil {
		undo()
		return err
	}
	return nil
}

// GetRules implements Store.
func (s *MemStore) GetRules(_ context.Context, project string) (Rules, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.rules[project]; ok {
		return cloneRules(r), nil
	}
	return DefaultRules(project), nil
}

func cloneRules(r Rules) Rules {
	b, _ := json.Marshal(r)
	var out Rules
	_ = json.Unmarshal(b, &out)
	if out.Events == nil {
		out.Events = map[Kind]Rule{}
	}
	return out
}

// PutRules implements Store.
func (s *MemStore) PutRules(_ context.Context, r Rules) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r = cloneRules(r)
	r.UpdatedAt = s.now()
	return s.commit(func() func() {
		old, had := s.rules[r.ProjectID]
		s.rules[r.ProjectID] = r
		return func() {
			if had {
				s.rules[r.ProjectID] = old
			} else {
				delete(s.rules, r.ProjectID)
			}
		}
	})
}

// ListChannels implements Store.
func (s *MemStore) ListChannels(_ context.Context, project string) ([]Channel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Channel{}
	for _, c := range s.channels {
		if c.ProjectID == project {
			out = append(out, c)
		}
	}
	SortChannels(out)
	return out, nil
}

// GetChannel implements Store.
func (s *MemStore) GetChannel(_ context.Context, project, id string) (Channel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.channels[id]; ok && c.ProjectID == project {
		return c, nil
	}
	return Channel{}, ErrNotFound
}

// PutChannel implements Store.
func (s *MemStore) PutChannel(_ context.Context, c Channel) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = s.now()
	}
	return s.commit(func() func() {
		old, had := s.channels[c.ID]
		s.channels[c.ID] = c
		return func() {
			if had {
				s.channels[c.ID] = old
			} else {
				delete(s.channels, c.ID)
			}
		}
	})
}

// DeleteChannel implements Store.
func (s *MemStore) DeleteChannel(_ context.Context, project, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.channels[id]
	if !ok || old.ProjectID != project {
		return nil
	}
	return s.commit(func() func() {
		delete(s.channels, id)
		return func() { s.channels[id] = old }
	})
}

// GetPrefs implements Store.
func (s *MemStore) GetPrefs(_ context.Context, login string) (Prefs, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.prefs[login]; ok {
		b, _ := json.Marshal(p)
		var out Prefs
		_ = json.Unmarshal(b, &out)
		return out, nil
	}
	return Prefs{Login: login}, nil
}

// PutPrefs implements Store.
func (s *MemStore) PutPrefs(_ context.Context, p Prefs) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p.UpdatedAt = s.now()
	return s.commit(func() func() {
		old, had := s.prefs[p.Login]
		s.prefs[p.Login] = p
		return func() {
			if had {
				s.prefs[p.Login] = old
			} else {
				delete(s.prefs, p.Login)
			}
		}
	})
}

// AppendLog implements Store.
func (s *MemStore) AppendLog(_ context.Context, e LogEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.At.IsZero() {
		e.At = s.now()
	}
	return s.commit(func() func() {
		old := s.log[e.ProjectID]
		next := append(append([]LogEntry(nil), old...), e)
		if len(next) > MaxLog {
			next = next[len(next)-MaxLog:]
		}
		s.log[e.ProjectID] = next
		return func() { s.log[e.ProjectID] = old }
	})
}

// ListLog implements Store.
func (s *MemStore) ListLog(_ context.Context, project, login string, limit int) ([]LogEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > MaxLog {
		limit = MaxLog
	}
	out := []LogEntry{}
	all := s.log[project]
	for i := len(all) - 1; i >= 0 && len(out) < limit; i-- {
		if login == "" || all[i].Login == login {
			out = append(out, all[i])
		}
	}
	return out, nil
}

// DeleteProject implements Store.
func (s *MemStore) DeleteProject(_ context.Context, project string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commit(func() func() {
		oldRules, hadRules := s.rules[project]
		oldLog, oldChans := s.log[project], map[string]Channel{}
		delete(s.rules, project)
		delete(s.log, project)
		for id, c := range s.channels {
			if c.ProjectID == project {
				oldChans[id] = c
				delete(s.channels, id)
			}
		}
		return func() {
			if hadRules {
				s.rules[project] = oldRules
			}
			s.log[project] = oldLog
			for id, c := range oldChans {
				s.channels[id] = c
			}
		}
	})
}
