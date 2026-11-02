package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/app/product"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/capabilities"
	"github.com/7ar-chat/harness/core/store"
	mcptools "github.com/7ar-chat/harness/features/mcp"
)

// installMarketplace seeds the shipped catalog into caps and makes the harness read the MCP servers of the run's
// project or user from it. A catalog that cannot be seeded is logged: the installs already stored keep working.
func installMarketplace(ctx context.Context, o *harness.Options, ts store.TeamStore, caps capabilities.Store) error {
	if caps == nil {
		return nil
	}
	if err := caps.SeedCatalog(ctx, product.Catalog()); err != nil {
		slog.Error("marketplace: seeding the catalog failed", "error", err)
	}
	src, err := product.NewMCPSource(ts, caps, nil, product.SecretKeyFromEnv(os.Getenv))
	if err != nil {
		return err
	}
	o.MCPServers = src
	return nil
}

// mcpTester is how the marketplace's "test" action reaches a server: through the guarded transport of features/mcp.
func mcpTester(version string) product.MCPTester {
	return testerOver(func(ctx context.Context, s config.MCPServer) ([]mcptools.ToolInfo, error) {
		return mcptools.ListTools(ctx, s, version, 10*time.Second)
	})
}

// testerOver adapts a tool lister to the product's tester: an authentication failure becomes the product's
// rejected-credentials error, the rest passes on as it is (the product shows none of it).
func testerOver(list func(context.Context, config.MCPServer) ([]mcptools.ToolInfo, error)) product.MCPTester {
	return func(ctx context.Context, s config.MCPServer) ([]product.CapabilityTool, error) {
		infos, err := list(ctx, s)
		if errors.Is(err, mcptools.ErrRejected) {
			return nil, product.ErrCredentialsRejected
		}
		if err != nil {
			return nil, err
		}
		out := make([]product.CapabilityTool, 0, len(infos))
		for _, t := range infos {
			out = append(out, product.CapabilityTool{Name: t.Name, Description: t.Description, ReadOnly: t.ReadOnly})
		}
		return out, nil
	}
}
