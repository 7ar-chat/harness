package main

import (
	"errors"
	"sort"

	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/hosts/tui"
)

// authConnector backs /connect in the terminal UI: it stores the key through
// the code path of `harness auth login` (config.SetAuthKey: ~/.har/auth.json,
// mode 0600, written atomically) and never returns or prints it.
type authConnector struct {
	dir       string
	providers map[string]config.Provider
}

func newAuthConnector(cfg *config.Config) *authConnector {
	return &authConnector{dir: config.HomeDir(), providers: cfg.Providers}
}

// Providers lists the built-in types and the named providers of the config
// files, with whether auth.json holds a key for each.
func (c *authConnector) Providers() []tui.ProviderInfo {
	keys, _ := config.ReadAuth(c.dir) // an unreadable file shows as "no key"; Save reports why
	names := []string{"openai", "anthropic"}
	for n := range c.providers {
		names = append(names, n)
	}
	sort.Strings(names[2:])
	out := make([]tui.ProviderInfo, 0, len(names))
	for _, n := range names {
		_, has := keys[n]
		out = append(out, tui.ProviderInfo{Name: n, BaseURL: c.providers[n].BaseURL, HasKey: has})
	}
	return out
}

// Save implements tui.Connector.
func (c *authConnector) Save(provider, key string) error {
	if c.dir == "" {
		return errors.New("no home directory: set HAR_HOME")
	}
	return config.SetAuthKey(c.dir, provider, key)
}

// RestartNeeded: the engine reads its configuration once at start, so a key
// saved now reaches the next start.
func (c *authConnector) RestartNeeded() bool { return true }
