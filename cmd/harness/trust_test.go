package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/config"
)

// project makes a git repository with a .har directory and moves the test into it.
func project(t *testing.T, files map[string]string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	t.Setenv("LOCAL_WORKSPACE", "")
	t.Setenv(config.TrustEnv, "")
	return root
}

func TestTrustUntrustAndListWorkOnTheCurrentProjectOrAPath(t *testing.T) {
	// Arrange
	home := harHome(t)
	root := project(t, map[string]string{".har/config.yml": "limits:\n  max_steps: 3\n"})
	other, _ := filepath.EvalSymlinks(t.TempDir())

	// Act
	code, out, errOut := cli(t, "", "trust")
	codeAgain, outAgain, _ := cli(t, "", "trust")
	codePath, _, _ := cli(t, "", "trust", other)
	_, list, _ := cli(t, "", "trust", "list")
	codeUn, outUn, _ := cli(t, "", "untrust")
	_, listAfter, _ := cli(t, "", "trust", "list")
	codeGhost, _, errGhost := cli(t, "", "untrust")
	codeNoDir, _, _ := cli(t, "", "trust", filepath.Join(other, "missing"))

	// Assert
	if code != 0 || !strings.Contains(out, root) || !strings.Contains(out, filepath.Join(home, "trust.json")) {
		t.Fatalf("trust: %d %q %q", code, out, errOut)
	}
	if codeAgain != 0 || !strings.Contains(outAgain, "already trusted") || codePath != 0 {
		t.Fatalf("again %d %q path %d", codeAgain, outAgain, codePath)
	}
	if !strings.Contains(list, root) || !strings.Contains(list, other) {
		t.Fatalf("list:\n%s", list)
	}
	if codeUn != 0 || !strings.Contains(outUn, root) || strings.Contains(listAfter, root) || !strings.Contains(listAfter, other) {
		t.Fatalf("untrust %d %q\n%s", codeUn, outUn, listAfter)
	}
	if codeGhost == 0 || !strings.Contains(errGhost, "not trusted") || codeNoDir == 0 {
		t.Fatalf("ghost %d %q nodir %d", codeGhost, errGhost, codeNoDir)
	}
	ok, err := config.IsTrusted(home, other)
	if err != nil || !ok {
		t.Fatalf("trusted %v %v", ok, err)
	}
}

func TestConfigDumpReportsWhatAnUntrustedProjectCouldNotDo(t *testing.T) {
	// Arrange
	harHome(t)
	project(t, map[string]string{
		".har/config.yml":      "permissions:\n  allow: [\"Bash(make*)\"]\n  mode: yolo\n",
		".har/permissions.yml": "allow:\n  - git push*\nmode: auto-edit\nask:\n  - make deploy*\n",
		".mcp.json":            `{"mcpServers":{"docs":{"type":"http","url":"https://docs.example.com/mcp"}}}`,
		".har/mcp.json":        `{"mcpServers":{"fs":{"command":"npx","args":["x"]}}}`,
	})

	// Act
	code, out, errOut := cli(t, "", "-config-dump")
	t.Setenv(config.TrustEnv, "1")
	codeT, outT, _ := cli(t, "", "-config-dump")

	// Assert
	if code != 0 || codeT != 0 {
		t.Fatalf("codes %d %d: %s", code, codeT, errOut)
	}
	for _, want := range []string{
		"project not trusted", "ignored:", "permissions.allow (1 rule(s)) ignored", `permissions.mode "yolo" ignored`,
		"ignored (permissions file):", "1 allow rule(s) ignored", `mode "auto-edit" ignored`,
		"mcp servers not started:", "docs", "fs", "run `harness trust` to enable",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dump lacks %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"ignored:", "not trusted", "run `harness trust` to enable", "ignored (permissions file)"} {
		if strings.Contains(outT, bad) {
			t.Errorf("trusted dump has %q:\n%s", bad, outT)
		}
	}
	if !strings.Contains(outT, "project trusted") || !strings.Contains(outT, "docs") {
		t.Errorf("trusted dump:\n%s", outT)
	}
}

func TestConfigInitStarterShowsTheUserFileLayout(t *testing.T) {
	// Arrange
	home := harHome(t)

	// Act
	code, _, errOut := cli(t, "", "config", "init")

	// Assert
	data, err := os.ReadFile(filepath.Join(home, "config.yml"))
	if code != 0 || err != nil {
		t.Fatalf("%d %v %s", code, err, errOut)
	}
	for _, want := range []string{"providers:", "# agents:", "explore:", "compaction:", "a project's .har/config.yml can only choose"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("starter lacks %q", want)
		}
	}
}

func TestTrustMCPApprovesTheExactCommandOfAProjectStdioServer(t *testing.T) {
	// Arrange: a trusted project with a stdio server, stdio allowed on the local backend.
	home := harHome(t)
	t.Setenv("SANDBOX_BACKEND", "local")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.yml"), []byte("mcp:\n  allow_stdio: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := project(t, map[string]string{".mcp.json": `{"mcpServers":{"fs":{"command":"echo","args":["a"]}}}`})

	// Act
	codeUntrusted, _, errUntrusted := cli(t, "", "trust", "mcp")
	cli(t, "", "trust")
	code, out, errOut := cli(t, "", "trust", "mcp", "fs")
	codeAgain, _, _ := cli(t, "", "trust", "mcp")
	cfg, err := config.LoadWith(config.LoadOptions{})
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"), []byte(`{"mcpServers":{"fs":{"command":"echo","args":["b"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err2 := config.LoadWith(config.LoadOptions{})
	codeRevoke, _, _ := cli(t, "", "untrust", "mcp", "fs")

	// Assert
	if codeUntrusted == 0 || !strings.Contains(errUntrusted, "not trusted") {
		t.Fatalf("an untrusted project must not get approvals: %d %q", codeUntrusted, errUntrusted)
	}
	if code != 0 || !strings.Contains(out, "approved MCP server fs") || codeAgain == 0 {
		t.Fatalf("approve: %d %q %q (again %d)", code, out, errOut, codeAgain)
	}
	if err != nil || len(cfg.MCPServers) != 1 || err2 != nil || len(changed.MCPServers) != 0 || len(changed.Layers.MCPPending) != 1 {
		t.Fatalf("approved %v/%v, after the command changed %v/%v", err, cfg.MCPServers, err2, changed.MCPServers)
	}
	if codeRevoke != 0 {
		t.Fatal("the stale approval of the server name must still be withdrawable")
	}
}
