package memstore

import (
	"bytes"
	"context"
	"sync"

	"github.com/GhaziBenDahmane/harness/store"
)

// credentials are kept beside the Store (keyed by it) so the Store struct is unchanged.
var credentials sync.Map // *Store → *credTable

type credTable struct {
	mu   sync.Mutex
	rows map[string]store.RunCredential
}

func (s *Store) creds() *credTable {
	v, _ := credentials.LoadOrStore(s, &credTable{rows: map[string]store.RunCredential{}})
	return v.(*credTable)
}

// PutRunCredential implements store.CredentialStore.
func (s *Store) PutRunCredential(_ context.Context, c store.RunCredential, replace bool) (bool, error) {
	s.mu.Lock()
	r, ok := s.runs[c.RunID]
	active := ok && r.UserLogin == c.UserLogin && !store.IsTerminal(r.Status)
	s.mu.Unlock()
	if !active {
		return false, nil
	}
	t := s.creds()
	t.mu.Lock()
	defer t.mu.Unlock()
	if old, ok := t.rows[c.RunID]; ok && (old.UserLogin != c.UserLogin || (!replace && !old.ExpiresAt.Before(c.ExpiresAt))) {
		return false, nil
	}
	c.Sealed = bytes.Clone(c.Sealed)
	t.rows[c.RunID] = c
	return true, nil
}

// GetRunCredential implements store.CredentialStore.
func (s *Store) GetRunCredential(_ context.Context, runID string) (*store.RunCredential, error) {
	t := s.creds()
	t.mu.Lock()
	defer t.mu.Unlock()
	c, ok := t.rows[runID]
	if !ok {
		return nil, store.ErrNotFound
	}
	c.Sealed = bytes.Clone(c.Sealed)
	return &c, nil
}

// DeleteRunCredential implements store.CredentialStore.
func (s *Store) DeleteRunCredential(_ context.Context, runID string) error {
	t := s.creds()
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.rows, runID)
	return nil
}

// PurgeRunCredentials implements store.CredentialStore.
func (s *Store) PurgeRunCredentials(_ context.Context) (int64, error) {
	s.mu.Lock()
	now := s.now()
	active := map[string]bool{}
	for id, r := range s.runs {
		active[id] = !store.IsTerminal(r.Status)
	}
	s.mu.Unlock()
	t := s.creds()
	t.mu.Lock()
	defer t.mu.Unlock()
	var n int64
	for id, c := range t.rows {
		if !active[id] || !c.ExpiresAt.After(now) {
			delete(t.rows, id)
			n++
		}
	}
	return n, nil
}
