package store

import (
	"context"
	"time"
)

// Marketplace sync run statuses (AstraForge's SkillCatalogSyncStatus).
const (
	MarketplaceRunRunning   = "running"
	MarketplaceRunSucceeded = "succeeded"
	MarketplaceRunPartial   = "partial"
	MarketplaceRunFailed    = "failed"
)

// MarketplaceSource is where a global skill was synced from (migration 0015).
type MarketplaceSource struct {
	GlobalSkillID   string
	RepositoryURL   string // without credentials
	Branch          string
	SourcePath      string // skills/<namespace>/<path>
	Namespace       string
	CommitSHA       string
	DeclaredVersion string
	// Deprecated: the path is gone from the repository (the skill is unpublished).
	Deprecated   bool
	LastSyncedAt time.Time
}

// MarketplaceIssue is a warning (discovery) or an error (one skill) of a sync.
type MarketplaceIssue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// MarketplaceRun is one sync (AstraForge's SkillCatalogSyncRun).
type MarketplaceRun struct {
	ID            string             `json:"id"`
	Trigger       string             `json:"trigger"`
	Status        string             `json:"status"`
	RepositoryURL string             `json:"repository_url"`
	Branch        string             `json:"branch"`
	CommitSHA     string             `json:"commit_sha"`
	Discovered    int                `json:"discovered_count"`
	Created       int                `json:"created_count"`
	Updated       int                `json:"updated_count"`
	Unchanged     int                `json:"unchanged_count"`
	Skipped       int                `json:"skipped_count"`
	Deprecated    int                `json:"deprecated_count"`
	Failed        int                `json:"failed_count"`
	Warnings      []MarketplaceIssue `json:"warnings"`
	Errors        []MarketplaceIssue `json:"errors"`
	StartedAt     time.Time          `json:"started_at"`
	FinishedAt    *time.Time         `json:"finished_at,omitempty"`
}

// SkillMarketplaceStore persists the marketplace sources of global skills and
// the sync runs (migration 0015).
type SkillMarketplaceStore interface {
	// MarketplaceSources returns every global skill's source.
	MarketplaceSources(ctx context.Context) ([]*MarketplaceSource, error)
	// UpsertMarketplaceSource creates or replaces the source of a global skill (ErrNotFound without it).
	UpsertMarketplaceSource(ctx context.Context, src MarketplaceSource) error
	// UpdateGlobalSkillInfo sets a global skill's display name and description (ErrNotFound).
	UpdateGlobalSkillInfo(ctx context.Context, id, displayName, description string) error
	// CreateMarketplaceRun inserts a run (assigns ID and StartedAt).
	CreateMarketplaceRun(ctx context.Context, r *MarketplaceRun) error
	// FinishMarketplaceRun stores a run's status, counts, issues and finished_at.
	FinishMarketplaceRun(ctx context.Context, r *MarketplaceRun) error
	// ListMarketplaceRuns returns the latest runs, newest first.
	ListMarketplaceRuns(ctx context.Context, limit int) ([]*MarketplaceRun, error)
}
