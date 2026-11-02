package memstore

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/7ar-chat/harness/core/store"
)

// Artifacts, blobs and skills, mimicking the Postgres constraints of 0003:
// unique (thread_id, path), unique (user_login, name), kind check, cascade on
// thread delete is not needed (threads are soft-deleted; GetArtifact hides them).
type extras struct {
	mu        sync.Mutex
	artifacts map[string]*store.Artifact
	versions  map[string][]store.ArtifactVersion
	blobs     map[string][]byte
	runSkills map[string][]string
	// blobGC is the garbage collection queue: key → last reference.
	blobGC map[string]time.Time
	// gcMu is held by a collection in flight (the Postgres row lock).
	gcMu  sync.Mutex
	locks map[string]chan struct{}
	// wsGit is work_workspace_git: thread → archived Git history (workspace_git.go).
	wsGit map[string]store.WorkspaceGitArchive
	// wsGitIssues is work_workspace_git_issues: why the latest history is not archived.
	wsGitIssues map[string]store.WorkspaceGitIssue
}

func (s *Store) x() *extras {
	s.extrasOnce.Do(func() {
		s.ext = &extras{
			artifacts: map[string]*store.Artifact{}, versions: map[string][]store.ArtifactVersion{},
			blobs: map[string][]byte{}, runSkills: map[string][]string{}, blobGC: map[string]time.Time{}, locks: map[string]chan struct{}{},
		}
	})
	return s.ext
}

func copyArtifact(a *store.Artifact) *store.Artifact {
	cp := *a
	if a.RestoreSkippedAt != nil {
		t := *a.RestoreSkippedAt
		cp.RestoreSkippedAt = &t
	}
	return &cp
}

func (s *Store) threadAlive(threadID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.threads[threadID]
	return ok && !t.deleted
}

func (s *Store) runExists(runID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.runs[runID]
	return ok
}

// recordRunSkills is called by CreateRun (work_runs.skills).
func (s *Store) recordRunSkills(runID string, skills []string) {
	if len(skills) == 0 {
		return
	}
	clean := make([]string, len(skills))
	for i, sk := range skills {
		clean[i] = store.CleanText(sk) // like Postgres: NUL and invalid UTF-8 are replaced
	}
	x := s.x()
	x.mu.Lock()
	x.runSkills[runID] = clean
	x.mu.Unlock()
}

// RunSkills implements store.SkillStore.
func (s *Store) RunSkills(_ context.Context, runID string) ([]string, error) {
	if !s.runExists(runID) {
		return nil, store.ErrNotFound
	}
	x := s.x()
	x.mu.Lock()
	defer x.mu.Unlock()
	return append([]string{}, x.runSkills[runID]...), nil
}

func validKind(k string) bool { return k == store.ArtifactOutput || k == store.ArtifactUpload }

// PutArtifactVersion implements store.ArtifactStore.
func (s *Store) PutArtifactVersion(_ context.Context, in store.PutArtifact) (*store.Artifact, string, error) {
	// Like Postgres: NUL and invalid UTF-8 are replaced, never refused.
	in.Path, in.Name, in.MimeType, in.SandboxID = store.CleanText(in.Path), store.CleanText(in.Name), store.CleanText(in.MimeType), store.CleanText(in.SandboxID)
	if !s.threadExists(in.ThreadID) {
		return nil, "", store.ErrNotFound // FK violation in Postgres (a soft-deleted thread keeps its row)
	}
	runID := in.RunID
	if runID != "" && !s.runExists(runID) {
		runID = ""
	}
	x := s.x()
	x.mu.Lock()
	defer x.mu.Unlock()
	now := s.now()
	var cur *store.Artifact
	for _, a := range x.artifacts {
		if a.ThreadID == in.ThreadID && a.Path == in.Path {
			cur = a
		}
	}
	change := ""
	switch {
	case cur == nil:
		if !validKind(in.Kind) {
			return nil, "", store.ErrInvalidData
		}
		cur = &store.Artifact{ID: uuid.NewString(), ThreadID: in.ThreadID, UserLogin: in.UserLogin, Path: in.Path, Name: in.Name,
			MimeType: in.MimeType, Kind: in.Kind, SizeBytes: in.SizeBytes, SHA256: in.SHA256, Version: 1, RunID: runID,
			SourceMtime: in.Mtime, SandboxID: in.SandboxID, CreatedAt: now, UpdatedAt: now}
		x.artifacts[cur.ID] = cur
		change = store.ChangeCreated
	case cur.SHA256 == in.SHA256 && !cur.Deleted && !in.Force:
		cur.SourceMtime = in.Mtime
		if in.SandboxID != "" {
			cur.SandboxID, cur.RestoreSkippedAt = in.SandboxID, nil
		}
	default:
		cur.Version++
		cur.SizeBytes, cur.SHA256, cur.RunID, cur.SourceMtime, cur.MimeType = in.SizeBytes, in.SHA256, runID, in.Mtime, in.MimeType
		cur.Deleted, cur.UpdatedAt = false, now
		cur.SandboxID, cur.RestoreSkippedAt = in.SandboxID, nil
		change = store.ChangeUpdated
	}
	if change != "" {
		x.versions[cur.ID] = append(x.versions[cur.ID], store.ArtifactVersion{Version: cur.Version, SizeBytes: cur.SizeBytes,
			SHA256: cur.SHA256, RunID: runID, CreatedAt: now})
		if _, queued := x.blobGC[cur.SHA256]; queued {
			x.blobGC[cur.SHA256] = now // a new reference refreshes the last reference
		}
	}
	return copyArtifact(cur), change, nil
}

