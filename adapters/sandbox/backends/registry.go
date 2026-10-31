// Package backends is the registry of sandbox backends selectable by SANDBOX_BACKEND=<name>.
package backends

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/7ar-chat/harness/core/sandbox"
)

// None is the name of the backend that provides no sandbox.
const None = "none"

// Settings are what the built-in backends need to be built; the composition root
// fills them from its configuration. A backend you register yourself reads the
// settings it needs from here or from its own sources (for example the environment).
type Settings struct {
	// Name selects the backend (SANDBOX_BACKEND, case-insensitive); empty means None.
	Name string
	// OpenSandbox configures the "opensandbox" backend.
	OpenSandbox OpenSandboxSettings
	// Local configures the "local" backend.
	Local LocalSettings
}

// OpenSandboxSettings configures the "opensandbox" backend.
type OpenSandboxSettings struct {
	URL            string
	APIKey         string
	UseServerProxy bool
	Image          string
	CPU            string
	Memory         string
	// TimeoutSeconds is the idle lifetime of a sandbox.
	TimeoutSeconds int
	// EgressAllowlist, when set, makes egress deny-by-default with these domains allowed.
	EgressAllowlist []string
	// EgressDenyCIDRs are denied in unrestricted mode (no allowlist); empty: the package default.
	EgressDenyCIDRs []string
	// CredentialProxy creates every sandbox with the egress sidecar's Credential Vault enabled.
	CredentialProxy bool
}

// LocalSettings configures the "local" backend.
type LocalSettings struct {
	// Workspace is the host directory mapped to /workspace ("": the working directory).
	Workspace string
	// StateDir is where the backend keeps its state ("": its default).
	StateDir string
}

// A Factory builds the Backend of the settings. A nil sandbox.Backend with a nil
// error means "no sandbox".
type Factory func(s Settings) (sandbox.Backend, error)

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

// NewBackend builds the backend named by s.Name with its registered factory
// (None: a nil sandbox.Backend).
func NewBackend(s Settings) (sandbox.Backend, error) {
	name := strings.ToLower(strings.TrimSpace(s.Name))
	if name == "" {
		name = None
	}
	regMu.RLock()
	f, ok := registry[name]
	regMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w %q (SANDBOX_BACKEND); registered: %s", ErrUnknownBackend, name, strings.Join(Registered(), ", "))
	}
	return f(s)
}

func init() {
	Register(None, func(Settings) (sandbox.Backend, error) { return nil, nil })
}
