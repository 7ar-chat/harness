package config

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Managed token modes (Studio COWORK_MANAGED_TOKEN_MODE).
const (
	// TokenModeLegacy gives the web search MCP the user's own access token.
	TokenModeLegacy = "legacy"
	// TokenModeExchange exchanges the user's token (RFC 8693) for a token of the configured audience.
	TokenModeExchange = "token_exchange"
)

// WebSearchServerName is the MCP server name of the web search (tools websearch__tavily_search, websearch__scrape_webpage).
const WebSearchServerName = "websearch"

// WebSearch is the Tavily web search configuration (parity with Work: AstraForge
// mounts the remote MCP at WEB_SEARCH_MCP_URL with the user's Studio token).
type WebSearch struct {
	// WebSearchEnabled is a kill switch; web search also needs WEB_SEARCH_MCP_URL.
	WebSearchEnabled bool   `env:"WEB_SEARCH_ENABLED" envDefault:"true"`
	WebSearchMCPURL  string `env:"WEB_SEARCH_MCP_URL"`
	// ManagedTokenMode is legacy (the user's token) or token_exchange (RFC 8693).
	ManagedTokenMode          string `env:"WORK_MANAGED_TOKEN_MODE" envDefault:"legacy"`
	TokenExchangeAudience     string `env:"WORK_TOKEN_EXCHANGE_AUDIENCE"`
	TokenExchangeScope        string `env:"WORK_TOKEN_EXCHANGE_SCOPE"`
	TokenExchangeClientID     string `env:"WORK_TOKEN_EXCHANGE_CLIENT_ID"`
	TokenExchangeClientSecret string `env:"WORK_TOKEN_EXCHANGE_CLIENT_SECRET"`
	TokenExchangeEndpoint     string `env:"WORK_TOKEN_EXCHANGE_TOKEN_ENDPOINT"`
	// ManagedTokenMinValiditySeconds: an exchanged token must stay valid this long
	// (0: max(900, LLM_TIMEOUT_SECONDS), like Studio's max(900, stream timeout)).
	ManagedTokenMinValiditySeconds int `env:"WORK_MANAGED_TOKEN_MIN_VALIDITY_SECONDS"`
	// TokenEncryptionKey (base64, 32 bytes) encrypts the run tokens at rest (AES-256-GCM).
	TokenEncryptionKey string `env:"WORK_TOKEN_ENCRYPTION_KEY"`
}

func (c *Config) normalizeWebSearch() error {
	w := &c.WebSearch
	w.WebSearchMCPURL = strings.TrimSpace(w.WebSearchMCPURL)
	w.ManagedTokenMode = strings.TrimSpace(w.ManagedTokenMode)
	if w.ManagedTokenMode == "" {
		w.ManagedTokenMode = TokenModeLegacy
	}
	for _, p := range []*string{&w.TokenExchangeAudience, &w.TokenExchangeScope, &w.TokenExchangeClientID,
		&w.TokenExchangeClientSecret, &w.TokenExchangeEndpoint, &w.TokenEncryptionKey} {
		*p = strings.TrimSpace(*p)
	}
	switch w.ManagedTokenMode {
	case TokenModeLegacy, TokenModeExchange:
	default:
		return fmt.Errorf("WORK_MANAGED_TOKEN_MODE must be legacy or token_exchange, got %q", w.ManagedTokenMode)
	}
	if w.WebSearchMCPURL == "" {
		return nil
	}
	if u, err := url.Parse(w.WebSearchMCPURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("WEB_SEARCH_MCP_URL must be an http(s) URL, got %q", w.WebSearchMCPURL)
	}
	for _, s := range c.MCPServers {
		if s.Name == WebSearchServerName {
			return fmt.Errorf("WORK_MCP_SERVERS: server name %q is reserved for WEB_SEARCH_MCP_URL", WebSearchServerName)
		}
	}
	return nil
}

// ErrWebSearchOff means web search is simply not configured (no URL or WEB_SEARCH_ENABLED=false).
var ErrWebSearchOff = errors.New("web search is not configured")

// WebSearchStatus reports whether web search can start: ErrWebSearchOff when it
// is not configured, another error when it is configured but must not start
// (no WORK_TOKEN_ENCRYPTION_KEY in production, incomplete token exchange).
func (c *Config) WebSearchStatus() error {
	w := c.WebSearch
	if !w.WebSearchEnabled || w.WebSearchMCPURL == "" {
		return ErrWebSearchOff
	}
	if u, err := url.Parse(w.WebSearchMCPURL); !c.IsDebug() && (err != nil || u.Scheme != "https") {
		return errors.New("WEB_SEARCH_MCP_URL must be an https URL in production (the users' access tokens are sent to it)")
	}
	if w.TokenEncryptionKey == "" && !c.IsDebug() {
		return errors.New("WORK_TOKEN_ENCRYPTION_KEY is required in production to store the users' tokens of web search runs")
	}
	if w.ManagedTokenMode == TokenModeExchange {
		var missing []string
		for name, v := range map[string]string{
			"WORK_TOKEN_EXCHANGE_AUDIENCE": w.TokenExchangeAudience, "WORK_TOKEN_EXCHANGE_SCOPE": w.TokenExchangeScope,
			"WORK_TOKEN_EXCHANGE_CLIENT_ID": w.TokenExchangeClientID, "WORK_TOKEN_EXCHANGE_CLIENT_SECRET": w.TokenExchangeClientSecret,
			"WORK_TOKEN_EXCHANGE_TOKEN_ENDPOINT": w.TokenExchangeEndpoint,
		} {
			if v == "" {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			slices.Sort(missing)
			return fmt.Errorf("token exchange configuration is incomplete: %s", strings.Join(missing, ", "))
		}
	}
	return nil
}

// TokenMinValidity is the lifetime an exchanged token must still have when a run is created.
func (c *Config) TokenMinValidity() time.Duration {
	if s := c.ManagedTokenMinValiditySeconds; s > 0 {
		return time.Duration(s) * time.Second
	}
	return time.Duration(max(900, c.LLMTimeout)) * time.Second
}
