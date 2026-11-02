// Package conformance is the behavioural test suite every store backend (memstore, filestore, pgstore) must pass: the same
// cases run against each one, so a backend cannot drift from the port's contract (core/store, core/sandbox.Store).
//
//	func TestConformance(t *testing.T) {
//		conformance.Run(t, func(t *testing.T) conformance.Backend { return conformance.Backend{Store: memstore.New(), ...} })
//	}
//
// It is test support, not production code (coverage.sh skips packages named conformance).
package conformance

import (
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/store"
)

// Store is everything a backend implements: the engine's store.Full plus the optional capabilities every backend carries.
type Store interface {
	store.Full
	store.HistoryStore
	store.WorkspaceGitStore
	store.MemoryEmbeddingStore
	sandbox.Store
}

// Backend is one fresh, empty store and the hooks the suite needs to drive time and restarts.
type Backend struct {
	Store Store
	// Advance moves the backend's clock forward by d: every timestamp already stored (run heartbeats and creation, approvals,
	// soft deletions, blob last references...) becomes d older, as seen by the age-based methods (InterruptStaleRuns,
	// ExpireApprovals, PurgeDeletedThreads, BlobCandidates...). The suite never sleeps; it advances.
	Advance func(t *testing.T, d time.Duration)
	// Reopen closes the store and opens the same state again (nil for a backend that does not persist between instances).
	// The old Store must not be used after it.
	Reopen func(t *testing.T) Store
}

// Factory returns a fresh Backend; it registers its cleanups on t and skips t when the backend is not available.
type Factory func(t *testing.T) Backend

// Run runs the whole suite.
func Run(t *testing.T, f Factory) {
	t.Helper()
	t.Run("Core", func(t *testing.T) { Core(t, f) })
	t.Run("Approvals", func(t *testing.T) { Approvals(t, f) })
	t.Run("Harness", func(t *testing.T) { Harness(t, f) })
	t.Run("Artifacts", func(t *testing.T) { Artifacts(t, f) })
	t.Run("Memory", func(t *testing.T) { Memory(t, f) })
	t.Run("Persistence", func(t *testing.T) { Persistence(t, f) })
}
