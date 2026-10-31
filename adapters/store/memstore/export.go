package memstore

import (
	"sort"
	"time"

	"github.com/7ar-chat/harness/core/store"
)

// ThreadState is the durable part of one thread, without its messages and
// events (see ExportThread). It lets a persistent store (store/filestore)
// journal and restore a Store without knowing its internals.
type ThreadState struct {
	Thread     store.Thread
	Deleted    bool
	DeletedAt  time.Time
	Runs       []store.Run
	RunSkills  map[string][]string
	Approvals  []store.Approval
	Summary    *store.ThreadSummary
	Goal       *store.Goal
	Feedback   map[string]store.Feedback
	Superseded []string // ids of the messages hidden by an edit
	Compaction []store.Compaction
}

// ThreadIDs returns the ids of every thread, deleted ones included.
func (s *Store) ThreadIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.threads))
	for id := range s.threads {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// ExportRun returns a copy of a run.
func (s *Store) ExportRun(id string) (store.Run, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok {
		return store.Run{}, false
	}
	return *r, true
}

// ExportMessages returns copies of all messages of a thread, superseded ones included.
func (s *Store) ExportMessages(threadID string) []*store.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*store.Message
	for _, m := range s.messages {
		if m.ThreadID == threadID {
			cp := *m
			out = append(out, &cp)
		}
	}
	return out
}

// ExportThread returns the state of a thread and all its messages (superseded
// ones included, in position order).
func (s *Store) ExportThread(id string) (ThreadState, []*store.Message, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.h.init()
	t, ok := s.threads[id]
	if !ok {
		return ThreadState{}, nil, false
	}
	st := ThreadState{Thread: t.Thread, Deleted: t.deleted, DeletedAt: t.deletedAt, RunSkills: map[string][]string{}}
	st.Thread.ActiveRun, st.Thread.PendingApproval, st.Thread.Goal, st.Thread.LastRun, st.Thread.Unread = nil, nil, nil, nil, false
	for _, r := range s.runs {
		if r.ThreadID == id {
			st.Runs = append(st.Runs, *r)
		}
	}
	sort.Slice(st.Runs, func(i, j int) bool { return st.Runs[i].ID < st.Runs[j].ID })
	x := s.x()
	x.mu.Lock()
	for _, r := range st.Runs {
		if sk, ok := x.runSkills[r.ID]; ok {
			st.RunSkills[r.ID] = append([]string(nil), sk...)
		}
	}
	x.mu.Unlock()
	for _, a := range s.approvalsLocked() {
		if a.ThreadID == id {
			st.Approvals = append(st.Approvals, *copyApproval(a))
		}
	}
	sort.Slice(st.Approvals, func(i, j int) bool { return st.Approvals[i].ID < st.Approvals[j].ID })
	if sum, ok := s.h.summaries[id]; ok {
		st.Summary = &sum
	}
	st.Goal = copyGoal(s.h.goals[id])
	if fb := s.h.feedback[id]; len(fb) > 0 {
		st.Feedback = map[string]store.Feedback{}
		for k, v := range fb {
			st.Feedback[k] = v
		}
	}
	var msgs []*store.Message
	for _, m := range s.messages {
		if m.ThreadID != id {
			continue
		}
		cp := *m
		msgs = append(msgs, &cp)
		if s.h.superseded[msgKey(id, m.ID)] {
			st.Superseded = append(st.Superseded, m.ID)
		}
	}
	sort.Strings(st.Superseded)
	for _, c := range s.h.compactions {
		if c.ThreadID == id {
			st.Compaction = append(st.Compaction, c)
		}
	}
	return st, msgs, true
}

// ImportThread restores the state exported by ExportThread (replacing what the
// Store holds for that thread, except messages and events).
func (s *Store) ImportThread(st ThreadState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.h.init()
	id := st.Thread.ID
	s.threads[id] = &thread{Thread: st.Thread, deleted: st.Deleted, deletedAt: st.DeletedAt}
	for _, r := range st.Runs {
		r := r
		s.runs[r.ID] = &r
	}
	x := s.x()
	x.mu.Lock()
	for run, sk := range st.RunSkills {
		x.runSkills[run] = sk
	}
	x.mu.Unlock()
	for _, a := range st.Approvals {
		a := a
		s.approvalsLocked()[a.ID] = copyApproval(&a)
	}
	delete(s.h.summaries, id)
	if st.Summary != nil {
		s.h.summaries[id] = *st.Summary
	}
	if st.Goal != nil {
		s.h.goals[id] = copyGoal(st.Goal)
	} else {
		delete(s.h.goals, id)
	}
	s.h.feedback[id] = st.Feedback
	for _, m := range st.Superseded {
		s.h.superseded[msgKey(id, m)] = true
	}
	kept := s.h.compactions[:0]
	for _, c := range s.h.compactions {
		if c.ThreadID != id {
			kept = append(kept, c)
		}
	}
	s.h.compactions = append(kept, st.Compaction...)
}

// ImportMessages inserts messages as they are (id, position and time kept),
// ignoring the ones already present, and advances the position counter.
func (s *Store) ImportMessages(msgs []*store.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	have := map[string]bool{}
	for _, m := range s.messages {
		have[msgKey(m.ThreadID, m.ID)] = true
	}
	for _, m := range msgs {
		if have[msgKey(m.ThreadID, m.ID)] {
			continue
		}
		cp := *m
		s.messages = append(s.messages, &cp)
		if m.Position > s.pos {
			s.pos = m.Position
		}
	}
	sort.SliceStable(s.messages, func(i, j int) bool { return s.messages[i].Position < s.messages[j].Position })
}

// ImportEvents inserts persisted events of a run (duplicate seqs are ignored).
func (s *Store) ImportEvents(runID string, evs []store.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	have := map[int64]bool{}
	cur := s.events[runID]
	for _, e := range cur {
		have[e.Seq] = true
	}
	for _, e := range evs {
		if !have[e.Seq] {
			have[e.Seq] = true
			cur = append(cur, e)
		}
	}
	sort.Slice(cur, func(i, j int) bool { return cur[i].Seq < cur[j].Seq })
	s.events[runID] = cur
}
