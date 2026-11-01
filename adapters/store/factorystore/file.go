package factorystore

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/7ar-chat/harness/core/factory"
)

const (
	// fileVersion is the schema of the persisted file; Open refuses any other.
	fileVersion = 1
	// maxEventsPerTask bounds a task's history in memory and in the file: the oldest events go first.
	maxEventsPerTask = 500
	// ledgerRetention is how long spend ledger entries survive a restart. The daily budget looks back
	// 24 h, a report at most a few weeks.
	ledgerRetention = 40 * 24 * time.Hour
	// flushEvery is the longest an event (or a webhook delivery id) waits before it is written: a burst
	// of AppendEvent calls costs one rewrite of the file per interval, not one per call.
	flushEvery = 200 * time.Millisecond
)

// fileData is the persisted form of Mem: everything a restart must not lose.
type fileData struct {
	Version    int                            `json:"version"`
	Tasks      map[string]*factory.Task       `json:"tasks"`
	Events     map[string][]factory.TaskEvent `json:"events"`
	NextEvent  int64                          `json:"nextEvent"`
	Triggers   map[string]*factory.Trigger    `json:"triggers"`
	Deliveries []string                       `json:"deliveries"` // oldest first
	Threads    map[string]factory.ThreadInfo  `json:"threads"`
	Ledger     []ledgerEntry                  `json:"ledger"`
}

// Open returns a Mem persisted to path, loading the file if it exists. A missing file is an empty
// store; a file that cannot be read or parsed is an error naming the path and is left untouched,
// because starting empty over it would silently drop tasks whose pull requests are still open.
// The parent directory is created (0700). path "" is NewMem: nothing is persisted.
//
// Persistence policy, per method:
//   - CreateTask, UpdateTask, AddSpend, PutTrigger, DeleteTrigger, SetThreadInfo write the whole file
//     before returning; when that fails the in-memory change is rolled back and the error returned, so
//     memory never gets ahead of what a restart would restore.
//   - AppendEvent and SeenDelivery are coalesced (at most one rewrite per 200 ms) and best-effort: a
//     failed write is logged and retried with the next change, so a full disk does not stop a task's
//     progress. A crash loses at most the last 200 ms of them.
//   - ClaimDueTriggers writes immediately but only logs a failure: the claim must be returned or the
//     tick is lost, and a tick fired twice after a restart is deduplicated by Source.Key.
//
// Call Close to flush pending events. One process per file: there is no cross-process locking.
func Open(path string, now func() time.Time) (*Mem, error) {
	m := NewMem(now)
	if path == "" {
		return m, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("factorystore: create directory of %s: %w", path, err)
	}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("factorystore: read %s: %w", path, err)
	default:
		if err := m.load(b); err != nil {
			return nil, fmt.Errorf("factorystore: load %s: %w", path, err)
		}
	}
	m.path = path
	return m, nil
}

// load replaces the empty state with the file's content.
func (m *Mem) load(b []byte) error {
	var d fileData
	if err := json.Unmarshal(b, &d); err != nil {
		return err
	}
	if d.Version != fileVersion {
		return fmt.Errorf("unsupported version %d (want %d)", d.Version, fileVersion)
	}
	for id, t := range d.Tasks {
		if t == nil || t.ID != id {
			return fmt.Errorf("task %q: entry does not match its id", id)
		}
		m.tasks[id] = t
	}
	for id, t := range d.Triggers {
		if t == nil || t.ID != id {
			return fmt.Errorf("trigger %q: entry does not match its id", id)
		}
		m.triggers[id] = t
	}
	for id, ti := range d.Threads {
		if ti.ThreadID != id {
			return fmt.Errorf("thread %q: entry does not match its id", id)
		}
		m.threads[id] = ti
	}
	m.nextEvent = d.NextEvent
	for id, ev := range d.Events {
		if m.tasks[id] == nil {
			continue // history of a task that no longer exists
		}
		if len(ev) > maxEventsPerTask {
			ev = ev[len(ev)-maxEventsPerTask:]
		}
		m.events[id] = ev
		for _, e := range ev {
			m.nextEvent = max(m.nextEvent, e.ID)
		}
	}
	if len(d.Deliveries) > maxDeliveries {
		d.Deliveries = d.Deliveries[len(d.Deliveries)-maxDeliveries:]
	}
	for _, id := range d.Deliveries {
		if _, dup := m.deliveries[id]; !dup {
			m.deliveries[id] = struct{}{}
			m.deliveryQ = append(m.deliveryQ, id)
		}
	}
	cutoff := m.now().Add(-ledgerRetention)
	for _, e := range d.Ledger {
		if !e.At.Before(cutoff) {
			m.ledger = append(m.ledger, e)
		}
	}
	return nil
}

