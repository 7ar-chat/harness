// Package filestore is a durable single-process store.Full for local hosts
// (CLI, TUI, desktop): it wraps store/memstore and persists the conversation
// state to an append-only JSON-lines journal in a state directory, so a
// closed terminal (or kill -9) never loses a session. No Postgres, no Redis,
// no cgo.
//
// # Design
//
// Reads are served from memory (memstore). Every mutating call is applied to
// memstore first, then the entities it changed are written to the journal as
// idempotent upsert records (thread, run, approval, summary, goal, feedback,
// superseded ids, compactions, plus append-only "msg" and "events" rows).
// Unchanged records are skipped (content hash), so heartbeats and no-op calls
// cost nothing. The journal is replayed into a fresh memstore on Open.
//
// # Durability
//
// Each batch is one write(2) on an O_APPEND file: the bytes are in the kernel
// when the call returns, so a process crash (kill -9) loses nothing and can
// only tear the last line, which Open detects and truncates. fsync policy
// (Options.Sync): SyncInterval (default) fsyncs at most every 500 ms and on
// Close, so a power failure loses at most that window; SyncAlways fsyncs every
// batch; SyncNever leaves it to the OS. A bad line that is not at the tail is
// reported as ErrCorrupt rather than silently skipped.
//
// # What persists
//
// Threads (title, pin, plan, pending question, interaction mode), messages,
// runs, event logs, approvals, goals, feedback, summaries and compactions.
// Artifacts, blobs, skills catalog, memories, workspace git archives and
// sandbox ids stay in memory (they are rebuilt or recreated on demand).
//
// # Crash recovery
//
// On Open every run that was queued or running when the previous process died
// is marked interrupted and its event log gets a RUN_ERROR(interrupted) so
// replays terminate.
//
// The journal is compacted (rewritten from the live state through a temp file
// and an atomic rename) on Open when it holds many superseded records or
// deleted threads.
package filestore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/GhaziBenDahmane/harness/adapters/store/memstore"
	"github.com/GhaziBenDahmane/harness/core/agui"
	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/eventlog"
)

// ErrClosed is returned by mutations after Close.
var ErrClosed = errors.New("filestore: closed")

// SyncPolicy chooses when the journal is fsynced.
type SyncPolicy int

const (
	// SyncInterval fsyncs at most every 500 ms and on Close (default).
	SyncInterval SyncPolicy = iota
	// SyncAlways fsyncs after every batch.
	SyncAlways
	// SyncNever never fsyncs (process crashes are still safe; power loss is not).
	SyncNever
)

// Options configures Open.
type Options struct {
	Sync SyncPolicy
	// SyncEvery overrides the 500 ms of SyncInterval.
	SyncEvery time.Duration
}

// Store is a store.Full (and harness.Store) persisting to a directory.
type Store struct {
	*memstore.Store
	dir  string
	lock *os.File

	jmu   sync.Mutex // serializes export+write so the journal order follows the state
	j     *journal
	cache map[string]uint64   // record key -> hash of the last written value
	msgs  map[string]struct{} // thread\x00id of the messages already journaled
	opts  Options

	closeOnce sync.Once
	stop      chan struct{}
	done      sync.WaitGroup
	closeErr  error

	// Recovered is how many runs Open marked interrupted; Truncated whether a
	// torn tail was cut off.
	Recovered int
	Truncated bool
}

const journalName = "journal.jsonl"

// Open opens (creating it if needed) the state directory dir, replays the
// journal, recovers interrupted runs and returns the store. Close it to flush.
// dir is required: choosing a default location is the caller's business
// (harness.OpenLocalStore does it).
func Open(dir string, opts ...Options) (*Store, error) {
	if dir == "" {
		return nil, errors.New("filestore: a state directory is required")
	}
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.SyncEvery <= 0 {
		o.SyncEvery = 500 * time.Millisecond
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	lock, err := lockDir(filepath.Join(dir, "LOCK"))
	if err != nil {
		return nil, err
	}
	s := &Store{Store: memstore.New(), dir: dir, lock: lock, opts: o, cache: map[string]uint64{}, msgs: map[string]struct{}{}, stop: make(chan struct{})}
	path := filepath.Join(dir, journalName)
	_ = os.Remove(path + ".tmp") // an interrupted compaction
	stats, err := s.replay(path)
	if err != nil {
		lock.Close()
		return nil, err
	}
	if s.j, err = openJournal(path, o.Sync == SyncAlways); err != nil {
		lock.Close()
		return nil, err
	}
	if err := s.recover(context.Background()); err != nil {
		_ = s.j.close()
		lock.Close()
		return nil, err
	}
	if stats.deleted > 0 || (stats.upserts > 1000 && stats.upserts > 2*stats.distinct) {
		if err := s.compact(); err != nil {
			_ = s.j.close()
			lock.Close()
			return nil, err
		}
	}
	if o.Sync == SyncInterval {
		s.done.Add(1)
		go s.syncLoop()
	}
	return s, nil
}

func (s *Store) syncLoop() {
	defer s.done.Done()
	t := time.NewTicker(s.opts.SyncEvery)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.jmu.Lock()
			j := s.j
			s.jmu.Unlock()
			_ = j.sync()
		}
	}
}

