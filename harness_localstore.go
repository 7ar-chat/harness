package harness

import "github.com/GhaziBenDahmane/harness/adapters/store/filestore"

// OpenLocalStore opens the durable single-process store for local hosts (CLI,
// TUI, desktop): threads, messages, runs, plans, summaries, approvals and
// event logs survive restarts, with no Postgres or Redis. Pass it as
// Options.Store and Close it on exit.
//
// dir is the state directory; "" uses $LOCAL_STATE_DIR, else
// <user dir>/sessions ($HAR_HOME or ~/.har), else $XDG_STATE_HOME/harness, else
// ~/.local/state/harness. Runs that were in
// progress when the previous process died are marked interrupted. Use
// Store.Sessions() to list, fork and delete threads. See store/filestore.
func OpenLocalStore(dir string) (*filestore.Store, error) { return filestore.Open(dir) }

var _ Store = (*filestore.Store)(nil)