// saveLocked writes the whole state now; the caller holds mu. On success pending coalesced changes
// are in the file too, so the flush timer is cancelled.
func (m *Mem) saveLocked() error {
	if m.path == "" {
		return nil
	}
	if err := m.writeLocked(); err != nil {
		return err
	}
	m.dirty = false
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	return nil
}

// bestEffortLocked is saveLocked for changes that must not fail their caller.
func (m *Mem) bestEffortLocked() {
	if err := m.saveLocked(); err != nil {
		slog.Warn("factory: persist state", "path", m.path, "err", err)
	}
}

// scheduleLocked marks the state dirty and arranges one write within flushEvery. After Close there
// is no timer left, so the change is written at once.
func (m *Mem) scheduleLocked() {
	if m.path == "" {
		return
	}
	m.dirty = true
	if m.closed {
		m.bestEffortLocked()
		return
	}
	if m.timer == nil {
		m.timer = time.AfterFunc(m.flushEvery, m.flushTimer)
	}
}

func (m *Mem) flushTimer() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.timer = nil
	if !m.dirty {
		return
	}
	if err := m.writeLocked(); err != nil {
		// Stay dirty: the next change schedules another attempt, Close tries a last time.
		m.failures.Add(1)
		slog.Warn("factory: persist state", "path", m.path, "err", err)
		return
	}
	m.dirty = false
}

// Close writes pending changes and stops the flush timer. It is idempotent; the store stays usable
// afterwards (changes then persist synchronously) so a late event is not lost to a shutdown race.
func (m *Mem) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	if m.path == "" || !m.dirty {
		return nil
	}
	if err := m.writeLocked(); err != nil {
		return err
	}
	m.dirty = false
	return nil
}

// writeLocked replaces the file atomically: a 0600 temp file in the same directory, fsynced, renamed
// over the target, then the directory fsynced so the rename itself survives a power cut. A failure
// after the rename (the directory sync) leaves the disk ahead of a rolled-back memory; the next
// successful write rewrites the whole state, which heals it.
func (m *Mem) writeLocked() error {
	d := fileData{
		Version: fileVersion, Tasks: m.tasks, Events: m.events, NextEvent: m.nextEvent,
		Triggers: m.triggers, Deliveries: m.deliveryQ, Threads: m.threads, Ledger: m.ledger,
	}
	b, err := json.Marshal(&d)
	if err != nil {
		return fmt.Errorf("factorystore: encode %s: %w", m.path, err)
	}
	tmp := m.path + ".tmp"
	// A leftover from a crash may have another mode; start from nothing.
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("factorystore: remove stale %s: %w", tmp, err)
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("factorystore: write %s: %w", tmp, err)
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("factorystore: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, m.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("factorystore: replace %s: %w", m.path, err)
	}
	m.writes.Add(1)
	if err := syncDir(filepath.Dir(m.path)); err != nil {
		return fmt.Errorf("factorystore: sync directory of %s: %w", m.path, err)
	}
	return nil
}

// syncDir fsyncs a directory; filesystems that cannot (EINVAL) are not an error.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if errors.Is(err, syscall.EINVAL) {
		return nil
	}
	return err
}
