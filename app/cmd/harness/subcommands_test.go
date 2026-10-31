package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// cli runs the command in process and returns the exit code and both outputs.
func cli(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(context.Background(), args, strings.NewReader(stdin), &out, &errOut, false, scripted())
	return code, out.String(), errOut.String()
}

func harHome(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "har")
	t.Setenv("HAR_HOME", dir)
	t.Setenv("HARNESS_NO_USER_DIR", "")
	for _, k := range []string{"LLM_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "LLM_PROVIDER"} {
		t.Setenv(k, "")
	}
	return dir
}

func TestAuthLoginListRemove(t *testing.T) {
	// Arrange
	home := harHome(t)

	// Act
	code, out, errOut := cli(t, "sk-live-abcdef123456\n", "auth", "login", "opencode", "-key-stdin")
	code2, out2, _ := cli(t, "sk-other-zzzzzzzz9999\n", "auth", "login", "-key-stdin", "anthropic")
	codeL, list, errL := cli(t, "", "auth", "list")

	// Assert
	if code != 0 || code2 != 0 || codeL != 0 {
		t.Fatalf("codes %d %d %d: %s %s", code, code2, codeL, errOut, errL)
	}
	file := filepath.Join(home, "auth.json")
	if !strings.Contains(out, file) || strings.Contains(out+out2+list, "sk-live-abcdef123456") || strings.Contains(out+out2+list, "sk-other-zzzzzzzz9999") {
		t.Fatalf("full key printed or file missing:\n%s%s%s", out, out2, list)
	}
	if !strings.Contains(list, "opencode") || !strings.Contains(list, "****3456") || !strings.Contains(list, "****9999") || !strings.Contains(list, file) {
		t.Fatalf("list:\n%s", list)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(file)
		ds, _ := os.Stat(home)
		if st.Mode().Perm() != 0o600 || ds.Mode().Perm() != 0o700 {
			t.Fatalf("modes file %v dir %v", st.Mode().Perm(), ds.Mode().Perm())
		}
	}
	if code, _, e := cli(t, "", "auth", "remove", "opencode"); code != 0 {
		t.Fatal(e)
	}
	_, list, _ = cli(t, "", "auth", "list")
	if strings.Contains(list, "opencode") || !strings.Contains(list, "anthropic") {
		t.Fatalf("after remove:\n%s", list)
	}
	if code, _, e := cli(t, "", "auth", "remove", "ghost"); code == 0 || !strings.Contains(e, "ghost") {
		t.Fatalf("removing a missing key: %d %s", code, e)
	}
}

func TestAuthLoginRejectsAnEmptyKeyAndABadName(t *testing.T) {
	harHome(t)

	if code, _, _ := cli(t, "\n", "auth", "login", "p", "-key-stdin"); code == 0 {
		t.Fatal("empty key accepted")
	}
	if code, _, _ := cli(t, "k\n", "auth", "login", "../x", "-key-stdin"); code == 0 {
		t.Fatal("bad name accepted")
	}
	if code, _, _ := cli(t, "", "auth"); code != 2 {
		t.Fatalf("usage code %d", code)
	}
}

func TestAuthListRefusesALooseFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix modes")
	}
	home := harHome(t)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(home, "auth.json")
	if err := os.WriteFile(file, []byte(`{"providers":{"p":{"api_key":"sk-secret-key-1"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := cli(t, "", "auth", "list")

	if code == 0 || !strings.Contains(errOut, file) || !strings.Contains(errOut, "chmod 600") || strings.Contains(out+errOut, "sk-secret") {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
}

func TestConfigPathAndInit(t *testing.T) {
	home := harHome(t)

	code, out, _ := cli(t, "", "config", "path")
	codeI, outI, errI := cli(t, "", "config", "init")
	codeI2, outI2, _ := cli(t, "", "config", "init")

	if code != 0 || !strings.Contains(out, "user dir:    "+home) || !strings.Contains(out, "project dir:") {
		t.Fatalf("path: %d %q", code, out)
	}
	file := filepath.Join(home, "config.yml")
	if codeI != 0 || codeI2 != 0 || !strings.Contains(outI, file) || !strings.Contains(outI2, "already exists") {
		t.Fatalf("init: %d %q %q %q", codeI, outI, errI, outI2)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(file)
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", st.Mode().Perm())
		}
	}
	// The starter file must itself be loadable.
	if code, _, e := cli(t, "", "-config-dump"); code != 0 {
		t.Fatalf("starter config rejected: %s", e)
	}
}

func TestConfigPathReportsADisabledUserDir(t *testing.T) {
	harHome(t)
	t.Setenv("HARNESS_NO_USER_DIR", "1")

	_, out, _ := cli(t, "", "config", "path")

	if !strings.Contains(out, "HARNESS_NO_USER_DIR") {
		t.Fatal(out)
	}
}

func TestConfigDumpShowsTheKeySourceAndTheProviderFlagNeverTheKey(t *testing.T) {
	home := harHome(t)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := "providers:\n  zen:\n    base_url: https://zen.example/v1\n    default_model: zm\n"
	if err := os.WriteFile(filepath.Join(home, "config.yml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, e := cli(t, "sk-zen-secret-0042\n", "auth", "login", "zen", "-key-stdin"); code != 0 {
		t.Fatal(e)
	}

	code, out, errOut := cli(t, "", "-config-dump", "-provider", "zen", "-model", "zm")

	if code != 0 {
		t.Fatalf("%d %s", code, errOut)
	}
	if strings.Contains(out, "sk-zen-secret") || !strings.Contains(out, "(auth.json)") || !strings.Contains(out, "https://zen.example/v1") || !strings.Contains(out, "-provider") {
		t.Fatalf("dump:\n%s", out)
	}
}

func TestHarnessNoUserDirHidesTheStoredKeyFromTheCommand(t *testing.T) {
	harHome(t)
	if code, _, e := cli(t, "sk-hidden-key-0001\n", "auth", "login", "openai", "-key-stdin"); code != 0 {
		t.Fatal(e)
	}
	t.Setenv("HARNESS_NO_USER_DIR", "1")

	_, out, _ := cli(t, "", "-config-dump")

	if strings.Contains(out, "LLM_API_KEY") {
		t.Fatalf("auth.json read despite HARNESS_NO_USER_DIR:\n%s", out)
	}
}

func TestTheRealFactoryExplainsAMissingKey(t *testing.T) {
	harHome(t)
	var out, errOut bytes.Buffer

	code := run(context.Background(), []string{"-p", "hi", "-ephemeral", "-provider", "anthropic"}, strings.NewReader(""), &out, &errOut, false, newHarness)

	if code != 1 || !strings.Contains(errOut.String(), "harness auth login anthropic") || !strings.Contains(errOut.String(), "ANTHROPIC_API_KEY") {
		t.Fatalf("code %d: %s", code, errOut.String())
	}
}
