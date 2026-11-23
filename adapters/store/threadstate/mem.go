// Package threadstate implements store.ThreadStateStore: Mem in memory, File persisted to a JSON file, PG on
// Postgres (table of migration 0700, applied by pgstore) shared between processes. One conformance suite runs
// against all of them.
package threadstate

import (
	"context"
	"encoding/json"
	"slices"
	"sync"

	"github.com/7ar-chat/harness/core/store"
)

// Mem is a store.ThreadStateStore in memory.
type Mem struct {
	mu   sync.Mutex
	docs map[string]map[string]json.RawMessage
	// save persists the new state before it replaces the current one (nil for a pure memory store).
	save func(map[string]map[string]json.RawMessage) error
}

var _ store.ThreadStateStore = (*Mem)(nil)

// NewMem returns an empty store.
func NewMem() *Mem { return &Mem{docs: map[string]map[string]json.RawMessage{}} }

// GetThreadState implements store.ThreadStateStore.
func (m *Mem) GetThreadState(_ context.Context, threadID, key string) (json.RawMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.docs[threadID][key]), nil
}

// UpdateThreadState implements store.ThreadStateStore.
func (m *Mem) UpdateThreadState(_ context.Context, threadID, key string, fn func(json.RawMessage) (json.RawMessage, error)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	out, err := fn(slices.Clone(m.docs[threadID][key]))
	if err != nil {
		return err
	}
	next := m.cloneDocs()
	if out == nil {
		delete(next[threadID], key)
		if len(next[threadID]) == 0 {
			delete(next, threadID)
		}
	} else {
		if next[threadID] == nil {
			next[threadID] = map[string]json.RawMessage{}
		}
		next[threadID][key] = slices.Clone(out)
	}
	return m.commit(next)
}

// DeleteThreadState implements store.ThreadStateStore.
func (m *Mem) DeleteThreadState(_ context.Context, threadID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.docs[threadID]; !ok {
		return nil
	}
	next := m.cloneDocs()
	delete(next, threadID)
	return m.commit(next)
}

func (m *Mem) commit(next map[string]map[string]json.RawMessage) error {
	if m.save != nil {
		if err := m.save(next); err != nil {
			return err
		}
	}
	m.docs = next
	return nil
}

func (m *Mem) cloneDocs() map[string]map[string]json.RawMessage {
	out := make(map[string]map[string]json.RawMessage, len(m.docs))
	for t, kv := range m.docs {
		c := make(map[string]json.RawMessage, len(kv))
		for k, v := range kv {
			c[k] = v
		}
		out[t] = c
	}
	return out
}
