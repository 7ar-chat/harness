package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/store"
)

// EnvironmentKey is the thread-state key holding the runtime environment of a thread.
const EnvironmentKey = "environment"

// ErrEnvironmentFrozen means the thread already has a workspace (a sandbox): its environment can no longer change.
// Open a new session in the other environment instead.
var ErrEnvironmentFrozen = fmt.Errorf("%w: the thread already has a workspace; its environment cannot change", ErrInvalidRequest)

// Environment is a runtime environment a thread can run in: a sandbox profile of the configured backend.
type Environment struct {
	// Name is the sandbox profile ("work", "coding", ...): what a thread stores.
	Name string `json:"name"`
	// Kind is the sandbox backend (SANDBOX_BACKEND): opensandbox, local, e2b, daytona...
	Kind        string `json:"kind"`
	Description string `json:"description"`
	// Egress summarises the network policy: "allowlist: a.com, b.com", "unrestricted (private ranges blocked)",
	// "managed by the backend".
	Egress string `json:"egress"`
	// Default marks the environment a thread without a choice runs in.
	Default bool `json:"default,omitempty"`
}

// Environments lists the runtime environments of the deployment: the profile of AGENT_PROFILE (the default) and
// each profile SANDBOX_IMAGES maps to an image. Empty without a sandbox.
func (h *Harness) Environments() []Environment {
	if !h.HasSandbox() {
		return []Environment{}
	}
	cfg := h.cfg
	kind := cfg.SandboxBackendName()
	def := cfg.SandboxProfile()
	names := map[string]bool{def: true}
	for n := range cfg.SandboxImages {
		names[n] = true
	}
	list := make([]string, 0, len(names))
	for n := range names {
		list = append(list, n)
	}
	sort.Strings(list)
	// The default first.
	if i := slices.Index(list, def); i > 0 {
		list = append([]string{def}, slices.Delete(list, i, i+1)...)
	}
	egress := egressSummary(h)
	out := make([]Environment, 0, len(list))
	for _, n := range list {
		desc := "The default image of the " + kind + " backend."
		if img := cfg.SandboxImages[n]; img != "" {
			desc = "Starts from " + img + " on the " + kind + " backend."
		}
		out = append(out, Environment{Name: n, Kind: kind, Description: desc, Egress: egress, Default: n == def})
	}
	return out
}

func egressSummary(h *Harness) string {
	cfg := h.cfg
	mode, err := cfg.EgressMode()
	switch {
	case err != nil:
		return "restricted by the deployment"
	case mode == "allowlist":
		return "allowlist: " + strings.Join(cfg.SandboxEgressAllowlist, ", ")
	case mode == "unrestricted":
		return "unrestricted (private ranges blocked)"
	}
	return "managed by the " + cfg.SandboxBackendName() + " backend"
}

// EnvironmentSelectable reports whether a front end should offer the choice: several environments, or
// ENVIRONMENT_SELECTOR is set.
func (h *Harness) EnvironmentSelectable() bool {
	envs := h.Environments()
	return len(envs) > 1 || (len(envs) > 0 && h.cfg.EnvironmentSelector)
}

// DefaultEnvironment is the name of the environment of a thread without a choice ("" without a sandbox).
func (h *Harness) DefaultEnvironment() string {
	for _, e := range h.Environments() {
		if e.Default {
			return e.Name
		}
	}
	return ""
}

// ThreadEnvironment is the environment recorded on the thread ("" when none: the default one applies).
func (h *Harness) ThreadEnvironment(ctx context.Context, threadID string) string {
	raw, err := h.ThreadState.GetThreadState(ctx, threadID, EnvironmentKey)
	if err != nil || len(raw) == 0 {
		return ""
	}
	var name string
	_ = json.Unmarshal(raw, &name)
	return name
}

// EnvironmentFrozen reports whether the thread already has a workspace.
func (h *Harness) EnvironmentFrozen(ctx context.Context, threadID string) bool {
	id, err := h.Store.ThreadSandbox(ctx, threadID)
	return err == nil && id != ""
}

// SetThreadEnvironment records the environment of a thread: ErrInvalidRequest for an unknown name,
// ErrEnvironmentFrozen once the thread has a workspace. Setting the one already recorded is always fine. An
// empty name clears the choice.
func (h *Harness) SetThreadEnvironment(ctx context.Context, user, threadID, name string) error {
	if err := h.ownsThread(ctx, user, threadID); err != nil {
		return err
	}
	name = strings.ToLower(strings.TrimSpace(name))
	if name != "" && !slices.ContainsFunc(h.Environments(), func(e Environment) bool { return e.Name == name }) {
		return fmt.Errorf("%w: unknown environment %q", ErrInvalidRequest, name)
	}
	if name == h.ThreadEnvironment(ctx, threadID) {
		return nil
	}
	if h.EnvironmentFrozen(ctx, threadID) {
		return ErrEnvironmentFrozen
	}
	return h.ThreadState.UpdateThreadState(ctx, threadID, EnvironmentKey, func(json.RawMessage) (json.RawMessage, error) {
		if name == "" {
			return nil, nil
		}
		return json.Marshal(name)
	})
}

// envManager creates a thread's sandbox in the environment the thread chose.
type envManager struct {
	sandbox.Manager
	ts store.ThreadStateStore
}

func (m envManager) Acquire(ctx context.Context, threadID, login string) (sandbox.Sandbox, error) {
	if raw, err := m.ts.GetThreadState(ctx, threadID, EnvironmentKey); err == nil && len(raw) > 0 {
		var name string
		if json.Unmarshal(raw, &name) == nil {
			ctx = sandbox.WithProfile(ctx, name)
		}
	}
	return m.Manager.Acquire(ctx, threadID, login)
}
