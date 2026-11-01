// Package mcpfeature is the implementation of harness.MCP(): the tools of the MCP servers of the configuration
// (MCP_SERVERS, .mcp.json, mcp.servers in config.yml), connected per run.
//
// Import it for its side effect in a program that runs MCP servers:
//
//	import _ "github.com/7ar-chat/harness/mcpfeature"
//
// The harness package does not import it (and so does not link the MCP SDK), which keeps programs that use no MCP
// servers smaller. harness.New fails, naming this package, when servers are configured and it is not imported.
package mcpfeature

import (
	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/engine/approvals"
	"github.com/7ar-chat/harness/features/mcp"
	"github.com/7ar-chat/harness/features/permissions"
)

func init() { harness.RegisterMCP(install) }

func install(b *harness.Builder) error {
	var pending []config.MCPServer
	if b.Config.Layers != nil {
		pending = b.Config.Layers.MCPPending
	}
	p := mcptools.NewWithPending(b.Config.MCPServers, pending, b.Options.Version, mcptools.MaxResultChars)
	if p == nil && b.Options.MCPServers != nil {
		p = mcptools.NewDynamic(nil, b.Options.Version, mcptools.MaxResultChars)
	}
	if p == nil {
		return nil
	}
	if b.Options.MCPServers != nil {
		p.Dynamic = b.Options.MCPServers
	}
	p.CallTimeout = b.Config.MCPCallTimeout()
	if b.Config.Layers != nil {
		p.TrustDir = b.Config.Layers.UserDir
	}
	if b.Store != nil {
		p.History = b.Store.ListMessages
	}
	b.AddTools(p)
	// A pending project server is only offered for approval where the approval gate asks the user.
	b.Configure(func(hs *agent.Harness) {
		for _, h := range hs.BeforeTool {
			if hk, ok := h.(*permissions.Hook); ok && hk.Inner != nil {
				h = hk.Inner
			}
			if g, ok := h.(*approvals.Gate); ok {
				g.Describe = p.Describe
				p.Ask = true
			}
		}
	})
	return nil
}
