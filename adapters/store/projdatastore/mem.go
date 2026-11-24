// Package projdatastore stores core/projdata: in memory, in a JSON file and in Postgres (migration 1400). One
// conformance suite runs against all three.
package projdatastore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/7ar-chat/harness/core/projdata"
)

// Snapshot is the whole state of a Mem, as the file store writes it.
type Snapshot struct {
	Version  int                                `json:"version"`
	Settings map[string]projdata.DigestSettings `json:"settings"`
	Slots    map[string]time.Time               `json:"slots"` // project + "\x00" + slot
	Digests  []projdata.Digest                  `json:"digests"`
	Secrets  map[string][]byte                  `json:"secrets"` // project + "\x00" + kind
}

// Mem is a Store in memory. With persist set (the file store does) every change is handed to it and one that
// fails is undone.
type Mem struct {
	mu      sync.Mutex
	st      Snapshot
	persist func(Snapshot) error
}

var _ projdata.Store = (*Mem)(nil)

func empty() Snapshot {
	return Snapshot{Version: 1, Settings: map[string]projdata.DigestSettings{}, Slots: map[string]time.Time{}, Secrets: map[string][]byte{}}
}

// NewMem returns an empty store.
func NewMem() *Mem { return &Mem{st: empty()} }

// Open returns a store persisted to a JSON file (0600, replaced atomically). One process per file.
func Open(path string) (*Mem, error) {
	m := NewMem()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("projdatastore: create directory of %s: %w", path, err)
	}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("projdatastore: read %s: %w", path, err)
	default:
		var sn Snapshot
		if err := json.Unmarshal(b, &sn); err != nil || sn.Version != 1 {
			return nil, fmt.Errorf("projdatastore: %s is not a version 1 project data file", path)
		}
		e := empty()
		if sn.Settings == nil {
			sn.Settings = e.Settings
		}
		if sn.Slots == nil {
			sn.Slots = e.Slots
		}
		if sn.Secrets == nil {
			sn.Secrets = e.Secrets
		}
		m.st = sn
	}
	m.persist = func(sn Snapshot) error {
		b, err := json.Marshal(&sn)
		if err != nil {
			return err
		}
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			return fmt.Errorf("projdatastore: write %s: %w", tmp, err)
		}
		if err := os.Rename(tmp, path); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("projdatastore: replace %s: %w", path, err)
		}
		return nil
	}
	return m, nil
}

func (m *Mem) clone() Snapshot {
	c := Snapshot{Version: 1, Settings: map[string]projdata.DigestSettings{}, Slots: map[string]time.Time{}, Secrets: map[string][]byte{}, Digests: slices.Clone(m.st.Digests)}
	for k, v := range m.st.Settings {
		c.Settings[k] = v
	}
	for k, v := range m.st.Slots {
		c.Slots[k] = v
	}
	for k, v := range m.st.Secrets {
		c.Secrets[k] = v
	}
	return c
}

// commit runs change under the lock, persists, and restores the old state when persisting fails.
func (m *Mem) commit(change func()) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.clone()
	change()
	if m.persist == nil {
		return nil
	}
	if err := m.persist(m.st); err != nil {
		m.st = old
		return err
	}
	return nil
}

// GetDigestSettings implements projdata.Store.
func (m *Mem) GetDigestSettings(_ context.Context, project string) (projdata.DigestSettings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.st.Settings[project]; ok {
		return s, nil
	}
	return projdata.DigestSettings{ProjectID: project}, nil
}

// PutDigestSettings implements projdata.Store.
func (m *Mem) PutDigestSettings(_ context.Context, s projdata.DigestSettings) error {
	return m.commit(func() { m.st.Settings[s.ProjectID] = s })
}

// EnabledDigests implements projdata.Store.
func (m *Mem) EnabledDigests(context.Context) ([]projdata.DigestSettings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []projdata.DigestSettings
	for _, s := range m.st.Settings {
		if s.Enabled {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProjectID < out[j].ProjectID })
	return out, nil
}

