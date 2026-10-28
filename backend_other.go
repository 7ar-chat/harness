//go:build !unix

package harness

import (
	"errors"

	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/sandbox"
)

func init() { sandbox.Register(config.BackendLocal, localBackend) }

func localBackend(*config.Config) (sandbox.Backend, error) {
	return nil, errors.New("SANDBOX_BACKEND=local is not supported on this platform")
}