// MarkArtifactDeleted implements store.ArtifactStore.
func (s *Store) MarkArtifactDeleted(_ context.Context, artifactID, runID, sandboxID string, unchangedSince time.Time) (*store.Artifact, error) {
	if runID != "" && !s.runExists(runID) {
		runID = ""
	}
	x := s.x()
	x.mu.Lock()
	defer x.mu.Unlock()
	a, ok := x.artifacts[artifactID]
	if !ok {
		return nil, store.ErrConflict // like Postgres: the conditional UPDATE matches no row
	}
	if a.Deleted || a.SandboxID == "" || a.SandboxID != sandboxID || !a.UpdatedAt.Before(unchangedSince) {
		return nil, store.ErrConflict
	}
	a.Deleted = true
	if runID != "" {
		a.RunID = runID
	}
	a.UpdatedAt = s.now()
	return copyArtifact(a), nil
}

// MarkArtifactsMaterialized implements store.ArtifactStore.
func (s *Store) MarkArtifactsMaterialized(_ context.Context, sandboxID string, artifactIDs []string) error {
	if sandboxID == "" {
		return nil
	}
	sandboxID = store.CleanText(sandboxID) // like Postgres
	x := s.x()
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, id := range artifactIDs {
		if a, ok := x.artifacts[id]; ok {
			a.SandboxID, a.RestoreSkippedAt = sandboxID, nil
		}
	}
	return nil
}

// MarkArtifactsRestoreSkipped implements store.ArtifactStore.
func (s *Store) MarkArtifactsRestoreSkipped(_ context.Context, artifactIDs []string) error {
	x := s.x()
	x.mu.Lock()
	defer x.mu.Unlock()
	now := s.now()
	for _, id := range artifactIDs {
		if a, ok := x.artifacts[id]; ok && a.RestoreSkippedAt == nil {
			t := now
			a.RestoreSkippedAt = &t
		}
	}
	return nil
}

// Clock implements store.ArtifactStore.
func (s *Store) Clock(context.Context) (time.Time, error) { return s.now(), nil }

// AdvisoryLock implements store.ArtifactStore (process-wide, like the
// cluster-wide Postgres lock for every Service sharing this store).
func (s *Store) AdvisoryLock(ctx context.Context, name string, wait time.Duration) (func(), error) {
	x := s.x()
	x.mu.Lock()
	ch := x.locks[name]
	if ch == nil {
		ch = make(chan struct{}, 1)
		x.locks[name] = ch
	}
	x.mu.Unlock()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, store.ErrLockTimeout
	}
}

