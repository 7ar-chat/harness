package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/config"
)

func TestConnectorSavesTheKeyInAuthJSONWithMode0600AndListsProviders(t *testing.T) {
	dir := t.TempDir()
	c := &authConnector{dir: dir, providers: map[string]config.Provider{"opencode": {Name: "opencode", BaseURL: "https://x.test/v1"}}}

	if err := c.Save("opencode", "sk-secret-value-1234"); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(dir, config.AuthFile))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
	var got []string
	for _, p := range c.Providers() {
		got = append(got, p.Name+"="+p.BaseURL+map[bool]string{true: "+key", false: ""}[p.HasKey])
	}
	if want := "openai=,anthropic=,opencode=https://x.test/v1+key"; strings.Join(got, ",") != want {
		t.Fatalf("providers %q, want %q", strings.Join(got, ","), want)
	}
	if !c.RestartNeeded() {
		t.Fatal("an in-process engine cannot pick the key up while running")
	}
}

func TestConnectorRejectsBadNamesAndEmptyKeysWithoutEchoingTheKey(t *testing.T) {
	c := &authConnector{dir: t.TempDir()}

	if err := c.Save("bad name", "sk-secret-value-1234"); err == nil || strings.Contains(err.Error(), "sk-secret") {
		t.Fatalf("error %v", err)
	}
	if err := c.Save("openai", "  "); err == nil {
		t.Fatal("an empty key was stored")
	}
	if err := (&authConnector{}).Save("openai", "k"); err == nil {
		t.Fatal("saved without a home directory")
	}
}
