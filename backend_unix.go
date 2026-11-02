//go:build unix

package harness

import (
	"github.com/7ar-chat/harness/adapters/sandbox/backends"
	"os"

	"github.com/7ar-chat/harness/adapters/sandbox/local"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/sandbox"
)

func init() { backends.Register(config.BackendLocal, localBackend) }

// localBackend is the SANDBOX_BACKEND=local backend: commands run on this machine.
func localBackend(s backends.Settings) (sandbox.Backend, error) {
	ws := s.Local.Workspace
	if ws == "" {
		var err error
		if ws, err = os.Getwd(); err != nil {
			return nil, err
		}
	}
	return local.New(local.Options{Workspace: ws, StateDir: s.Local.StateDir})
}
