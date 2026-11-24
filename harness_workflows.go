package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/sandbox"
)

// WorkflowSandboxKey is the sandbox key (the "thread id" the sandbox manager files it under) of a workflow run's
// sandbox for a repository: one sandbox per run and repository, shared by the command steps of the run.
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
	if h.Sandboxes == nil || !h.Sandboxes.Enabled() {
		return nil, sandbox.ErrDisabled
	}
	key := WorkflowSandboxKey(runID, repo)
	if repo != "" && h.factory != nil && h.factory.Store != nil {
		err := h.factory.Store.SetThreadInfo(ctx, factory.ThreadInfo{ThreadID: key, Login: login, Repo: repo, BaseBranch: baseBranch, Branch: branch})
		if err != nil {
			return nil, err
		}
	}
	return h.Sandboxes.Acquire(ctx, key, login)
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
