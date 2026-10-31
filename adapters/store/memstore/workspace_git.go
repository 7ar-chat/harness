package memstore

import (
	"context"
	"time"

	"github.com/7ar-chat/harness/core/store"
)

var _ store.WorkspaceGitStore = (*Store)(nil)

// WorkspaceGit implements store.WorkspaceGitStore.
func (s *Store) WorkspaceGit(_ context.Context, threadID string) (*store.WorkspaceGitArchive, error) {
	x := s.x()
	x.mu.Lock()
	defer x.mu.Unlock()
	a, ok := x.wsGit[threadID]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &a, nil
}

// PutWorkspaceGit implements store.WorkspaceGitStore (like the 0007 trigger,
// a replaced archive blob is queued for garbage collection).
func (s *Store) PutWorkspaceGit(_ context.Context, a store.WorkspaceGitArchive) error {
	if !s.threadExists(a.ThreadID) {
		return store.ErrNotFound
	}
	x := s.x()
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.wsGit == nil {
		x.wsGit = map[string]store.WorkspaceGitArchive{}
	}
	now := s.now()
	if prev, ok := x.wsGit[a.ThreadID]; ok && prev.BlobKey != a.BlobKey {
		x.blobGC[prev.BlobKey] = now
	}
	delete(x.blobGC, a.BlobKey)
	a.UpdatedAt = now
	x.wsGit[a.ThreadID] = a
	return nil
}

// purgeWorkspaceGitLocked drops the archives (and archive issues) of purged
// threads, queueing their blobs for garbage collection (x.mu held), like the
// cascade + 0007 trigger.
func (x *extras) purgeWorkspaceGitLocked(purged map[string]bool, now time.Time) {
	for id, a := range x.wsGit {
		if purged[id] {
			x.blobGC[a.BlobKey] = now
			delete(x.wsGit, id)
		}
	}
	for id := range x.wsGitIssues {
		if purged[id] {
			delete(x.wsGitIssues, id)
		}
	}
}

// WorkspaceGitIssue implements store.WorkspaceGitStore.
func (s *Store) WorkspaceGitIssue(_ context.Context, threadID string) (*store.WorkspaceGitIssue, error) {
	x := s.x()
	x.mu.Lock()
	defer x.mu.Unlock()
	i, ok := x.wsGitIssues[threadID]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &i, nil
}

// SetWorkspaceGitIssue implements store.WorkspaceGitStore.
func (s *Store) SetWorkspaceGitIssue(_ context.Context, i store.WorkspaceGitIssue) error {
	if !s.threadExists(i.ThreadID) {
		return nil
	}
	x := s.x()
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.wsGitIssues == nil {
		x.wsGitIssues = map[string]store.WorkspaceGitIssue{}
	}
	i.UpdatedAt = s.now()
	x.wsGitIssues[i.ThreadID] = i
	return nil
}

// ClearWorkspaceGitIssue implements store.WorkspaceGitStore.
func (s *Store) ClearWorkspaceGitIssue(_ context.Context, threadID string) error {
	x := s.x()
	x.mu.Lock()
	defer x.mu.Unlock()
	delete(x.wsGitIssues, threadID)
	return nil
}

func (s *Store) threadExists(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.threads[id]
	return ok
}
