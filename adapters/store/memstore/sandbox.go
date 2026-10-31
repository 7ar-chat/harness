package memstore

import (
	"context"
	"sort"

	"github.com/GhaziBenDahmane/harness/core/sandbox"
)

// SwapThreadSandbox implements the sandbox manager's compare-and-set.
func (s *Store) SwapThreadSandbox(_ context.Context, threadID, expected, sandboxID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.threads[threadID]
	if !ok || t.deleted || t.SandboxID != expected {
		return false, nil
	}
	t.SandboxID = sandboxID
	return true, nil
}

// UserSandboxes lists the user's live threads with a recorded sandbox.
func (s *Store) UserSandboxes(_ context.Context, login string) ([]sandbox.UserSandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []sandbox.UserSandbox
	for id, t := range s.threads {
		if t.UserLogin == login && !t.deleted && t.SandboxID != "" {
			out = append(out, sandbox.UserSandbox{ThreadID: id, SandboxID: t.SandboxID})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ThreadID < out[j].ThreadID })
	return out, nil
}
