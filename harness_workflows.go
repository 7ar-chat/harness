package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"
	"sync"

	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/sandbox"
)

// memSandboxStore is the sandbox.Store of workflow runs: their sandboxes belong to no conversation, so no thread row
// records them. It lives in this process: a restart forgets the sandboxes (a step that was running starts again in a
// new one; the provider expires the old).
type memSandboxStore struct {
	mu    sync.Mutex
	ids   map[string]string // key -> sandbox id
	owner map[string]string // key -> login
}

func newMemSandboxStore() *memSandboxStore {
	return &memSandboxStore{ids: map[string]string{}, owner: map[string]string{}}
}

func (s *memSandboxStore) ThreadSandbox(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ids[key], nil
}

func (s *memSandboxStore) SetThreadSandbox(_ context.Context, key, expected, id string) error {
	_, err := s.SwapThreadSandbox(context.Background(), key, expected, id)
	return err
}

func (s *memSandboxStore) SwapThreadSandbox(_ context.Context, key, expected, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ids[key] != expected {
		return false, nil
	}
	if id == "" {
		delete(s.ids, key)
	} else {
		s.ids[key] = id
	}
	return true, nil
}

func (s *memSandboxStore) CountUserSandboxes(_ context.Context, login string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k := range s.ids {
		if s.owner[k] == login {
			n++
		}
	}
	return n, nil
}

func (s *memSandboxStore) UserSandboxes(_ context.Context, login string) ([]sandbox.UserSandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []sandbox.UserSandbox
	for k, id := range s.ids {
		if s.owner[k] == login {
			out = append(out, sandbox.UserSandbox{ThreadID: k, SandboxID: id})
		}
	}
	return out, nil
}

// claim records who a key belongs to (for the quota).
func (s *memSandboxStore) claim(key, login string) {
	s.mu.Lock()
	s.owner[key] = login
	s.mu.Unlock()
}

func (s *memSandboxStore) keysWithPrefix(prefix string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.ids {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out
}

// WorkflowSandboxKey is the key a workflow run's sandbox for a repository is filed under: one sandbox per run and
// repository, shared by the command steps of the run.
func WorkflowSandboxKey(runID, repo string) string {
	if repo == "" {
		return "wfrun-" + runID
	}
	sum := sha256.Sum256([]byte(repo))
	return "wfrun-" + runID + "-" + hex.EncodeToString(sum[:4])
}

// WorkflowSandbox returns the sandbox a workflow run's command steps run in, creating it when needed. With a repo
// the repository is checked out on branch (the factory's checkout does it when the sandbox is created, under the
// run owner's credentials); without one the sandbox is an empty workspace. It fails with sandbox.ErrDisabled when no
// backend is configured.
func (h *Harness) WorkflowSandbox(ctx context.Context, login, runID, repo, baseBranch, branch string) (sandbox.Sandbox, error) {
	if h.wfSandboxes == nil || !h.wfSandboxes.Enabled() {
		return nil, sandbox.ErrDisabled
	}
	key := WorkflowSandboxKey(runID, repo)
	if repo != "" && h.factory != nil && h.factory.Store != nil {
		err := h.factory.Store.SetThreadInfo(ctx, factory.ThreadInfo{ThreadID: key, Login: login, Repo: repo, BaseBranch: baseBranch, Branch: branch})
		if err != nil {
			return nil, err
		}
	}
	h.wfStore.claim(key, login)
	return h.wfSandboxes.Acquire(ctx, key, login)
}

// ReleaseWorkflowSandboxes destroys the sandboxes of a finished workflow run (best effort).
func (h *Harness) ReleaseWorkflowSandboxes(ctx context.Context, runID string) {
	if h.wfSandboxes == nil || h.wfStore == nil {
		return
	}
	for _, key := range h.wfStore.keysWithPrefix("wfrun-" + runID) {
		id, _ := h.wfStore.ThreadSandbox(ctx, key)
		if id == "" {
			continue
		}
		if err := h.wfSandboxes.Kill(ctx, id); err != nil {
			slog.Warn("workflows: destroying a run's sandbox failed", "run", runID, "sandbox", id, "error", err)
		}
		_, _ = h.wfStore.SwapThreadSandbox(ctx, key, id, "")
	}
}

// SandboxEnabled reports whether a sandbox backend is configured.
func (h *Harness) SandboxEnabled() bool { return h.Sandboxes != nil && h.Sandboxes.Enabled() }

// CommandVerdict applies the factory policy (policyName "": the factory's default) to a shell command a workflow
// step is about to run directly: deny is the refusal text, ask says a person must approve it first. Without the
// policy feature nothing is denied and nothing asked.
func (h *Harness) CommandVerdict(policyName, command, baseBranch, branch string) (deny string, ask bool, reason string) {
	if h.factory == nil || h.factory.Policy == nil {
		return "", false, ""
	}
	d := h.factory.Policy.DecideCommand(policyName, command, baseBranch, branch)
	switch {
	case d.Deny != "":
		return d.Deny, false, ""
	case d.Ask:
		return "", true, "policy " + firstPolicy(policyName) + " asks before running this command (" + d.Rule + ")"
	}
	return "", false, ""
}

func firstPolicy(n string) string {
	if n == "" {
		return "default"
	}
	return n
}
