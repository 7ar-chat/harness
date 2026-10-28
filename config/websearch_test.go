package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/config"
)

func TestWebSearchIsOffWithoutURLAndValidatesItsSettings(t *testing.T) {
	// Defaults: web search off, legacy mode; a bad mode, a bad URL or a WORK_MCP_SERVERS entry named websearch stop the service.
	// Arrange / Act
	cfg, err := config.Load()

	// Assert
	if err != nil || cfg.ManagedTokenMode != config.TokenModeLegacy || !cfg.WebSearchEnabled || !errors.Is(cfg.WebSearchStatus(), config.ErrWebSearchOff) {
		t.Fatalf("err %v cfg %+v", err, cfg.WebSearch)
	}
	for name, env := range map[string]map[string]string{
		"mode":     {"WORK_MANAGED_TOKEN_MODE": "magic"},
		"url":      {"WEB_SEARCH_MCP_URL": "ftp://mcp"},
		"reserved": {"WEB_SEARCH_MCP_URL": "https://mcp/websearch", "WORK_MCP_SERVERS": `[{"name":"websearch","url":"http://x"}]`},
	} {
		t.Run(name, func(t *testing.T) {
			for k, v := range env {
				t.Setenv(k, v)
			}
			if _, err := config.Load(); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

func TestWebSearchStatusRefusesUnsafeOrIncompleteSetups(t *testing.T) {
	// Production needs WORK_TOKEN_ENCRYPTION_KEY; token_exchange needs every WORK_TOKEN_EXCHANGE_* value; the kill switch turns it off.
	cases := []struct {
		name string
		env  map[string]string
		want string // "" ok, "off", or an error substring
	}{
		{"dev without key", map[string]string{}, ""},
		{"prod without key", map[string]string{"DJANGO_PRODUCTION": "1"}, "WORK_TOKEN_ENCRYPTION_KEY"},
		{"prod with key", map[string]string{"DJANGO_PRODUCTION": "1", "WORK_TOKEN_ENCRYPTION_KEY": "k"}, ""},
		{"prod over http", map[string]string{"DJANGO_PRODUCTION": "1", "WORK_TOKEN_ENCRYPTION_KEY": "k", "WEB_SEARCH_MCP_URL": "http://mcp/websearch"}, "https"},
		{"dev over http", map[string]string{"WEB_SEARCH_MCP_URL": "http://localhost:8080/mcp"}, ""},
		{"kill switch", map[string]string{"WEB_SEARCH_ENABLED": "false"}, "off"},
		{"exchange incomplete", map[string]string{"WORK_MANAGED_TOKEN_MODE": "token_exchange", "WORK_TOKEN_EXCHANGE_AUDIENCE": "a"},
			"WORK_TOKEN_EXCHANGE_CLIENT_ID, WORK_TOKEN_EXCHANGE_CLIENT_SECRET, WORK_TOKEN_EXCHANGE_SCOPE, WORK_TOKEN_EXCHANGE_TOKEN_ENDPOINT"},
		{"exchange complete", map[string]string{"WORK_MANAGED_TOKEN_MODE": "token_exchange", "WORK_TOKEN_EXCHANGE_AUDIENCE": "a",
			"WORK_TOKEN_EXCHANGE_SCOPE": "s", "WORK_TOKEN_EXCHANGE_CLIENT_ID": "c", "WORK_TOKEN_EXCHANGE_CLIENT_SECRET": "x",
			"WORK_TOKEN_EXCHANGE_TOKEN_ENDPOINT": "https://idp/token"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			t.Setenv("WEB_SEARCH_MCP_URL", "https://mcp.example.com/mcp/websearch")
			t.Setenv("SANDBOX_EGRESS_UNRESTRICTED", "true")
			for k, v := range tc.env { // may override WEB_SEARCH_MCP_URL
				t.Setenv(k, v)
			}
			cfg, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}

			// Act
			status := cfg.WebSearchStatus()

			// Assert
			switch {
			case tc.want == "" && status != nil, tc.want == "off" && !errors.Is(status, config.ErrWebSearchOff),
				tc.want != "" && tc.want != "off" && (status == nil || !strings.Contains(status.Error(), tc.want)):
				t.Fatalf("status %v want %q", status, tc.want)
			}
		})
	}
}
