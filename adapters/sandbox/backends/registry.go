// Package backends is the registry of sandbox backends selectable by SANDBOX_BACKEND=<name>.
package backends

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/core/sandbox"
)

// A Factory builds the Backend of a configuration. A nil sandbox.Backend with a nil
// error means "no sandbox".
type Factory func(cfg *config.Config) (sandbox.Backend, error)

var (
	regMu    sync.RWMutex
	registry = map[string]Factory{}
)

// Register makes a backend selectable by SANDBOX_BACKEND=<name>. Built-ins
// register themselves: "none" (here), "opensandbox" (adapters/sandbox/opensandbox) and "local" (unix only,
// from the harness package). A host with its own adapter calls Register from
// an init function or before harness.New, then runs with SANDBOX_BACKEND=<name>
// (or passes the sandbox.Backend in harness.Options.Backend instead, which needs no
// registration). Names are case-insensitive; registering a name twice, an
// empty name or a nil factory panics: it is a programming error.
func Register(name string, f Factory) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" || f == nil {
		panic("sandbox: Register needs a name and a factory")
	}
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := registry[name]; dup {
		panic("sandbox: backend " + name + " registered twice")
	}
	registry[name] = f
}

// Registered lists the names of the registered backends, sorted.
func Registered() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ErrUnknownBackend means SANDBOX_BACKEND names no registered backend.
var ErrUnknownBackend = errors.New("unknown sandbox backend")

// NewBackend builds the backend selected by the configuration
// (Config.SandboxBackendName): the registered factory of SANDBOX_BACKEND, or
// of "opensandbox" when only OPENSANDBOX_URL is set, or none (a nil sandbox.Backend).
func NewBackend(cfg *config.Config) (sandbox.Backend, error) {
	name := cfg.SandboxBackendName()
	regMu.RLock()
	f, ok := registry[name]
	regMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w %q (SANDBOX_BACKEND); registered: %s", ErrUnknownBackend, name, strings.Join(Registered(), ", "))
	}
	return f(cfg)
}

func init() {
	Register(config.BackendNone, func(*config.Config) (sandbox.Backend, error) { return nil, nil })
}
