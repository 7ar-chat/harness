package memstore

import (
	"context"
	"sort"
	"time"
)

// PurgeDeletedThreads implements store.ArtifactStore: like the Postgres
// cascade, everything of the thread goes — runs and their
// approvals — while idea usage keeps its rows without the thread (ON DELETE SET NULL); memories are per user and stay.
func (s *Store) PurgeDeletedThreads(_ context.Context, olderThan time.Duration, limit int) (int, error) {
	n, _, _ := s.purgeThreads(olderThan, limit)
	return n, nil
}

func (s *Store) purgeThreads(olderThan time.Duration, limit int) (int, map[string]bool, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.h.init()
	cutoff := s.now().Add(-olderThan)
	var ids []string
	for id, t := range s.threads {
		if t.deleted && t.deletedAt.Before(cutoff) {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return s.threads[ids[i]].deletedAt.Before(s.threads[ids[j]].deletedAt) })
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	purge := map[string]bool{}
	for _, id := range ids {
		purge[id] = true
		delete(s.threads, id)
		delete(s.h.summaries, id)
		delete(s.h.goals, id)
		delete(s.h.feedback, id)
	}
	var runIDs []string
	for id, r := range s.runs {
		if purge[r.ThreadID] {
			runIDs = append(runIDs, id)
			delete(s.runs, id)
			delete(s.events, id)
		}
	}
	for id, a := range s.approvals {
		if purge[a.ThreadID] {
			delete(s.approvals, id)
		}
	}
	for _, u := range s.ideaUsage {
		if purge[u.ThreadID] {
			u.ThreadID = ""
		}
	}
	compactions := s.h.compactions[:0]
	for _, c := range s.h.compactions {
		if !purge[c.ThreadID] {
			compactions = append(compactions, c)
		}
	}
	s.h.compactions = compactions
	kept := s.messages[:0]
	for _, m := range s.messages {
		if purge[m.ThreadID] {
			delete(s.h.superseded, msgKey(m.ThreadID, m.ID))
			continue
		}
		kept = append(kept, m)
	}
	s.messages = kept
	x := s.x()
	x.mu.Lock()
	defer x.mu.Unlock()
	now := s.now()
	for id, a := range x.artifacts {
		if !purge[a.ThreadID] {
			continue
		}
		for _, v := range x.versions[id] {
			x.blobGC[v.SHA256] = now // last referenced now: the grace period starts
		}
		delete(x.versions, id)
		delete(x.artifacts, id)
	}
	x.purgeWorkspaceGitLocked(purge, now)
	return len(ids), purge, runIDs
}

func (x *extras) referencedLocked(key string) bool {
	for _, a := range x.wsGit {
		if a.BlobKey == key {
			return true // the current workspace Git archive of a thread
		}
	}
	for _, vs := range x.versions {
		for _, v := range vs {
			if v.SHA256 == key {
				return true
			}
		}
	}
	return false
}

// collectableLocked is the collection condition (x.mu held).
func (x *extras) collectableLocked(key string, cutoff time.Time) bool {
	last, ok := x.blobGC[key]
	return ok && last.Before(cutoff) && !x.referencedLocked(key)
}

// TouchBlob implements store.ArtifactStore (gcMu stands for the Postgres row
// lock of an in-flight collection).
func (s *Store) TouchBlob(_ context.Context, key string) error {
	x := s.x()
	x.gcMu.Lock()
	defer x.gcMu.Unlock()
	x.mu.Lock()
	defer x.mu.Unlock()
	if _, ok := x.blobGC[key]; ok {
		x.blobGC[key] = s.now()
	}
	return nil
}

// QueueBlob implements store.ArtifactStore.
func (s *Store) QueueBlob(_ context.Context, key string) error {
	x := s.x()
	x.gcMu.Lock()
	defer x.gcMu.Unlock()
	x.mu.Lock()
	defer x.mu.Unlock()
	x.blobGC[key] = s.now()
	return nil
}

// BlobCandidates implements store.ArtifactStore.
func (s *Store) BlobCandidates(_ context.Context, grace time.Duration, limit int) ([]string, error) {
	x := s.x()
	x.mu.Lock()
	defer x.mu.Unlock()
	cutoff := s.now().Add(-grace)
	var out []string
	for key, last := range x.blobGC {
		if !last.Before(cutoff) {
			continue
		}
		if x.referencedLocked(key) {
			delete(x.blobGC, key)
			continue
		}
		out = append(out, key)
	}
	sort.Slice(out, func(i, j int) bool {
		if a, b := x.blobGC[out[i]], x.blobGC[out[j]]; !a.Equal(b) {
			return a.Before(b)
		}
		return out[i] < out[j]
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// CollectBlob implements store.ArtifactStore.
func (s *Store) CollectBlob(ctx context.Context, key string, grace time.Duration, del func(context.Context) error) (bool, error) {
	x := s.x()
	x.gcMu.Lock()
	defer x.gcMu.Unlock()
	x.mu.Lock()
	if !x.collectableLocked(key, s.now().Add(-grace)) {
		x.mu.Unlock()
		return false, nil
	}
	last := x.blobGC[key]
	delete(x.blobGC, key)
	x.mu.Unlock()
	if err := del(ctx); err != nil {
		x.mu.Lock()
		x.blobGC[key] = last // rollback
		x.mu.Unlock()
		return false, err
	}
	return true, nil
}

// CollectBlobs implements store.BlobStore.
func (s *Store) CollectBlobs(_ context.Context, keys []string, grace time.Duration) ([]string, error) {
	x := s.x()
	x.gcMu.Lock()
	defer x.gcMu.Unlock()
	x.mu.Lock()
	defer x.mu.Unlock()
	cutoff := s.now().Add(-grace)
	var out []string
	for _, k := range keys {
		if !x.collectableLocked(k, cutoff) {
			continue
		}
		delete(x.blobGC, k)
		delete(x.blobs, k)
		delete(x.blobs, "preview/"+k+".pdf")
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// SetThreadDeletedAt ages a soft-deleted thread (tests).
func (s *Store) SetThreadDeletedAt(id string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.threads[id]; ok && t.deleted {
		t.deletedAt = at
	}
}

// Blob reports whether a blob is stored (tests).
func (s *Store) Blob(key string) bool {
	x := s.x()
	x.mu.Lock()
	defer x.mu.Unlock()
	_, ok := x.blobs[key]
	return ok
}
