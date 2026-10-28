package sandbox

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/GhaziBenDahmane/harness/config"
)

// A Factory builds the Backend of a configuration. A nil Backend with a nil
// error means "no sandbox".
type Factory func(cfg *config.Config) (Backend, error)

var (
	regMu    sync.RWMutex
	registry = map[string]Factory{}
)

// Register makes a backend selectable by SANDBOX_BACKEND=<name>. Built-ins
// register themselves: "opensandbox", "none" (here) and "local" (unix only,
// from the harness package). A host with its own adapter calls Register from
// an init function or before harness.New, then runs with SANDBOX_BACKEND=<name>
// (or passes the Backend in harness.Options.Backend instead, which needs no
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
// of "opensandbox" when only OPENSANDBOX_URL is set, or none (a nil Backend).
func NewBackend(cfg *config.Config) (Backend, error) {
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
	Register(config.BackendNone, func(*config.Config) (Backend, error) { return nil, nil })
	Register(config.BackendOpenSandbox, func(cfg *config.Config) (Backend, error) {
		if cfg.OpenSandboxURL == "" {
			return nil, errors.New("SANDBOX_BACKEND=opensandbox needs OPENSANDBOX_URL")
		}
		return BackendFromConfig(cfg)
	})
}
