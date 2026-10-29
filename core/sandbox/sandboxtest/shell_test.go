package sandboxtest_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/core/sandbox"
	"github.com/GhaziBenDahmane/harness/core/sandbox/sandboxtest"
)

func TestOnlySandboxPathsAreMappedNotAHostPathHoldingWorkspace(t *testing.T) {
	// ShellExec and HostSandbox map the sandbox's /workspace paths of a command but leave alone a host script under a .../workspace/... checkout (the CI's) and /workspacefoo.
	// Arrange
	tmp := t.TempDir()
	script := filepath.Join(tmp, "agent", "workspace", "job", "check.sh")
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("printf '%s\\n' \"$1\"; cat \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	command := "bash " + script + " '/workspace/outputs/report.txt' && cd /workspace/outputs && echo $PWD /workspacefoo"
	shell := sandboxtest.NewSandbox("s")
	shell.SetLineTransport(false)
	shell.Put("/workspace/outputs/report.txt", "report\n")
	shell.SetExec(sandboxtest.ShellExec(shell))
	host := sandboxtest.NewHostSandbox("h", filepath.Join(tmp, "sbx"))
	if err := host.WriteFile(context.Background(), "/workspace/outputs/report.txt", []byte("report\n")); err != nil {
		t.Fatal(err)
	}
	run := func(sb sandbox.Sandbox) (string, int, error) {
		var out strings.Builder
		res, err := sb.Exec(context.Background(), sandbox.ExecRequest{Command: command, OnStdout: func(s string) { out.WriteString(s) }})
		return out.String(), res.ExitCode, err
	}

	// Act
	shellOut, shellCode, shellErr := run(shell)
	hostOut, hostCode, hostErr := run(host)

	// Assert
	want := "/workspace/outputs/report.txt\nreport\n/workspace/outputs /workspacefoo\n"
	if shellErr != nil || shellCode != 0 || shellOut != want {
		t.Fatalf("ShellExec %q exit %d %v", shellOut, shellCode, shellErr)
	}
	if hostErr != nil || hostCode != 0 || hostOut != want {
		t.Fatalf("HostSandbox %q exit %d %v", hostOut, hostCode, hostErr)
	}
}