// Dir is the state directory.
func (s *Store) Dir() string { return s.dir }

// Close journals anything still unwritten, fsyncs and releases the directory.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		close(s.stop)
		s.done.Wait()
		err := s.syncAll()
		s.jmu.Lock()
		if cerr := s.j.close(); err == nil {
			err = cerr
		}
		s.jmu.Unlock()
		if lerr := s.lock.Close(); err == nil {
			err = lerr
		}
		s.closeErr = err
	})
	return s.closeErr
}

// ---- journaling ----

type threadRec struct {
	Thread    store.Thread
	Deleted   bool
	DeletedAt time.Time
}

type batch struct {
	s   *Store
	buf bytes.Buffer
	err error
}

func (b *batch) add(typ, key string, v any, dedupe bool) {
	if b.err != nil {
		return
	}
	raw, err := json.Marshal(v)
	if err != nil {
		b.err = fmt.Errorf("filestore: marshal %s: %w", typ, err)
		return
	}
	if dedupe {
		h := fnv.New64a()
		h.Write(raw)
		ck := typ + "\x00" + key
		if sum := h.Sum64(); b.s.cache[ck] == sum {
			return
		} else {
			b.s.cache[ck] = sum
		}
	}
	line, err := json.Marshal(record{T: typ, K: key, V: raw})
	if err != nil {
		b.err = err
		return
	}
	b.buf.Write(line)
	b.buf.WriteByte('\n')
}

// flush writes the batch; on failure the dedupe cache is dropped so the next
// call re-emits everything that may have been lost.
func (b *batch) flush() error {
	if b.err == nil {
		b.err = b.s.j.write(b.buf.Bytes())
	}
	if b.err != nil {
		b.s.cache = map[string]uint64{}
		b.s.msgs = map[string]struct{}{}
	}
	return b.err
}

func msgKey(thread, id string) string { return thread + "\x00" + id }

func (s *Store) addThread(b *batch, id string) {
	st, msgs, ok := s.Store.ExportThread(id)
	if !ok {
		return
	}
	b.add("thread", id, threadRec{Thread: st.Thread, Deleted: st.Deleted, DeletedAt: st.DeletedAt}, true)
	for i := range st.Runs {
		b.add("run", st.Runs[i].ID, st.Runs[i], true)
	}
	for run, sk := range st.RunSkills {
		b.add("skills", run, sk, true)
	}
	for i := range st.Approvals {
		b.add("approval", st.Approvals[i].ID, st.Approvals[i], true)
	}
	b.add("summary", id, st.Summary, true)
	b.add("goal", id, st.Goal, true)
	b.add("feedback", id, st.Feedback, true)
	b.add("superseded", id, st.Superseded, true)
	b.add("compactions", id, st.Compaction, true)
	for _, m := range msgs {
		k := msgKey(m.ThreadID, m.ID)
		if _, done := s.msgs[k]; !done {
			s.msgs[k] = struct{}{}
			b.add("msg", k, m, false)
		}
	}
}

// syncThreads journals what changed in the given threads.
func (s *Store) syncThreads(ids ...string) error {
	s.jmu.Lock()
	defer s.jmu.Unlock()
	b := &batch{s: s}
	seen := map[string]bool{}
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			s.addThread(b, id)
		}
	}
	return b.flush()
}

func (s *Store) syncAll() error { return s.syncThreads(s.Store.ThreadIDs()...) }

func (s *Store) syncRun(runID string) error {
	s.jmu.Lock()
	defer s.jmu.Unlock()
	b := &batch{s: s}
	if r, ok := s.Store.ExportRun(runID); ok {
		b.add("run", runID, r, true)
	}
	return b.flush()
}

// syncRunEnd journals a run row and the messages of its thread that no
// wrapper saw (side paths such as artifact notes): cheaper than a full thread
// sync, which re-hashes every run of the thread.
func (s *Store) syncRunEnd(runID string) error {
	threadID := s.runThread(runID)
	msgs := s.Store.ExportMessages(threadID)
	s.jmu.Lock()
	defer s.jmu.Unlock()
	b := &batch{s: s}
	if r, ok := s.Store.ExportRun(runID); ok {
		b.add("run", runID, r, true)
	}
	for _, m := range msgs {
		if k := msgKey(m.ThreadID, m.ID); !has(s.msgs, k) {
			s.msgs[k] = struct{}{}
			b.add("msg", k, m, false)
		}
	}
	return b.flush()
}

