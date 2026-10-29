package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/hosts/tui"
)

// statusFunc describes the configuration in effect for /status and /mcp: the
// files and the layer each comes from, the provider, the permission mode and
// the MCP servers. Secrets never appear (the API key is named by its source,
// URLs are cut to their host, commands to their program).
func statusFunc(cfg *config.Config) func(context.Context) ([]tui.StatusItem, error) {
	return func(context.Context) ([]tui.StatusItem, error) { return statusItems(cfg), nil }
}

func statusItems(cfg *config.Config) []tui.StatusItem {
	var items []tui.StatusItem
	add := func(section, label, value string) {
		items = append(items, tui.StatusItem{Section: section, Label: label, Value: value})
	}
	cf := func(label, value string) { add(tui.SectionConfig, label, value) }

	ly := cfg.Layers
	if ly == nil {
		ly = &config.Layers{}
	}
	if p := cfg.LLMProviderName; p != "" {
		cf("provider", fmt.Sprintf("%s (type %s)", p, cfg.LLMProvider))
	} else {
		cf("provider", cfg.LLMProvider)
	}
	if h := hostOf(cfg.LLMBaseURL); h != "" {
		cf("base url", h)
	}
	if cfg.LLMAPIKey != "" {
		cf("api key", "set, from "+cfg.LLMKeySource)
	} else {
		cf("api key", "not set")
	}
	perm := cfg.AgentPermissions
	if perm == "" {
		perm = "ask (default)"
	}
	cf("permissions", perm)
	if p := cfg.AgentProfile; p != "" {
		cf("profile", p)
	}
	cf("sandbox", cfg.SandboxBackendName())
	if ly.UserFile != "" {
		cf("config.yml · user", ly.UserFile)
	}
	if ly.ProjectFile != "" {
		cf("config.yml · project", ly.ProjectFile)
	}
	if ly.AuthFile != "" {
		cf("auth.json · user", ly.AuthFile)
	}
	if d := config.UserDir(); d != "" {
		if p := filepath.Join(d, tuiConfigFile); fileExists(p) {
			cf("tui.json · user", p)
		}
	}
	for _, f := range ly.MCPSources {
		cf("mcp file", f)
	}
	var env []string
	for name, src := range ly.Sources {
		if src == config.SourceEnv {
			env = append(env, name)
		}
	}
	if len(env) > 0 {
		sort.Strings(env)
		cf("environment", fmt.Sprintf("%d settings: %s", len(env), strings.Join(env, ", ")))
	}
	if ly.ProjectRoot != "" {
		trust := "not trusted (harness trust)"
		if ly.Trusted {
			trust = "trusted (" + ly.TrustedBy + ")"
		}
		cf("project", ly.ProjectRoot+", "+trust)
	}
	for _, s := range ly.Ignored {
		cf("ignored", s)
	}

	for _, s := range cfg.MCPServers {
		v := mcpSummary(s) + " (" + s.Origin + ")"
		if n := len(s.Tools); n > 0 {
			v += fmt.Sprintf(", %d tools allowed", n)
		}
		add(tui.SectionMCP, s.Name, v)
	}
	for _, s := range ly.MCPSkipped {
		add(tui.SectionMCP, "not started", s)
	}
	return items
}

// mcpSummary describes a server without its URL path, query, credentials or arguments.
func mcpSummary(s config.MCPServer) string {
	if s.IsStdio() {
		sum := "stdio: " + filepath.Base(s.Command)
		if n := len(s.Args); n > 0 {
			sum += fmt.Sprintf(" (+%d args)", n)
		}
		return sum
	}
	t := s.Type
	if t == "" {
		t = "http"
	}
	return t + ": " + hostOf(s.URL)
}

// hostOf is the scheme and host of a URL.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
