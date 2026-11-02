package conformance

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/7ar-chat/harness/core/store"
)

// Artifacts runs the suite of store.ArtifactStore, store.BlobStore, store.SkillStore and store.WorkspaceGitStore:
// artifact versions and deletion, listing, the blob garbage collection (queue, grace period, purge of deleted threads),
// advisory locks, the artifact notes of runs and messages, run skills and the archived workspace Git history.
func Artifacts(t *testing.T, f Factory) {
	t.Helper()
	t.Run("Put", func(t *testing.T) { artSuitePut(t, f) })
	t.Run("Delete", func(t *testing.T) { artSuiteDelete(t, f) })
	t.Run("Materialized", func(t *testing.T) { artSuiteMaterialized(t, f) })
	t.Run("Listing", func(t *testing.T) { artSuiteListing(t, f) })
	t.Run("Blobs", func(t *testing.T) { artSuiteBlobs(t, f) })
	t.Run("BlobGC", func(t *testing.T) { artSuiteBlobGC(t, f) })
	t.Run("Purge", func(t *testing.T) { artSuitePurge(t, f) })
	t.Run("Lock", func(t *testing.T) { artSuiteLock(t, f) })
	t.Run("Clock", func(t *testing.T) { artSuiteClock(t, f) })
	t.Run("Skills", func(t *testing.T) { artSuiteSkills(t, f) })
	t.Run("RunArtifacts", func(t *testing.T) { artSuiteRunArtifacts(t, f) })
	t.Run("ToolVerification", func(t *testing.T) { artSuiteToolVerification(t, f) })
	t.Run("WorkspaceGit", func(t *testing.T) { artSuiteWorkspaceGit(t, f) })
	t.Run("WorkspaceGitIssue", func(t *testing.T) { artSuiteWorkspaceGitIssue(t, f) })
	t.Run("Sanitize", func(t *testing.T) { artSuiteSanitize(t, f) })
}

// ---- helpers (prefix art*, shared by the artifacts_*.go files) ----

func artThread(t *testing.T, s Store) *store.Thread {
	t.Helper()
	th, err := s.CreateThread(context.Background(), "alice", "t")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	return th
}

// artRun queues a run (with a user message) on the thread.
func artRun(t *testing.T, s Store, threadID string) *store.Run {
	t.Helper()
	run, _, err := s.CreateRun(context.Background(), store.NewRun{RunID: uuid.NewString(), ThreadID: threadID, UserLogin: "alice",
		Model: "m", Message: store.Message{ID: uuid.NewString(), Role: store.RoleUser, Content: "go"}})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return run
}

// artIn is a PutArtifactVersion input for /workspace/<name> with the given content hash.
func artIn(threadID, name, sha string) store.PutArtifact {
	return store.PutArtifact{ThreadID: threadID, UserLogin: "alice", Path: "/workspace/" + name, Name: name, MimeType: "text/plain",
		Kind: store.ArtifactOutput, SizeBytes: int64(len(sha)), SHA256: sha}
}

func artPut(t *testing.T, s Store, in store.PutArtifact) (*store.Artifact, string) {
	t.Helper()
	a, change, err := s.PutArtifactVersion(context.Background(), in)
	if err != nil {
		t.Fatalf("PutArtifactVersion(%s, %s): %v", in.Path, in.SHA256, err)
	}
	return a, change
}

func artGet(t *testing.T, s Store, id string) *store.Artifact {
	t.Helper()
	a, err := s.GetArtifact(context.Background(), id)
	if err != nil {
		t.Fatalf("GetArtifact(%s): %v", id, err)
	}
	return a
}

// artSince is the "listing started" instant of MarkArtifactDeleted for a test that advanced its clock by at least an
// hour: everything stored before the advance is unchanged since then, everything written after it is not.
func artSince(t *testing.T, s Store) time.Time {
	t.Helper()
	now, err := s.Clock(context.Background())
	if err != nil {
		t.Fatalf("Clock: %v", err)
	}
	return now.Add(-30 * time.Minute)
}

func artMust(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func artDeleteThread(t *testing.T, s Store, id string) {
	t.Helper()
	if _, err := s.DeleteThread(context.Background(), "alice", id); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}
}

func artStrings(ss []string) string { return "[" + strings.Join(ss, " ") + "]" }

func artSameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