func has(m map[string]struct{}, k string) bool { _, ok := m[k]; return ok }

func (s *Store) runThread(runID string) string {
	if r, ok := s.Store.ExportRun(runID); ok {
		return r.ThreadID
	}
	return ""
}

func (s *Store) journalMessages(msgs ...*store.Message) error {
	s.jmu.Lock()
	defer s.jmu.Unlock()
	b := &batch{s: s}
	for _, m := range msgs {
		k := msgKey(m.ThreadID, m.ID)
		s.msgs[k] = struct{}{}
		b.add("msg", k, m, false)
	}
	return b.flush()
}

func (s *Store) journalEvents(runID string, evs []store.Event) error {
	s.jmu.Lock()
	defer s.jmu.Unlock()
	b := &batch{s: s}
	// Records stay bounded: 500 events per line.
	for len(evs) > 0 {
		n := min(500, len(evs))
		b.add("events", runID, evs[:n], false)
		evs = evs[n:]
	}
	return b.flush()
}

// ---- replay, recovery, compaction ----

type replayStats struct{ upserts, distinct, deleted int }

func (s *Store) replay(path string) (replayStats, error) {
	var (
		threads  = map[string]threadRec{}
		runs     = map[string]store.Run{}
		approvs  = map[string]store.Approval{}
		skills   = map[string][]string{}
		summary  = map[string]*store.ThreadSummary{}
		goals    = map[string]*store.Goal{}
		feedback = map[string]map[string]store.Feedback{}
		superse  = map[string][]string{}
		compacts = map[string][]store.Compaction{}
		msgs     = map[string]*store.Message{}
		events   = map[string][]store.Event{}
		memory   = map[string]*store.MemoryEntry{}
		memoff   = map[string]bool{}
		stats    replayStats
		seen     = map[string]bool{}
	)
	_, truncated, err := scan(path, func(r record) error {
		if r.T != "events" && r.T != "msg" && r.T != "mem" {
			stats.upserts++
			if k := r.T + "\x00" + r.K; !seen[k] {
				seen[k] = true
				stats.distinct++
			}
		}
		var err error
		switch r.T {
		case "thread":
			var v threadRec
			if err = json.Unmarshal(r.V, &v); err == nil {
				threads[r.K] = v
			}
		case "run":
			var v store.Run
			if err = json.Unmarshal(r.V, &v); err == nil {
				runs[r.K] = v
			}
		case "approval":
			var v store.Approval
			if err = json.Unmarshal(r.V, &v); err == nil {
				approvs[r.K] = v
			}
		case "skills":
			var v []string
			if err = json.Unmarshal(r.V, &v); err == nil {
				skills[r.K] = v
			}
		case "summary":
			var v *store.ThreadSummary
			if err = json.Unmarshal(r.V, &v); err == nil {
				summary[r.K] = v
			}
		case "goal":
			var v *store.Goal
			if err = json.Unmarshal(r.V, &v); err == nil {
				goals[r.K] = v
			}
		case "feedback":
			var v map[string]store.Feedback
			if err = json.Unmarshal(r.V, &v); err == nil {
				feedback[r.K] = v
			}
		case "superseded":
			var v []string
			if err = json.Unmarshal(r.V, &v); err == nil {
				superse[r.K] = v
			}
		case "compactions":
			var v []store.Compaction
			if err = json.Unmarshal(r.V, &v); err == nil {
				compacts[r.K] = v
			}
		case "msg":
			var v store.Message
			if err = json.Unmarshal(r.V, &v); err == nil {
				msgs[r.K] = &v
			}
		case "events":
			var v []store.Event
			if err = json.Unmarshal(r.V, &v); err == nil {
				events[r.K] = append(events[r.K], v...)
			}
		case "mem": // a memory entry; null is a tombstone
			var v *store.MemoryEntry
			if err = json.Unmarshal(r.V, &v); err == nil {
				if v == nil {
					delete(memory, r.K)
				} else {
					memory[r.K] = v
				}
			}
		case "memoff":
			var v bool
			if err = json.Unmarshal(r.V, &v); err == nil {
				memoff[r.K] = v
			}
		}
		if err != nil {
			return fmt.Errorf("%w: record %s/%s: %v", ErrCorrupt, r.T, r.K, err)
		}
		return nil
	})
	if err != nil {
		return stats, err
	}
	s.Truncated = truncated
	for id, tr := range threads {
		if tr.Deleted {
			stats.deleted++
		}
		st := memstore.ThreadState{Thread: tr.Thread, Deleted: tr.Deleted, DeletedAt: tr.DeletedAt,
			RunSkills: map[string][]string{}, Summary: summary[id], Goal: goals[id], Feedback: feedback[id],
			Superseded: superse[id], Compaction: compacts[id]}
		st.Thread.SandboxID = "" // sandboxes do not survive the process
		for _, r := range runs {
			if r.ThreadID == id {
				st.Runs = append(st.Runs, r)
				if sk, ok := skills[r.ID]; ok {
					st.RunSkills[r.ID] = sk
				}
			}
		}
		for _, a := range approvs {
			if a.ThreadID == id {
				st.Approvals = append(st.Approvals, a)
			}
		}
		s.Store.ImportThread(st)
	}
	all := make([]*store.Message, 0, len(msgs))
	for k, m := range msgs {
		all = append(all, m)
		s.msgs[k] = struct{}{}
	}
	s.Store.ImportMessages(all)
	for run, evs := range events {
		s.Store.ImportEvents(run, evs)
	}
	var memEntries []*store.MemoryEntry
	for _, e := range memory {
		memEntries = append(memEntries, e)
	}
	var memDisabled []string
	for l, off := range memoff {
		if off {
			memDisabled = append(memDisabled, l)
		}
	}
	s.Store.ImportMemory(memEntries, memDisabled)
	// Everything replayed is what the journal already holds: warm the dedupe
	// cache so the next sync does not rewrite it.
	s.jmu.Lock()
	defer s.jmu.Unlock()
	b := &batch{s: s}
	for _, id := range s.Store.ThreadIDs() {
		s.addThread(b, id)
	}
	b.buf.Reset() // only wanted the side effect on s.cache
	return stats, nil
}

