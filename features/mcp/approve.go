package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/approvals"
	"github.com/GhaziBenDahmane/harness/engine/tools"
)

// A stdio server a project defines runs a program, so it needs the user's approval of its exact command
// (config.MCPApproved). While it is pending, the provider offers the model one tool per pending server,
// "mcp_approve__{server}_{hash}": calling it pauses the run with a normal approval (engine/approvals) that names the
// project, server, command, arguments and the keys of the environment (never its values). When the user approves,
// the tool records the same trust entry as `harness trust mcp` (config.ApproveMCP) and the server's tools are
// available from the next run (the run that decided the approval had its tools built before). A denial leaves the
// server pending: the tool is not offered again on the thread (its earlier result is in the history). The hash is
// part of the name, so a changed command is a new tool and asks again. Where no approval can be asked the tool is
// refused (approvals.Gate.Denial), so the server stays unstarted.

// approveToolName is the name of the approval tool of a pending server.
func approveToolName(s config.MCPServer) string {
	name := invalidName.ReplaceAllString(s.Name, "_")
	if room := 64 - len(approvals.TrustMCPToolPrefix) - 9; len(name) > room {
		name = name[:room]
	}
	return approvals.TrustMCPToolPrefix + name + "_" + config.MCPFingerprint(s)[:8]
}

// Describe is the text the approval of a pending server's tool shows: the project, the server, its exact command
// and arguments and the names of its environment variables (never their values). "" when toolName names no
// pending server of this provider.
func (p *Provider) Describe(toolName string) string {
	for _, s := range p.Pending {
		if approveToolName(s) == toolName {
			return describePending(s)
		}
	}
	return ""
}

func describePending(s config.MCPServer) string {
	args, _ := json.Marshal(append([]string{}, s.Args...))
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := "none"
	if len(keys) > 0 {
		env = strings.Join(keys, ", ")
	}
	cmd, _ := json.Marshal(s.Command)
	return fmt.Sprintf("Run the project's MCP server %q. project: %s; command: %s; args: %s; env keys: %s",
		s.Name, s.ProjectRoot, cmd, args, env)
}

// pendingTools returns the approval tools of the pending servers still unasked on the thread, and connects the
// servers approved since the configuration was loaded (the approval of an earlier run).
func (p *Provider) pendingTools(ctx context.Context, rc tools.RunContext) (offer []tool.BaseTool, approved []config.MCPServer) {
	for _, s := range p.Pending {
		ok, err := config.MCPApproved(p.TrustDir, s.ProjectRoot, s)
		switch {
		case err != nil:
			continue
		case ok:
			approved = append(approved, s)
		case p.Ask && p.TrustDir != "" && !p.asked(ctx, rc.ThreadID, approveToolName(s)):
			offer = append(offer, &approveTool{server: s, trustDir: p.TrustDir, name: approveToolName(s)})
		}
	}
	return offer, approved
}

// asked reports that the thread already holds a result of that approval tool (a denial, or a cancelled question), or
// that its history cannot be read (fail closed: no new question). A call still waiting for its decision has no result.
func (p *Provider) asked(ctx context.Context, threadID, name string) bool {
	if threadID == "" || p.History == nil {
		return false
	}
	msgs, err := p.History(ctx, threadID)
	if err != nil {
		return true
	}
	var ids []string
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			if c.Function.Name == name {
				ids = append(ids, c.ID)
			}
		}
	}
	for _, m := range msgs {
		if m.Role == store.RoleTool && slices.Contains(ids, m.ToolCallID) {
			return true
		}
	}
	return false
}

// approveTool asks the user to approve running a project's stdio server.
type approveTool struct {
	server   config.MCPServer
	trustDir string
	name     string
}

func (t *approveTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: t.name,
		Desc: fmt.Sprintf("The project defines the MCP server %q (a local program) that is not approved yet, so its tools are missing. "+
			"Call this tool, with no arguments, to ask the user to approve running it; if the task needs that server, call it first. "+
			"If the user denies, do not ask again; its tools then become available in the run after an approval.", t.server.Name),
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{}),
	}, nil
}

// InvokableRun runs only once the user approved the call (the approval gate pauses it, and refuses it where the
// user cannot be asked): it records the approval of the exact command.
func (t *approveTool) InvokableRun(context.Context, string, ...tool.Option) (string, error) {
	if _, err := config.ApproveMCP(t.trustDir, t.server.ProjectRoot, t.server); err != nil {
		return "", tools.Errorf("the approval could not be recorded: %v", err)
	}
	return fmt.Sprintf("The user approved the MCP server %q. Its tools are available from the next run (the user's next message); they are not in this run.", t.server.Name), nil
}