// ListArtifacts implements store.ArtifactStore (not deleted first, newest first).
func (s *Store) ListArtifacts(_ context.Context, threadID string) ([]*store.Artifact, error) {
	x := s.x()
	x.mu.Lock()
	defer x.mu.Unlock()
	var out []*store.Artifact
	for _, a := range x.artifacts {
		if a.ThreadID == threadID {
			out = append(out, copyArtifact(a))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Deleted != out[j].Deleted {
			return !out[i].Deleted
		}
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

// GetArtifact implements store.ArtifactStore.
func (s *Store) GetArtifact(_ context.Context, id string) (*store.Artifact, error) {
	x := s.x()
	x.mu.Lock()
	a, ok := x.artifacts[id]
	var cp *store.Artifact
	if ok {
		cp = copyArtifact(a)
	}
	x.mu.Unlock()
	if !ok || !s.threadAlive(cp.ThreadID) {
		return nil, store.ErrNotFound
	}
	return cp, nil
}

// ListArtifactVersions implements store.ArtifactStore (newest first).
func (s *Store) ListArtifactVersions(_ context.Context, artifactID string) ([]store.ArtifactVersion, error) {
	x := s.x()
	x.mu.Lock()
	defer x.mu.Unlock()
	vs := x.versions[artifactID]
	out := make([]store.ArtifactVersion, 0, len(vs))
	for i := len(vs) - 1; i >= 0; i-- {
		out = append(out, vs[i])
	}
	return out, nil
}

// GetArtifactVersion implements store.ArtifactStore.
func (s *Store) GetArtifactVersion(_ context.Context, artifactID string, version int) (*store.ArtifactVersion, error) {
	x := s.x()
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, v := range x.versions[artifactID] {
		if v.Version == version {
			cp := v
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}

// PutBlob implements store.BlobStore.
func (s *Store) PutBlob(_ context.Context, key string, data []byte) error {
	x := s.x()
	x.mu.Lock()
	defer x.mu.Unlock()
	if _, ok := x.blobs[key]; !ok {
		x.blobs[key] = append([]byte(nil), data...)
	}
	return nil
}

// GetBlob implements store.BlobStore.
func (s *Store) GetBlob(_ context.Context, key string) ([]byte, error) {
	x := s.x()
	x.mu.Lock()
	defer x.mu.Unlock()
	b, ok := x.blobs[key]
	if !ok {
		return nil, store.ErrNotFound
	}
	return append([]byte(nil), b...), nil
}

// HasBlob implements store.BlobStore.
func (s *Store) HasBlob(_ context.Context, key string) (bool, error) {
	x := s.x()
	x.mu.Lock()
	defer x.mu.Unlock()
	_, ok := x.blobs[key]
	return ok, nil
}

// DeleteBlob implements store.BlobStore.
func (s *Store) DeleteBlob(_ context.Context, key string) error {
	x := s.x()
	x.mu.Lock()
	defer x.mu.Unlock()
	delete(x.blobs, key)
	return nil
}

// AppendRunArtifacts implements store.ArtifactStore.
func (s *Store) AppendRunArtifacts(_ context.Context, runID string, items []any) error {
	if len(items) == 0 {
		return nil
	}
	items = store.CleanValue(items).([]any) // like Postgres: NUL and invalid UTF-8 are replaced
	s.mu.Lock()
	defer s.mu.Unlock()
	s.h.init()
	// Round-trip through JSON like a jsonb column.
	merge := func(prev []any) []any {
		b, _ := json.Marshal(append(append([]any{}, prev...), items...))
		var merged []any
		_ = json.Unmarshal(b, &merged)
		return merged
	}
	for i := len(s.messages) - 1; i >= 0; i-- {
		m := s.messages[i]
		if m.RunID != runID || m.Role != store.RoleAssistant || s.h.superseded[msgKey(m.ThreadID, m.ID)] {
			continue
		}
		md := map[string]any{}
		for k, v := range m.Metadata {
			md[k] = v
		}
		prev, _ := md["artifacts"].([]any)
		md["artifacts"] = merge(prev)
		m.Metadata = md
		return nil
	}
	r, ok := s.runs[runID]
	if !ok {
		return nil
	}
	if t, alive := s.threads[r.ThreadID]; !alive || t.deleted {
		return nil
	}
	s.insertLocked(&store.Message{ThreadID: r.ThreadID, RunID: runID, Role: store.RoleAssistant, Metadata: map[string]any{"artifacts": merge(nil)}})
	return nil
}

// AppendToolVerification implements store.HarnessStore.
func (s *Store) AppendToolVerification(_ context.Context, threadID, toolCallID string, items []any) error {
	if toolCallID == "" || len(items) == 0 {
		return nil
	}
	items = store.CleanValue(items).([]any) // like Postgres: NUL and invalid UTF-8 are replaced
	s.mu.Lock()
	defer s.mu.Unlock()
	s.h.init()
	for i := len(s.messages) - 1; i >= 0; i-- {
		m := s.messages[i]
		if m.ThreadID != threadID || m.ToolCallID != toolCallID || m.Role != store.RoleTool || s.h.superseded[msgKey(m.ThreadID, m.ID)] {
			continue
		}
		md := map[string]any{}
		for k, v := range m.Metadata {
			md[k] = v
		}
		prev, _ := md["verification"].([]any)
		b, _ := json.Marshal(append(append([]any{}, prev...), items...)) // like a jsonb column
		var merged []any
		_ = json.Unmarshal(b, &merged)
		md["verification"] = merged
		m.Metadata = md
		return nil
	}
	return nil
}
