package config_test

import (
	"testing"

	"github.com/7ar-chat/harness/config"
)

func TestBrowserURLAddsTheBrowserServerWithTheSafeToolList(t *testing.T) {
	cfg, err := config.LoadWith(config.LoadOptions{NoFiles: true, Environ: []string{"HARNESS_BROWSER_URL=http://browser:8931/mcp"}})
	if err != nil {
		t.Fatal(err)
	}
	var found *config.MCPServer
	for i := range cfg.MCPServers {
		if cfg.MCPServers[i].Name == "browser" {
			found = &cfg.MCPServers[i]
		}
	}
	if found == nil || found.URL != "http://browser:8931/mcp" {
		t.Fatalf("servers = %+v", cfg.MCPServers)
	}
	for _, n := range found.Tools {
		if n == "browser_run_code_unsafe" || n == "browser_evaluate" {
			t.Fatalf("%s must not be offered", n)
		}
	}
	if len(found.Tools) == 0 {
		t.Fatal("no tool allow-list: every tool would be offered")
	}
}

func TestMCPServersEntryNamedBrowserWins(t *testing.T) {
	cfg, err := config.LoadWith(config.LoadOptions{NoFiles: true, Environ: []string{
		"HARNESS_BROWSER_URL=http://browser:8931/mcp", `MCP_SERVERS=[{"name":"browser","url":"http://other:1/mcp"}]`}})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.MCPServers) != 1 || cfg.MCPServers[0].URL != "http://other:1/mcp" {
		t.Fatalf("servers = %+v", cfg.MCPServers)
	}
}

func TestBrowserBackendChoosesWhereTheBrowserRuns(t *testing.T) {
	load := func(env ...string) *config.Config {
		t.Helper()
		cfg, err := config.LoadWith(config.LoadOptions{NoFiles: true, Environ: env})
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	hasBrowser := func(c *config.Config) bool {
		for _, s := range c.MCPServers {
			if s.Name == "browser" {
				return true
			}
		}
		return false
	}
	// Default: the always-on service.
	if c := load("HARNESS_BROWSER_URL=http://browser:8931/mcp"); !hasBrowser(c) || c.BrowserSandboxBackend() != "" {
		t.Fatal("without BROWSER_BACKEND the service at HARNESS_BROWSER_URL is the browser")
	}
	// A sandbox provider replaces the service, whatever the code sandboxes use.
	c := load("HARNESS_BROWSER_URL=http://browser:8931/mcp", "BROWSER_BACKEND=E2B", "SANDBOX_BACKEND=daytona")
	if hasBrowser(c) || c.BrowserSandboxBackend() != "e2b" || c.SandboxBackendName() != "daytona" {
		t.Fatalf("browser %q, code %q, static browser server: %v", c.BrowserSandboxBackend(), c.SandboxBackendName(), hasBrowser(c))
	}
	// "none" switches the browser off, "service" is the default.
	if c := load("HARNESS_BROWSER_URL=http://browser:8931/mcp", "BROWSER_BACKEND=none"); hasBrowser(c) {
		t.Fatal("BROWSER_BACKEND=none must give no browser")
	}
	if c := load("HARNESS_BROWSER_URL=http://browser:8931/mcp", "BROWSER_BACKEND=service"); !hasBrowser(c) {
		t.Fatal("BROWSER_BACKEND=service is the default")
	}
	if _, err := config.LoadWith(config.LoadOptions{NoFiles: true, Environ: []string{"BROWSER_BACKEND=chromeless"}}); err == nil {
		t.Fatal("an unknown BROWSER_BACKEND must be refused")
	}
}

func TestBrowserSandboxConfigIsOpenToTheInternetAndUsesTheBrowserImage(t *testing.T) {
	cfg, err := config.LoadWith(config.LoadOptions{NoFiles: true, Environ: []string{
		"BROWSER_BACKEND=e2b", "SANDBOX_BACKEND=daytona", "SANDBOX_EGRESS_ALLOWLIST=github.com", "SANDBOX_IMAGES=coding=harness-base",
		"DAYTONA_SNAPSHOT=code-snap", "E2B_TEMPLATE=code-tpl", "BROWSER_IMAGE=my-browser", "HARNESS_PRODUCTION=1"}})
	if err != nil {
		t.Fatal(err)
	}
	b := cfg.BrowserSandboxConfig()
	if b.SandboxBackendName() != "e2b" || b.SandboxImages["browser"] != "my-browser" || len(b.SandboxImages) != 1 {
		t.Fatalf("backend %q, images %v", b.SandboxBackendName(), b.SandboxImages)
	}
	if mode, err := b.EgressMode(); err != nil || len(b.SandboxEgressAllowlist) != 0 {
		t.Fatalf("a browser has to reach any site, even in production: mode %q, err %v, allowlist %v", mode, err, b.SandboxEgressAllowlist)
	}
	// The code sandboxes' own settings are untouched.
	if cfg.SandboxBackendName() != "daytona" || len(cfg.SandboxEgressAllowlist) != 1 || cfg.SandboxImages["coding"] != "harness-base" {
		t.Fatalf("the code configuration changed: %q %v %v", cfg.SandboxBackendName(), cfg.SandboxEgressAllowlist, cfg.SandboxImages)
	}
}
