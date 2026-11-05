package config

import (
	"fmt"
	"strings"
)

// BrowserTools are the tools of the Playwright MCP server the agent is given. browser_run_code_unsafe and
// browser_evaluate (arbitrary code in the page and in the browser process), the network log and the console are left out.
var BrowserTools = []string{
	"browser_navigate", "browser_navigate_back", "browser_snapshot", "browser_click", "browser_type", "browser_fill_form",
	"browser_select_option", "browser_press_key", "browser_hover", "browser_wait_for", "browser_tabs",
	"browser_take_screenshot", "browser_file_upload", "browser_handle_dialog", "browser_find", "browser_close",
}

// BrowserSandboxBackend is the sandbox provider that runs the browser in a sandbox of its own ("daytona", "e2b" or
// "opensandbox"), or "" when the browser is the always-on service or absent.
func (c *Config) BrowserSandboxBackend() string {
	switch n := strings.ToLower(strings.TrimSpace(c.BrowserBackend)); n {
	case BackendDaytona, BackendE2B, BackendOpenSandbox:
		return n
	}
	return ""
}

// ValidateBrowser reports a BROWSER_BACKEND the harness does not know.
func (c *Config) ValidateBrowser() error {
	switch n := strings.ToLower(strings.TrimSpace(c.BrowserBackend)); n {
	case "", "service", BackendNone, BackendDaytona, BackendE2B, BackendOpenSandbox:
		return nil
	default:
		return fmt.Errorf("BROWSER_BACKEND=%q: use service, daytona, e2b, opensandbox or none", c.BrowserBackend)
	}
}

// BrowserSandboxConfig is the configuration of the browser sandbox backend: this one with the browser provider as the
// sandbox backend, browser sandboxes (profile "browser") starting from BrowserImage, and open internet access.
// A browser has to reach arbitrary sites, so the allow-list of the code sandboxes does not apply; the deny list of
// private and link-local ranges does.
func (c *Config) BrowserSandboxConfig() *Config {
	b := *c
	b.SandboxBackend = c.BrowserSandboxBackend()
	b.SandboxImages = map[string]string{"browser": strings.TrimSpace(c.BrowserImage)}
	b.SandboxEgressAllowlist = nil
	b.SandboxEgressUnrestricted = true
	return &b
}

// addBrowserServer registers HARNESS_BROWSER_URL as the MCP server "browser".
func (c *Config) addBrowserServer() error {
	u := strings.TrimSpace(c.BrowserURL)
	if u == "" || c.BrowserSandboxBackend() != "" || strings.EqualFold(strings.TrimSpace(c.BrowserBackend), BackendNone) {
		return nil // the browser runs in a sandbox of its own (or is switched off): see BrowserSandboxConfig
	}
	for _, s := range c.MCPServers {
		if s.Name == "browser" {
			return nil // MCP_SERVERS defines it
		}
	}
	if err := validURL(u); err != nil {
		return fmt.Errorf("HARNESS_BROWSER_URL: %w", err)
	}
	c.MCPServers = append(c.MCPServers, MCPServer{Name: "browser", URL: u, Tools: append([]string(nil), BrowserTools...), Origin: "HARNESS_BROWSER_URL"})
	return nil
}