// ClaimSlot implements projdata.Store.
func (m *Mem) ClaimSlot(_ context.Context, project, slot string, at time.Time) (bool, error) {
	key := project + "\x00" + slot
	won := false
	err := m.commit(func() {
		if _, ok := m.st.Slots[key]; !ok {
			m.st.Slots[key], won = at, true
		}
	})
	return won && err == nil, err
}

// ReleaseSlot implements projdata.Store.
func (m *Mem) ReleaseSlot(_ context.Context, project, slot string) error {
	return m.commit(func() { delete(m.st.Slots, project+"\x00"+slot) })
}

// AddDigest implements projdata.Store.
func (m *Mem) AddDigest(_ context.Context, d projdata.Digest) error {
	d.HTML, d.JSON = slices.Clone(d.HTML), slices.Clone(d.JSON)
	return m.commit(func() {
		m.st.Digests = append(m.st.Digests, d)
		var mine []int
		for i, x := range m.st.Digests {
			if x.ProjectID == d.ProjectID {
				mine = append(mine, i)
			}
		}
		extra := len(mine) - projdata.KeepDigests
		if extra <= 0 {
			return
		}
		sort.SliceStable(mine, func(a, b int) bool { return m.st.Digests[mine[a]].CreatedAt.Before(m.st.Digests[mine[b]].CreatedAt) })
		drop := map[int]bool{}
		for _, i := range mine[:extra] {
			drop[i] = true
		}
		var keep []projdata.Digest
		for i, x := range m.st.Digests {
			if !drop[i] {
				keep = append(keep, x)
			}
		}
		m.st.Digests = keep
	})
}

// ListDigests implements projdata.Store.
func (m *Mem) ListDigests(_ context.Context, project string, limit int) ([]projdata.Digest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []projdata.Digest{}
	for _, d := range m.st.Digests {
		if d.ProjectID == project {
			d.HTML, d.JSON = nil, nil
			out = append(out, d)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// GetDigest implements projdata.Store.
func (m *Mem) GetDigest(_ context.Context, project, id string) (projdata.Digest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range m.st.Digests {
		if d.ID == id && d.ProjectID == project {
			d.HTML, d.JSON = slices.Clone(d.HTML), slices.Clone(d.JSON)
			return d, nil
		}
	}
	return projdata.Digest{}, projdata.ErrNotFound
}

// PutSecret implements projdata.Store.
func (m *Mem) PutSecret(_ context.Context, project, kind string, sealed []byte) error {
	sealed = slices.Clone(sealed)
	return m.commit(func() { m.st.Secrets[project+"\x00"+kind] = sealed })
}

// GetSecret implements projdata.Store.
func (m *Mem) GetSecret(_ context.Context, project, kind string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.st.Secrets[project+"\x00"+kind]; ok {
		return slices.Clone(b), nil
	}
	return nil, projdata.ErrNotFound
}

// DeleteSecret implements projdata.Store.
func (m *Mem) DeleteSecret(_ context.Context, project, kind string) error {
	return m.commit(func() { delete(m.st.Secrets, project+"\x00"+kind) })
}

// DeleteProject implements projdata.Store.
func (m *Mem) DeleteProject(_ context.Context, project string) error {
	return m.commit(func() {
		delete(m.st.Settings, project)
		prefix := project + "\x00"
		for k := range m.st.Slots {
			if strings.HasPrefix(k, prefix) {
				delete(m.st.Slots, k)
			}
		}
		for k := range m.st.Secrets {
			if strings.HasPrefix(k, prefix) {
				delete(m.st.Secrets, k)
			}
		}
		var keep []projdata.Digest
		for _, d := range m.st.Digests {
			if d.ProjectID != project {
				keep = append(keep, d)
			}
		}
		m.st.Digests = keep
	})
}
