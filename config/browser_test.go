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
