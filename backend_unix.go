//go:build unix

package harness

import (
	"github.com/GhaziBenDahmane/harness/adapters/sandbox/backends"
	"os"

	"github.com/GhaziBenDahmane/harness/adapters/sandbox/local"
	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/core/sandbox"
)

func init() { backends.Register(config.BackendLocal, localBackend) }

// localBackend is the SANDBOX_BACKEND=local backend: commands run on this machine.
func localBackend(cfg *config.Config) (sandbox.Backend, error) {
	ws := cfg.LocalWorkspace
	if ws == "" {
		var err error
		if ws, err = os.Getwd(); err != nil {
			return nil, err
		}
	}
	return local.New(local.Options{Workspace: ws, StateDir: cfg.LocalStateDir})
}
