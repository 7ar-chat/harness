package main

import (
	"context"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/app/product"
	"github.com/7ar-chat/harness/core/sandbox"
	core "github.com/7ar-chat/harness/core/workflows"
	wfengine "github.com/7ar-chat/harness/features/workflows"
)

// workflowSandbox gives the command steps of a run their sandbox (one per run and repository, the repository checked
// out on the step's branch under the run owner's credentials).
func workflowSandbox(h *harness.Harness) func(context.Context, core.Run, string, string) (sandbox.Sandbox, error) {
	return func(ctx context.Context, r core.Run, repo, branch string) (sandbox.Sandbox, error) {
		base := ""
		if repo == r.Repo {
			base = r.BaseBranch
		}
		return h.WorkflowSandbox(ctx, r.Owner, r.ID, repo, base, branch)
	}
}

// workflowJudge applies the factory policy to the command of a step before it runs: supervised asks a person,
// protected paths and secrets are refused as for an agent.
func workflowJudge(h *harness.Harness) func(context.Context, core.Run, core.Node, string) wfengine.Verdict {
	return func(_ context.Context, r core.Run, n core.Node, cmd string) wfengine.Verdict {
		repo := core.RepoOf(r, n)
		deny, ask, reason := h.CommandVerdict(n.Policy, cmd, r.BaseBranch, core.BranchFor(r, repo))
		return wfengine.Verdict{Deny: deny, Ask: ask, Reason: reason}
	}
}

// workflowPreview starts the app of a preview step through the product's preview mechanism.
func workflowPreview(pv *product.Preview) func(context.Context, core.Run, core.Node, sandbox.Sandbox) (string, string, error) {
	if pv == nil {
		return nil
	}
	return func(ctx context.Context, r core.Run, n core.Node, sb sandbox.Sandbox) (string, string, error) {
		url, log, ok, err := pv.StartIn(ctx, sb, harness.WorkflowSandboxKey(r.ID, core.RepoOf(r, n)))
		switch {
		case err != nil:
			return "", log, err
		case !ok:
			return "", log, wfengine.ErrNoPreview
		}
		return url, log, nil
	}
}