// InterruptedMessage is the RUN_ERROR text written for runs killed with the process.
const InterruptedMessage = "The run was interrupted because the application stopped."

// recover marks the runs the previous process left queued or running as
// interrupted and terminates their event log.
func (s *Store) recover(ctx context.Context) error {
	log := eventlog.NewMemory(s)
	for _, id := range s.Store.ThreadIDs() {
		st, _, _ := s.Store.ExportThread(id)
		for _, r := range st.Runs {
			if store.IsTerminal(r.Status) {
				continue
			}
			if last, ok, err := s.Store.LastEvent(ctx, r.ID); err != nil {
				return err
			} else if !ok || !agui.IsTerminal(last.Type) {
				w, err := log.Resume(ctx, r.ID, r.LastSeq)
				if err != nil {
					return err
				}
				if w.LastSeq() == 0 {
					if _, err := w.Append(ctx, agui.RunStartedEvent(r.ThreadID, r.ID)); err != nil {
						return err
					}
				}
				if _, err := w.Append(ctx, agui.RunErrorEvent(agui.CodeInterrupted, InterruptedMessage)); err != nil {
					return err
				}
				if err := w.Close(ctx); err != nil {
					return err
				}
			}
			if err := s.SetRunOutcome(ctx, r.ID, store.StatusInterrupted, "interrupted", InterruptedMessage); err != nil {
				return err
			}
			s.Recovered++
		}
	}
	return s.syncAll()
}

// compact rewrites the journal from the live state (deleted threads dropped)
// into a temp file and atomically renames it over the journal.
func (s *Store) compact() error {
	path := filepath.Join(s.dir, journalName)
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	nj, err := openJournal(tmp, false)
	if err != nil {
		return err
	}
	s.jmu.Lock()
	defer s.jmu.Unlock()
	old := s.j
	s.j, s.cache, s.msgs = nj, map[string]uint64{}, map[string]struct{}{}
	fail := func(err error) error {
		_ = nj.close()
		_ = os.Remove(tmp)
		s.j = old
		s.cache, s.msgs = map[string]uint64{}, map[string]struct{}{} // forces a full re-emit later
		return err
	}
	b := &batch{s: s}
	entries, disabled := s.Store.ExportMemory()
	for _, e := range entries {
		b.add("mem", e.ID, e, true)
	}
	for _, l := range disabled {
		b.add("memoff", l, true, true)
	}
	for _, id := range s.Store.ThreadIDs() {
		st, _, ok := s.Store.ExportThread(id)
		if !ok || st.Deleted {
			continue
		}
		s.addThread(b, id)
		for _, r := range st.Runs {
			var after int64
			for {
				evs, err := s.Store.ListEvents(context.Background(), r.ID, after, 500)
				if err != nil {
					return fail(err)
				}
				if len(evs) == 0 {
					break
				}
				b.add("events", r.ID, evs, false)
				after = evs[len(evs)-1].Seq
			}
		}
		if b.buf.Len() > 4<<20 {
			if err := b.flush(); err != nil {
				return fail(err)
			}
			b.buf.Reset()
		}
	}
	if err := b.flush(); err != nil {
		return fail(err)
	}
	if err := nj.sync(); err != nil {
		return fail(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fail(err)
	}
	if d, err := os.Open(s.dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	nj.path = path
	nj.always = s.opts.Sync == SyncAlways
	_ = old.close()
	return nil
}
