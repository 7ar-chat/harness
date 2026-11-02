//go:build !unix

package harness

import (
	"errors"
	"github.com/7ar-chat/harness/adapters/sandbox/backends"

	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/sandbox"
)

func init() { backends.Register(config.BackendLocal, localBackend) }

func localBackend(backends.Settings) (sandbox.Backend, error) {
	return nil, errors.New("SANDBOX_BACKEND=local is not supported on this platform")
}
