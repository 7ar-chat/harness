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

// addBrowserServer registers HARNESS_BROWSER_URL as the MCP server "browser".
func (c *Config) addBrowserServer() error {
	u := strings.TrimSpace(c.BrowserURL)
	if u == "" {
		return nil
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
