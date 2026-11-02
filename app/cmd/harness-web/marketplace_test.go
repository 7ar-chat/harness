package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/capstore"
	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/app/product"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/capabilities"
	mcptools "github.com/7ar-chat/harness/features/mcp"
)

func TestTeamModeSeedsTheCatalogAndKeepsTheMarketplaceInTheStateDirectory(t *testing.T) {
	// Arrange
	wbEnv(t)
	dir := t.TempDir()
	t.Setenv("LOCAL_STATE_DIR", dir)
	ctx := context.Background()

	// Act
	team, err := buildWith(ctx, wbConfig(t), false, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOCAL_STATE_DIR", t.TempDir()) // one process owns a state directory
	solo, err := buildWith(ctx, wbConfig(t), true, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = solo.closer.Close() })

	// Assert: the shipped catalog is in the store, and the marketplace is a team feature
	if team.caps == nil || solo.caps != nil {
		t.Fatalf("caps: team %v, solo %v", team.caps, solo.caps)
	}
	catalog, err := team.caps.ListCatalog(ctx)
	if err != nil || len(catalog) != len(product.Catalog()) {
		t.Fatalf("catalog %d entries (%v), want %d", len(catalog), err, len(product.Catalog()))
	}
	if _, err := team.caps.PutInstall(ctx, capabilities.Install{Scope: capabilities.ScopeUser, ScopeID: "alice", Name: "mine", URL: "https://a.example/mcp", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := team.closer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "capabilities.json")); err != nil {
		t.Fatalf("no state file beside team.json: %v", err)
	}
}

func TestInstallMarketplaceWiresTheSourceAndSurvivesASeedFailure(t *testing.T) {
	// Arrange
	t.Setenv("HARNESS_SECRET_KEY", "k")
	ctx := context.Background()
	var o harness.Options

	// Act / Assert: no store, no source
	if err := installMarketplace(ctx, &o, teamstore.NewMem(), nil); err != nil || o.MCPServers != nil {
		t.Fatalf("without a store: %v, %v", err, o.MCPServers != nil)
	}
	// A catalog that cannot be seeded is logged: the installs already stored keep working.
	if err := installMarketplace(ctx, &o, teamstore.NewMem(), seedFails{capstore.NewMem(nil)}); err != nil || o.MCPServers == nil {
		t.Fatalf("seed failure: %v, source set %v", err, o.MCPServers != nil)
	}
	// A store the source cannot be built over is an error.
	if err := installMarketplace(ctx, &harness.Options{}, nil, capstore.NewMem(nil)); err == nil {
		t.Fatal("a missing team store was accepted")
	}
}

type seedFails struct{ *capstore.Mem }

func (seedFails) SeedCatalog(context.Context, []capabilities.Entry) error { return errors.New("boom") }

func TestTheMarketplaceTesterMapsRejectedCredentialsAndListsTools(t *testing.T) {
	// Arrange
	srv := config.MCPServer{Name: "x", URL: "https://x.example/mcp", Untrusted: true}
	run := func(infos []mcptools.ToolInfo, err error) ([]product.CapabilityTool, error) {
		return testerOver(func(_ context.Context, got config.MCPServer) ([]mcptools.ToolInfo, error) {
			if got.URL != srv.URL {
				t.Errorf("the server was changed: %+v", got)
			}
			return infos, err
		})(context.Background(), srv)
	}

	// Act
	ok, okErr := run([]mcptools.ToolInfo{{Name: "a", Description: "d", ReadOnly: true}}, nil)
	_, rejected := run(nil, errors.Join(mcptools.ErrRejected, errors.New("body")))
	_, other := run(nil, errors.New("dial tcp: refused"))

	// Assert
	if okErr != nil || len(ok) != 1 || ok[0] != (product.CapabilityTool{Name: "a", Description: "d", ReadOnly: true}) {
		t.Fatalf("tools %+v, err %v", ok, okErr)
	}
	if !errors.Is(rejected, product.ErrCredentialsRejected) || other == nil || errors.Is(other, product.ErrCredentialsRejected) {
		t.Fatalf("rejected %v, other %v", rejected, other)
	}
	// The real lister refuses a server without an address before it connects anywhere.
	if _, err := mcpTester("web")(context.Background(), srv0()); err == nil {
		t.Fatal("a server without a URL was listed")
	}
}

func srv0() config.MCPServer { return config.MCPServer{Name: "x", Untrusted: true} }
