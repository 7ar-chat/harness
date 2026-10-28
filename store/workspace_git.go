package store

import (
	"context"
	"time"
)

// WorkspaceGitArchive is the archived local Git history (/workspace/.git as a
// tar.gz blob) of a thread, restored into every new sandbox of the thread.
type WorkspaceGitArchive struct {
	ThreadID string
	// BlobKey is the blob holding the archive ("workspace-git/<thread>/<uuid>.tar.gz").
	BlobKey string
	// Head is the HEAD commit archived.
	Head      string
	SizeBytes int64
	UpdatedAt time.Time
}

// Reasons of a WorkspaceGitIssue.
const (
	// GitIssueTooLarge: the history exceeded the archive cap and was not archived.
	GitIssueTooLarge = "too_large"
	// GitIssueFailed: archiving failed (timeout, blob store error…).
	GitIssueFailed = "failed"
)

// WorkspaceGitIssue records that the thread's latest history was not archived
// (the stored archive, if any, is older than the sandbox's history).
type WorkspaceGitIssue struct {
	ThreadID string
	Reason   string
	// Head is the HEAD that could not be archived; SizeBytes the refused archive size (too_large).
	Head      string
	SizeBytes int64
	UpdatedAt time.Time
}

// WorkspaceGitStore persists the thread's archived Git history. The archive
// blobs are never referenced by an artifact version: a replaced archive and
// the archive of a purged thread are queued for the blob garbage collection
// (work_blob_gc, collected after WORK_BLOB_GC_GRACE_HOURS).
type WorkspaceGitStore interface {
	// WorkspaceGit returns the thread's archive (ErrNotFound when none).
	WorkspaceGit(ctx context.Context, threadID string) (*WorkspaceGitArchive, error)
	// PutWorkspaceGit records the thread's new archive; the previous one's blob
	// (when its key differs) is queued for garbage collection, last referenced now.
	PutWorkspaceGit(ctx context.Context, a WorkspaceGitArchive) error
	// WorkspaceGitIssue returns why the latest history is not archived (ErrNotFound: it is).
	WorkspaceGitIssue(ctx context.Context, threadID string) (*WorkspaceGitIssue, error)
	// SetWorkspaceGitIssue records a failed archive (a thread that no longer exists is ignored).
	SetWorkspaceGitIssue(ctx context.Context, i WorkspaceGitIssue) error
	// ClearWorkspaceGitIssue forgets it (the history was archived).
	ClearWorkspaceGitIssue(ctx context.Context, threadID string) error
}
