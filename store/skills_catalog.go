package store

import (
	"context"
	"errors"
	"time"
)

// Skill version statuses.
const (
	SkillDraft     = "draft"
	SkillPublished = "published"
)

// Skill version source types (Work: upload / editor / generated).
const (
	SkillSourceUpload    = "upload"
	SkillSourceEditor    = "editor"
	SkillSourceGenerated = "generated"
)

// ErrIdempotencyConflict means an Idempotency-Key was reused for another request.
var ErrIdempotencyConflict = errors.New("idempotency key reused with another request")

// SkillVersion is one immutable package version of a private or global skill.
type SkillVersion struct {
	ID string
	// SkillID is the work_skills id (Global=false) or the work_global_skills id.
	SkillID     string
	Global      bool
	Version     string
	Status      string
	SourceType  string
	Name        string
	Description string
	SHA256      string
	Files       int
	SizeBytes   int64
	CreatedBy   string
	CreatedAt   time.Time
	// Package is the canonical zip (only loaded by GetSkillVersion).
	Package []byte
}

// SkillMeta is the version bookkeeping of a private skill (work_skills).
type SkillMeta struct {
	SkillID            string
	DisplayName        string
	PublishedVersionID string
	LatestVersionID    string
}

// GlobalSkill is an operator-published skill (work_global_skills).
type GlobalSkill struct {
	ID                 string
	Name               string
	DisplayName        string
	Description        string
	Tags               []string
	PublishedVersionID string
	LatestVersionID    string
	CreatedBy          string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// NewSkillDraft is one package of a draft batch (a new private skill).
type NewSkillDraft struct {
	// Name is the unique identifier (slug) among the user's private skills.
	Name        string
	DisplayName string
	Description string
	Version     SkillVersion
	Publish     bool
}

// DraftItem is one created draft.
type DraftItem struct {
	SkillID   string
	VersionID string
	Name      string
}

// Idempotency identifies an idempotent command.
type Idempotency struct {
	Login   string
	Command string
	Key     string
	// RequestHash fingerprints the request: a replay with another hash is ErrIdempotencyConflict.
	RequestHash string
}

// CommandResult is a stored command response (StatusCode 0: claimed, still running).
type CommandResult struct {
	RequestHash string
	StatusCode  int
	Response    []byte
	// CreatedAt is when the key was claimed (memstore: lease and retention).
	CreatedAt time.Time
}

const (
	// CommandClaimLease frees the claim of a request that died before storing
	// its result (status 0), so its Idempotency-Key is not stuck forever (B-18).
	CommandClaimLease = 5 * time.Minute
	// CommandRetention is how long an Idempotency-Key replays its result (24 h).
	CommandRetention = 24 * time.Hour
)

// SkillCreationSession is a thread in mode skill_creation.
type SkillCreationSession struct {
	ThreadID    string
	UserLogin   string
	Mode        string
	Action      string
	PackageRoot string
	Title       string
	CreatedAt   time.Time
}

// SkillCatalogStore persists skill versions, global skills, idempotent skill
// commands and skill creation sessions (migration 0008).
type SkillCatalogStore interface {
	// SkillMetas returns the version bookkeeping of the user's private skills by id.
	SkillMetas(ctx context.Context, login string) (map[string]SkillMeta, error)
	// ListSkillVersions returns the versions of skills (private or global ids), newest first, without packages.
	ListSkillVersions(ctx context.Context, skillIDs []string) (map[string][]*SkillVersion, error)
	// GetSkillVersion returns a version with its package (ErrNotFound).
	GetSkillVersion(ctx context.Context, versionID string) (*SkillVersion, error)
	// AddSkillVersion adds a version to a private skill of login (v.Global=false)
	// or to a global skill; publish makes it the published version. A
	// duplicate version label is ErrConflict. The private skill's effective
	// package (work_skills.package) follows the published version, else the latest.
	AddSkillVersion(ctx context.Context, login string, v SkillVersion, publish bool) (*SkillVersion, error)
	// PublishSkillVersion makes an existing version the published one.
	PublishSkillVersion(ctx context.Context, login, skillID, versionID string, global bool) error
	// CreateSkillDrafts creates every draft as a new private skill in one
	// transaction and stores the command response (response(items)) under the
	// idempotency key; a replay of the same request returns the stored response
	// (replayed=true), another request with that key is ErrIdempotencyConflict.
	// A name already used by the user is ErrConflict.
	CreateSkillDrafts(ctx context.Context, idem Idempotency, drafts []NewSkillDraft, response func([]DraftItem) []byte) (body []byte, replayed bool, err error)

	ListGlobalSkills(ctx context.Context) ([]*GlobalSkill, error)
	GetGlobalSkill(ctx context.Context, id string) (*GlobalSkill, error)
	// CreateGlobalSkill creates a global skill with its first version (ErrConflict on a used name).
	CreateGlobalSkill(ctx context.Context, s GlobalSkill, v SkillVersion, publish bool) (*GlobalSkill, *SkillVersion, error)
	UnpublishGlobalSkill(ctx context.Context, id string) error
	SetGlobalSkillTags(ctx context.Context, id string, tags []string) error
	DeleteGlobalSkill(ctx context.Context, id string) error

	// CommandResult returns a stored command result (ErrNotFound).
	CommandResult(ctx context.Context, login, command, key string) (*CommandResult, error)
	// SaveCommandResult stores a command result, completing the key's claim
	// (ClaimCommand); ErrConflict when the key already has a result.
	SaveCommandResult(ctx context.Context, idem Idempotency, r CommandResult) error
	// ClaimCommand reserves an idempotency key before a command runs (a pending
	// row: CommandResult then has StatusCode 0); false when the key is already
	// claimed or done. A concurrent identical command waits for its result.
	ClaimCommand(ctx context.Context, idem Idempotency) (bool, error)
	// ReleaseCommand drops a pending claim (the command failed: a retry may run it).
	ReleaseCommand(ctx context.Context, idem Idempotency) error

	CreateSkillCreationSession(ctx context.Context, s SkillCreationSession) error
	// GetSkillCreationSession returns the session of a live thread of login (ErrNotFound).
	GetSkillCreationSession(ctx context.Context, login, threadID string) (*SkillCreationSession, error)
	// ListSkillCreationSessions returns the user's sessions (live threads), newest first, and their total.
	ListSkillCreationSessions(ctx context.Context, login string, limit, offset int) ([]*SkillCreationSession, int, error)
}
