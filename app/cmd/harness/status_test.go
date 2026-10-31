package main

import (
	"strings"
	"testing"

	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/hosts/tui"
)

func TestStatusItemsNameTheLayersAndNeverShowSecrets(t *testing.T) {
	// Arrange
	cfg := &config.Config{
		LLMProvider:      "openai",
		LLMBaseURL:       "https://user:hunter2@llm.example.com/v1?key=abc",
		LLMAPIKey:        "sk-very-secret",
		LLMKeySource:     "env LLM_API_KEY",
		AgentPermissions: "auto-edit",
		MCPServers: []config.MCPServer{
			{Name: "fs", Command: "/usr/bin/mcp-fs", Args: []string{"--token", "tok-secret"}, Env: map[string]string{"K": "env-secret"}, Origin: "~/.har/mcp.json"},
			{Name: "docs", URL: "https://docs.example.com/mcp?auth=url-secret", Headers: map[string]string{"Authorization": "Bearer hdr-secret"}, Tools: []string{"a", "b"}, Origin: "project"},
		},
		Layers: &config.Layers{
			UserFile: "/home/u/.har/config.yml", ProjectFile: "/work/.har/config.yml", ProjectRoot: "/work",
			Sources:    map[string]config.Source{"LLM_API_KEY": config.SourceEnv, "AGENT_PERMISSIONS": config.SourceUser},
			MCPSkipped: []string{"slow (stdio server in an untrusted project)"},
		},
	}

	// Act
	items := statusItems(cfg)

	// Assert
	var text strings.Builder
	by := map[string]string{}
	for _, it := range items {
		text.WriteString(it.Section + "|" + it.Label + "|" + it.Value + "\n")
		by[it.Section+"/"+it.Label] = it.Value
	}
	for _, secret := range []string{"hunter2", "sk-very-secret", "abc", "tok-secret", "env-secret", "url-secret", "hdr-secret"} {
		if strings.Contains(text.String(), secret) {
			t.Errorf("secret %q in the status:\n%s", secret, text.String())
		}
	}
	for key, want := range map[string]string{
		tui.SectionConfig + "/config.yml · user":    "/home/u/.har/config.yml",
		tui.SectionConfig + "/config.yml · project": "/work/.har/config.yml",
		tui.SectionConfig + "/permissions":          "auto-edit",
		tui.SectionConfig + "/base url":             "https://llm.example.com",
		tui.SectionConfig + "/api key":              "set, from env LLM_API_KEY",
		tui.SectionMCP + "/fs":                      "stdio: mcp-fs (+2 args) (~/.har/mcp.json)",
		tui.SectionMCP + "/docs":                    "http: https://docs.example.com (project), 2 tools allowed",
		tui.SectionMCP + "/not started":             "slow (stdio server in an untrusted project)",
	} {
		if got := by[key]; got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if env := by[tui.SectionConfig+"/environment"]; env != "1 settings: LLM_API_KEY" {
		t.Errorf("environment = %q", env)
	}
}
