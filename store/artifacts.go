package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ErrLockTimeout means an advisory lock could not be taken in time.
var ErrLockTimeout = errors.New("lock wait timed out")

// Artifact kinds.
const (
	ArtifactOutput = "output"
	ArtifactUpload = "upload"
)

// Artifact changes reported by PutArtifactVersion / MarkArtifactDeleted.
const (
	ChangeCreated = "created"
	ChangeUpdated = "updated"
	ChangeDeleted = "deleted"
)

// Artifact is a durable file of a thread's /workspace (latest version denormalized).
type Artifact struct {
	ID        string
	ThreadID  string
	UserLogin string
	Path      string
	Name      string
	MimeType  string
	Kind      string
	SizeBytes int64
	SHA256    string
	Version   int
	RunID     string
	// SourceMtime is the sandbox mtime (unix seconds) observed for the latest version.
	SourceMtime float64
	Deleted     bool
	// SandboxID is the sandbox in which the latest version is known to be
	// present (seen in a listing or written); "" when none yet. Only artifacts
	// materialized in the snapshotted sandbox can be found deleted there.
	SandboxID string
	// RestoreSkippedAt is set when a workspace restore could not write the
	// artifact (limits, read/write failure); such files go first next time.
	RestoreSkippedAt *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// ArtifactVersion is one stored version of an artifact.
type ArtifactVersion struct {
	Version   int
	SizeBytes int64
	SHA256    string
	RunID     string
	CreatedAt time.Time
}

// PutArtifact is the input of PutArtifactVersion.
type PutArtifact struct {
	ThreadID  string
	UserLogin string
	Path      string
	Name      string
	MimeType  string
	// Kind is used when the artifact is created; an existing artifact keeps its kind.
	Kind      string
	SizeBytes int64
	SHA256    string
	RunID     string
	Mtime     float64
	// Force records a new version even when the content equals the latest one (restore).
	Force bool
	// SandboxID, when set, records that the content is present in that sandbox.
	SandboxID string
}

// ArtifactStore persists artifact metadata (content lives in a BlobStore).
type ArtifactStore interface {
	// PutArtifactVersion creates the artifact (ChangeCreated), adds a version when
	// the content differs, Force is set or it was deleted (ChangeUpdated), or only
	// refreshes the observed mtime ("").
	PutArtifactVersion(ctx context.Context, in PutArtifact) (*Artifact, string, error)
	// MarkArtifactDeleted flags the artifact as removed from /workspace of
	// sandboxID: only when its latest version is materialized in that sandbox
	// and was not changed since unchangedSince (ErrConflict otherwise).
	MarkArtifactDeleted(ctx context.Context, artifactID, runID, sandboxID string, unchangedSince time.Time) (*Artifact, error)
	// MarkArtifactsMaterialized records that the latest versions are present in the sandbox.
	MarkArtifactsMaterialized(ctx context.Context, sandboxID string, artifactIDs []string) error
	// MarkArtifactsRestoreSkipped records that a workspace restore could not write them.
	MarkArtifactsRestoreSkipped(ctx context.Context, artifactIDs []string) error
	// Clock is the database time (the reference of updated_at).
	Clock(ctx context.Context) (time.Time, error)
	// AdvisoryLock takes a cluster-wide lock (shared by every api and runtime
	// process) waiting at most wait (ErrLockTimeout); call the func to release it.
	AdvisoryLock(ctx context.Context, name string, wait time.Duration) (func(), error)
	// PurgeDeletedThreads hard-deletes up to limit threads soft-deleted more
	// than olderThan ago (runs, messages, events, artifacts, versions, goals,
	// feedback) and queues their content blobs for garbage collection, last
	// referenced now. A new artifact version referencing a queued blob
	// refreshes its last reference in the same statement (PutArtifactVersion).
	PurgeDeletedThreads(ctx context.Context, olderThan time.Duration, limit int) (int, error)
	// TouchBlob refreshes the last reference of a queued blob; call it before
	// checking that content to reuse is stored. It waits for an in-flight
	// collection of the blob (CollectBlob), so the check that follows sees the
	// content gone and stores it again.
	TouchBlob(ctx context.Context, key string) error
	// QueueBlob queues key for garbage collection, last referenced now (an
	// existing queue row is refreshed). Call it before storing a blob that no
	// artifact version may ever reference (an orphan after a failure, a cached
	// preview): the GC deletes it once unreferenced for the grace period.
	QueueBlob(ctx context.Context, key string) error
	// BlobCandidates returns up to limit queued blob keys that no artifact
	// version references and that were last referenced more than grace ago
	// (queued keys that are referenced again leave the queue).
	BlobCandidates(ctx context.Context, grace time.Duration, limit int) ([]string, error)
	// CollectBlob removes key from the queue only when it is still unreferenced
	// and was last referenced more than grace ago, then calls del (deleting the
	// content from an external backend) before committing: the queue row stays
	// locked until del returns and is kept when del fails. It reports whether
	// the blob was collected.
	CollectBlob(ctx context.Context, key string, grace time.Duration, del func(context.Context) error) (bool, error)
	// ListArtifacts returns every artifact of a thread (deleted included).
	ListArtifacts(ctx context.Context, threadID string) ([]*Artifact, error)
	GetArtifact(ctx context.Context, id string) (*Artifact, error)
	ListArtifactVersions(ctx context.Context, artifactID string) ([]ArtifactVersion, error)
	GetArtifactVersion(ctx context.Context, artifactID string, version int) (*ArtifactVersion, error)
	// AppendRunArtifacts appends items to metadata.artifacts of the run's last
	// assistant message; a run without one gets an empty assistant message
	// (content "") carrying them.
	AppendRunArtifacts(ctx context.Context, runID string, items []any) error
}

// BlobStore is the Postgres blob backend (work_blobs).
type BlobStore interface {
	// PutBlob stores data under key; an existing key is kept (content-addressed).
	PutBlob(ctx context.Context, key string, data []byte) error
	// GetBlob returns ErrNotFound for an unknown key.
	GetBlob(ctx context.Context, key string) ([]byte, error)
	HasBlob(ctx context.Context, key string) (bool, error)
	DeleteBlob(ctx context.Context, key string) error
	// CollectBlobs deletes in one statement, among keys, the queued blobs that
	// are still unreferenced and were last referenced more than grace ago,
	// with their queue row and cached preview ("preview/<key>.pdf"); it
	// returns the collected keys.
	CollectBlobs(ctx context.Context, keys []string, grace time.Duration) ([]string, error)
}

// UserSkill is an uploaded skill package.
type UserSkill struct {
	ID        string
	UserLogin string
	Name      string
	// DisplayName is the SKILL.md name (Name is its slug); "" = Name.
	DisplayName string
	Description string
	Version     int
	SHA256      string
	Files       int
	SizeBytes   int64
	// Package is the canonical zip (only loaded by GetUserSkill).
	Package   []byte
	CreatedAt time.Time
	UpdatedAt time.Time
}

// SkillStore persists user skills, per-user toggles and run-requested skills.
type SkillStore interface {
	// UpsertUserSkill stores a package; re-uploading a name bumps its version (same id).
	UpsertUserSkill(ctx context.Context, s UserSkill) (*UserSkill, error)
	ListUserSkills(ctx context.Context, login string) ([]*UserSkill, error)
	GetUserSkill(ctx context.Context, login, id string) (*UserSkill, error)
	DeleteUserSkill(ctx context.Context, login, id string) error
	// SkillSettings returns the explicit per-user toggles by skill id.
	SkillSettings(ctx context.Context, login string) (map[string]bool, error)
	SetSkillEnabled(ctx context.Context, login, skillID string, enabled bool) error
	// RunSkills returns the skill names requested at run creation.
	RunSkills(ctx context.Context, runID string) ([]string, error)
}

// ExtraAttachments is the eino schema.Message Extra key holding the
// []Attachment of a user message in the model history (agent → image attacher).
const ExtraAttachments = "work.attachments"

// Attachment is one entry of a user message's metadata.attachments.
type Attachment struct {
	ArtifactID string `json:"artifactId"`
	Name       string `json:"name"`
	Path       string `json:"path"`
	SizeBytes  int64  `json:"sizeBytes"`
	MimeType   string `json:"mimeType"`
	// Version is the upload version attached (extra field, not in the contract's list).
	Version int `json:"version,omitempty"`
}

// MessageAttachments decodes metadata.attachments of a message.
func MessageAttachments(m *Message) []Attachment {
	raw, ok := m.Metadata["attachments"]
	if !ok {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var out []Attachment
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}

// ArtifactPaths lists the paths of a message's metadata.artifacts (tool
// results and end-of-run changes), deleted ones suffixed with " (deleted)".
func ArtifactPaths(metadata map[string]any) []string {
	raw, ok := metadata["artifacts"]
	if !ok {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var items []struct {
		Path   string `json:"path"`
		Change string `json:"change"`
	}
	if json.Unmarshal(b, &items) != nil {
		return nil
	}
	var out []string
	for _, it := range items {
		switch {
		case it.Path == "":
		case it.Change == ChangeDeleted:
			out = append(out, it.Path+" (deleted)")
		default:
			out = append(out, it.Path)
		}
	}
	return out
}
