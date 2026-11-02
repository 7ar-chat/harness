package product

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/capabilities"
	"github.com/7ar-chat/harness/core/store"
)

// capabilitiesDomain separates the key of the marketplace credentials from the one of the connector tokens: both
// derive from the same server key, and what was sealed for one must never open as the other.
const capabilitiesDomain = "harness-capabilities:"

var errNoSecretKey = errors.New("the server has no secret key (HARNESS_SECRET_KEY)")

// NewMCPSource is harness.Options.MCPServers for the marketplace: the MCP servers a run may use besides the
// configured ones, read from the database at each run.
//
// The scope is decided by the thread. A project thread uses the installs of its project and nothing else; a
// personal thread (no project) uses the installs of the user who runs it. Never both: a member's personal keys
// must not reach a session the whole project shares.
//
// catalog is the set of entries the installs refer to; nil reads the store's catalog at each run (what the API
// shows), which is what the commands use after seeding it. An entry is active when its install (or its default)
// turns it on and its required fields are filled in. A server whose credentials cannot be opened (the secret key
// changed) is skipped with a warning and never fails the run. secretKey may be empty: servers that need no
// stored credential still work, those that have some are skipped.
func NewMCPSource(team store.TeamStore, caps capabilities.Store, catalog []capabilities.Entry, secretKey []byte) (harness.MCPServersFunc, error) {
	if team == nil || caps == nil {
		return nil, errors.New("product: the marketplace needs a team store and a capability store")
	}
	var box *secretBox
	if len(secretKey) > 0 {
		var err error
		if box, err = newSecretBoxFor(capabilitiesDomain, secretKey); err != nil {
			return nil, err
		}
	}
	return func(ctx context.Context, threadID, login string) ([]config.MCPServer, error) {
		scope, id := capabilities.ScopeUser, login
		meta, err := team.GetThreadMeta(ctx, threadID)
		switch {
		case err == nil && meta.ProjectID != "":
			scope, id = capabilities.ScopeProject, meta.ProjectID
		case err != nil && !errors.Is(err, store.ErrNotFound):
			return nil, fmt.Errorf("resolve the thread's scope: %w", err)
		}
		if id == "" {
			return nil, nil
		}
		entries := catalog
		if entries == nil {
			if entries, err = caps.ListCatalog(ctx); err != nil {
				return nil, fmt.Errorf("list the catalog: %w", err)
			}
		}
		installs, err := caps.ListInstalls(ctx, scope, id)
		if err != nil {
			return nil, fmt.Errorf("list the installs: %w", err)
		}
		var out []config.MCPServer
		for _, it := range capabilities.Effective(entries, installs) {
			if !it.Enabled {
				continue
			}
			s, err := serverOf(box, it)
			if err != nil {
				slog.Warn("marketplace: server skipped, its credentials cannot be opened", "server", it.Name, "scope", string(scope), "scope_id", id)
				continue
			}
			out = append(out, s)
		}
		return out, nil
	}, nil
}

// serverOf is the MCP server of an item: the catalog's URL with its header templates filled from the install's
// credentials, or the custom server's own URL and headers. Every one is Untrusted. The error says only that the
// credentials could not be opened.
func serverOf(box *secretBox, it capabilities.Item) (config.MCPServer, error) {
	values, err := openSecrets(box, it.Install)
	if err != nil {
		return config.MCPServer{}, err
	}
	s := config.MCPServer{Name: it.Name, Tools: it.AllowedTools(), Untrusted: true}
	if it.Entry != nil {
		s.URL, s.Type, s.Headers = it.Entry.URL, it.Entry.Transport, it.Entry.Expand(values)
	} else {
		s.URL, s.Type = it.Install.URL, it.Install.Transport
		if s.Type == "" {
			s.Type = capabilities.TransportHTTP
		}
		if len(values) > 0 {
			s.Headers = values
		}
	}
	if len(s.Headers) == 0 {
		s.Headers = nil
	}
	return s, nil
}

// openSecrets decodes an install's sealed credentials: field key (catalog) or header name (custom) to value.
// An install without credentials yields none, whatever the box.
func openSecrets(box *secretBox, in *capabilities.Install) (map[string]string, error) {
	if in == nil || len(in.Secrets) == 0 {
		return nil, nil
	}
	if box == nil {
		return nil, errNoSecretKey
	}
	plain, err := box.open(in.Secrets)
	if err != nil {
		return nil, err
	}
	var values map[string]string
	if err := json.Unmarshal(plain, &values); err != nil {
		return nil, err
	}
	return values, nil
}

// SecretKeyFromEnv is the key that seals the credentials kept in the database: HARNESS_SECRET_KEY, else the OIDC
// session key when OIDC is configured. Every process that reads or writes them (the web server, the workers) must
// derive the same one.
func SecretKeyFromEnv(getenv func(string) string) []byte {
	if k := getenv("HARNESS_SECRET_KEY"); k != "" {
		return []byte(k)
	}
	if k := getenv("OIDC_SESSION_KEY"); k != "" && getenv("OIDC_ISSUER") != "" {
		return []byte(k)
	}
	return nil
}
