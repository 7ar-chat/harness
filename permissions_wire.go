package harness

import (
	"fmt"
	"os"
	"strings"

	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/engine/agent"
	"github.com/GhaziBenDahmane/harness/engine/approvals"
	"github.com/GhaziBenDahmane/harness/features/gitsafe"
	"github.com/GhaziBenDahmane/harness/features/permissions"
)

// wirePermissions puts the permission layer (allow/ask/deny rules, modes) in
// front of the tools and snapshots the git working tree at the start of runs.
// It applies to the local backend and the coding profile only: with neither,
// the harness is left exactly as it is (the work profile on OpenSandbox).
//
// The mode is AGENT_PERMISSIONS; without it the local backend asks (ask) and
// the coding profile on a remote sandbox, which is isolated, does not (yolo:
// deny rules still apply). The interactive approvals gate stays behind it for
// tools the policy has no rule for.
func wirePermissions(cfg *config.Config, hs agent.Harness) (agent.Harness, error) {
	coding := strings.EqualFold(strings.TrimSpace(cfg.AgentProfile), "coding")
	if !cfg.LocalSandbox() && !coding {
		return hs, nil
	}
	ws := cfg.LocalWorkspace
	if ws == "" && cfg.LocalSandbox() {
		ws, _ = os.Getwd()
	}
	mode := cfg.AgentPermissions
	if strings.TrimSpace(mode) == "" && !cfg.LocalSandbox() {
		mode = string(permissions.ModeYolo)
	}
	policy, err := permissions.Load(permissions.Options{
		Workspace: ws, Mode: mode,
		Allow: os.Getenv("AGENT_PERMISSIONS_ALLOW"), Ask: os.Getenv("AGENT_PERMISSIONS_ASK"), Deny: os.Getenv("AGENT_PERMISSIONS_DENY"),
	})
	if err != nil {
		return hs, fmt.Errorf("harness: %w", err)
	}
	var out []agent.BeforeToolHook
	wrapped := false
	for _, h := range hs.BeforeTool {
		if g, ok := h.(*approvals.Gate); ok && !wrapped {
			out = append(out, &permissions.Hook{Policy: policy, Inner: g})
			wrapped = true
			continue
		}
		out = append(out, h)
	}
	hs.BeforeTool = out
	if cfg.LocalSandbox() && ws != "" {
		hs.RunStart = append(hs.RunStart, gitsafe.Hook{Dir: ws})
	}
	return hs, nil
}
