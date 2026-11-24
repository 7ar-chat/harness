// Package projdata is the small per-project state that has no home of its own: the weekly digest (its setting, the
// slots already sent, the history with the generated report) and the sealed credentials of outside services a project
// is connected to (TAPD). adapters/store/projdatastore stores it; app/product serves it.
package projdata

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned for a digest or secret that does not exist (also in another project).
var ErrNotFound = errors.New("projdata: not found")

// KeepDigests is how many digests of a project the store keeps; older ones are dropped when a new one is added.
const KeepDigests = 26

// DigestSettings is a project's weekly digest setting. A project that never set one has the zero value (off).
type DigestSettings struct {
	ProjectID string `json:"projectId"`
	Enabled   bool   `json:"enabled"`
	// Weekday is 0 (Sunday) to 6 (Saturday), Hour 0 to 23, in the zone Timezone (an IANA name; "" is UTC).
	Weekday  int    `json:"weekday"`
	Hour     int    `json:"hour"`
	Timezone string `json:"timezone"`
	// EnabledAt is when the digest was last switched on: a slot before it is never sent.
	EnabledAt time.Time `json:"enabledAt,omitzero"`
	// LastSentAt is when the last digest was generated; LastSlot the slot that generated it (or skipped it).
	LastSentAt time.Time `json:"lastSentAt,omitzero"`
	LastSlot   string    `json:"lastSlot,omitempty"`
}

// Digest trigger values.
const (
	TriggerSchedule = "schedule"
	TriggerManual   = "manual"
)

// Digest is one generated report: the range it covers, a summary line, and the report itself.
type Digest struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"projectId"`
	Slot      string    `json:"slot"`
	Trigger   string    `json:"trigger"`
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
	CreatedAt time.Time `json:"createdAt"`
	Summary   string    `json:"summary"`
	// HTML is the self-contained report page, JSON the raw numbers. They are empty in a listing.
	HTML []byte `json:"html,omitempty"`
	JSON []byte `json:"json,omitempty"`
}

// Store persists the state above. All methods are safe for concurrent use, across processes for Postgres.
type Store interface {
	// GetDigestSettings returns the project's setting, the zero value (with ProjectID) when it never set one.
	GetDigestSettings(ctx context.Context, project string) (DigestSettings, error)
	PutDigestSettings(ctx context.Context, s DigestSettings) error
	// EnabledDigests returns the settings of every project whose digest is on.
	EnabledDigests(ctx context.Context) ([]DigestSettings, error)
	// ClaimSlot records that this process takes the slot of the project; it reports false when another claimed it
	// before. A claim is the only thing that lets a digest be generated, so two replicas never both send one.
	ClaimSlot(ctx context.Context, project, slot string, at time.Time) (bool, error)
	// ReleaseSlot gives a claim back (the generation failed and may be retried).
	ReleaseSlot(ctx context.Context, project, slot string) error
	// AddDigest stores the digest (ID and times come from the caller) and drops the digests beyond KeepDigests,
	// oldest first.
	AddDigest(ctx context.Context, d Digest) error
	// ListDigests returns up to limit digests, newest first, without their HTML and JSON.
	ListDigests(ctx context.Context, project string, limit int) ([]Digest, error)
	// GetDigest returns one digest with its report, ErrNotFound when it is not in the project.
	GetDigest(ctx context.Context, project, id string) (Digest, error)
	// PutSecret, GetSecret and DeleteSecret keep one sealed blob per project and kind ("tapd"). The store never
	// sees the plain text. GetSecret returns ErrNotFound when there is none; DeleteSecret never fails for a missing one.
	PutSecret(ctx context.Context, project, kind string, sealed []byte) error
	GetSecret(ctx context.Context, project, kind string) ([]byte, error)
	DeleteSecret(ctx context.Context, project, kind string) error
	// DeleteProject removes everything the project owns.
	DeleteProject(ctx context.Context, project string) error
}
