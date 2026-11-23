// Package usagestore implements usage.Store: Mem (in memory), File (Mem persisted to disk) and PG (tables of migration
// 0580, applied by pgstore).
package usagestore

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/7ar-chat/harness/core/usage"
)

// Mem is a usage.Store in memory.
type Mem struct {
	mu      sync.Mutex
	entries []usage.Entry
	runs    map[string]bool
	caps    map[string]float64
	// onAppend and onCaps persist a change before it is applied (File sets them); an error rejects the change.
	onAppend func([]usage.Entry) error
	onCaps   func(map[string]float64) error
}

var _ usage.Store = (*Mem)(nil)

// NewMem returns an empty store.
func NewMem() *Mem { return &Mem{runs: map[string]bool{}, caps: map[string]float64{}} }

func capKey(scope, id string) string { return scope + "\x00" + id }

func checkCap(scope, id string, usd float64) error {
	if scope != usage.ScopeProject && scope != usage.ScopeUser {
		return fmt.Errorf("%w: cap scope %q", usage.ErrInvalid, scope)
	}
	if id == "" {
		return fmt.Errorf("%w: cap id", usage.ErrInvalid)
	}
	if usd < 0 || math.IsNaN(usd) || math.IsInf(usd, 0) {
		return fmt.Errorf("%w: cap must be a non-negative amount", usage.ErrInvalid)
	}
	return nil
}

// Record implements usage.Store.
func (m *Mem) Record(_ context.Context, entries []usage.Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var fresh []usage.Entry
	added := map[string]bool{}
	for _, e := range entries {
		if e.RunID == "" {
			return fmt.Errorf("%w: entry without a run id", usage.ErrInvalid)
		}
		if m.runs[e.RunID] {
			continue // the run is already booked
		}
		e.At = e.At.UTC()
		fresh = append(fresh, e)
		added[e.RunID] = true
	}
	if len(fresh) == 0 {
		return nil
	}
	if m.onAppend != nil {
		if err := m.onAppend(fresh); err != nil {
			return err
		}
	}
	m.entries = append(m.entries, fresh...)
	for id := range added {
		m.runs[id] = true
	}
	return nil
}

func (m *Mem) selected(q usage.Query) []usage.Entry {
	var out []usage.Entry
	for _, e := range m.entries {
		if q.Matches(e) {
			out = append(out, e)
		}
	}
	return out
}

// Report implements usage.Store.
func (m *Mem) Report(_ context.Context, q usage.Query) (*usage.Report, error) {
	if !usage.ValidGroup(q.Group) {
		return nil, fmt.Errorf("%w: group %q", usage.ErrInvalid, q.Group)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return usage.Aggregate(m.selected(q), q), nil
}

// Spent implements usage.Store.
func (m *Mem) Spent(_ context.Context, projectID, user string, from time.Time) (float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var sum float64
	for _, e := range m.selected(usage.Query{ProjectID: projectID, User: user, From: from}) {
		sum += e.CostUSD
	}
	return sum, nil
}

// SetCap implements usage.Store.
func (m *Mem) SetCap(_ context.Context, scope, id string, usd float64) error {
	if err := checkCap(scope, id, usd); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	next := make(map[string]float64, len(m.caps)+1)
	for k, v := range m.caps {
		next[k] = v
	}
	if usd == 0 {
		delete(next, capKey(scope, id))
	} else {
		next[capKey(scope, id)] = usd
	}
	if m.onCaps != nil {
		if err := m.onCaps(next); err != nil {
			return err
		}
	}
	m.caps = next
	return nil
}

// Cap implements usage.Store.
func (m *Mem) Cap(_ context.Context, scope, id string) (float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.caps[capKey(scope, id)], nil
}
