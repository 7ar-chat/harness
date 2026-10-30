package harness

import (
	"os"
	"path/filepath"

	"github.com/GhaziBenDahmane/harness/adapters/store/filestore"
	"github.com/GhaziBenDahmane/harness/config"
)

// OpenLocalStore opens the durable single-process store for local hosts (CLI,
// TUI, desktop): threads, messages, runs, plans, summaries, approvals and
// event logs survive restarts, with no Postgres or Redis. Pass it as
// Options.Store and Close it on exit.
//
// dir is the state directory; "" uses DefaultLocalStoreDir. Runs that were in
// progress when the previous process died are marked interrupted. Use
// Store.Sessions() to list, fork and delete threads. See store/filestore.
func OpenLocalStore(dir string) (*filestore.Store, error) {
	if dir == "" {
		dir = DefaultLocalStoreDir()
	}
	return filestore.Open(dir)
}

// DefaultLocalStoreDir is the state directory OpenLocalStore uses when none is
// given: $LOCAL_STATE_DIR, else <user dir>/sessions ($HAR_HOME or ~/.har), else
// (user directory disabled or unknown) $XDG_STATE_HOME/harness, else
// ~/.local/state/harness.
func DefaultLocalStoreDir() string {
	if d := os.Getenv("LOCAL_STATE_DIR"); d != "" {
		return d
	}
	if u := config.UserDir(); u != "" {
		return filepath.Join(u, config.SessionsFolder)
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "harness")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "harness-state")
	}
	return filepath.Join(home, ".local", "state", "harness")
}

var _ Store = (*filestore.Store)(nil)
