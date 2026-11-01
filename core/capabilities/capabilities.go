// Package capabilities is the vocabulary of the MCP marketplace: a catalog of MCP servers the product ships
// (a fixture, kept in the database), and what a project or a user installed from it or brought themselves.
// It holds types, the store port and the rule that decides what is active; adapters/store/capstore persists
// them and the product serves them.
package capabilities

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Scope says who an install belongs to. A project thread uses the project's installs, a personal thread the
// user's own: one member's personal servers (and their keys) never reach a shared session.
type Scope string

// Install scopes.
const (
	ScopeProject Scope = "project"
	ScopeUser    Scope = "user"
)

// Transport is how a server is reached. Only remote servers are supported: nothing here starts a process.
const (
	TransportHTTP = "http"
	TransportSSE  = "sse"
)

// Field is a value a user supplies when enabling a catalog entry (an API key). Entry.Headers refer to it as
// ${key}.
type Field struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Help     string `json:"help,omitempty"`
	Secret   bool   `json:"secret"`
	Required bool   `json:"required"`
}

// Entry is one server of the catalog. The URL is fixed by the catalog, so enabling an entry never lets a user
// point the server at an address of their choosing.
type Entry struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Category    string `json:"category,omitempty"`
	Homepage    string `json:"homepage,omitempty"`
	URL         string `json:"url"`
	Transport   string `json:"transport"`
	// Headers are sent with every request; a value may contain ${field} placeholders (Entry.Fields).
	Headers map[string]string `json:"headers,omitempty"`
	Fields  []Field           `json:"fields,omitempty"`
	// Tools is the default allow-list of the server's tool names (the names the server reports, not the
	// exposed {server}__{tool} ones). Empty: only the tools the server marks read-only.
	Tools []string `json:"tools,omitempty"`
	// DefaultEnabled turns the entry on for every project and user that has not decided otherwise. An
	// entry with a required field is never on until someone fills it in.
	DefaultEnabled bool `json:"defaultEnabled"`
	// Builtin marks an entry that came from the shipped fixture (set by the store on seeding).
	Builtin bool `json:"builtin,omitempty"`
}

// Install is a project's or a user's decision about a server: an override of a catalog entry (turn it on or
// off, fill its fields, narrow its tools) or a custom server of their own.
type Install struct {
	ID      string `json:"id"`
	Scope   Scope  `json:"scope"`
	ScopeID string `json:"scopeId"` // the project id, or the user's login
	// CatalogID names the entry this install configures; empty for a custom server.
	CatalogID string `json:"catalogId,omitempty"`
	// Name is the server name that prefixes its tools ({name}__{tool}). A catalog install uses the entry id.
	Name string `json:"name"`
	// URL and Transport describe a custom server; they are ignored for a catalog install.
	URL       string `json:"url,omitempty"`
	Transport string `json:"transport,omitempty"`
	// Tools replaces the allow-list (the entry's default, or read-only tools for a custom server) when set.
	Tools   []string `json:"tools,omitempty"`
	Enabled bool     `json:"enabled"`
	// Secrets is the sealed JSON object of what the user filled in: field key to value for a catalog install,
	// header name to value for a custom one. Only the host that owns the key can read it; SecretKeys names
	// what is set so the API can say so without opening it.
	Secrets    []byte    `json:"-"`
	SecretKeys []string  `json:"secretKeys,omitempty"`
	CreatedBy  string    `json:"createdBy,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// Errors of a Store.
var (
	ErrNotFound = errors.New("capabilities: not found")
	// ErrConflict: a custom server's name is taken in its scope (by another install or by a catalog entry).
	ErrConflict = errors.New("capabilities: name already in use")
)

// Store keeps the catalog and the installs.
type Store interface {
	// SeedCatalog makes the builtin entries equal to entries: new ones are added, existing ones updated,
	// builtin ones that are no longer listed removed (installs that refer to them stay and are reported as
	// orphans by Effective). Entries added in other ways are left alone.
	SeedCatalog(ctx context.Context, entries []Entry) error
	ListCatalog(ctx context.Context) ([]Entry, error)
	// GetCatalogEntry returns ErrNotFound for an unknown id.
	GetCatalogEntry(ctx context.Context, id string) (*Entry, error)

	// PutInstall stores in. An empty ID creates it (a fresh id and timestamps); otherwise it replaces the
	// install with that id, ErrNotFound if there is none. A catalog install is unique per (scope, scope id,
	// catalog id): creating a second replaces the first's content and keeps its id. A custom install's Name
	// must be unused in the scope, by another install and by any catalog entry id: ErrConflict.
	PutInstall(ctx context.Context, in Install) (*Install, error)
	GetInstall(ctx context.Context, id string) (*Install, error)
	ListInstalls(ctx context.Context, scope Scope, scopeID string) ([]Install, error)
	DeleteInstall(ctx context.Context, id string) error
	// DeleteInstalls removes every install of a scope (a deleted project); it is not an error when there are none.
	DeleteInstalls(ctx context.Context, scope Scope, scopeID string) error
}

var (
	slugRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	// placeholderRe finds ${key} in a header template.
	placeholderRe = regexp.MustCompile(`\$\{([a-z][a-z0-9_]{0,31})\}`)
)

// ValidName reports whether s can name a server: lower case, starts with a letter, at most 32 characters of
// letters, digits, "-" and "_". Tool names are {name}__{tool}, so "__" is not allowed inside it.
func ValidName(s string) bool { return slugRe.MatchString(s) && !strings.Contains(s, "__") }

// Validate checks a catalog entry: the fixture is code, so a mistake in it should fail a test, not a deployment.
func (e Entry) Validate() error {
	switch {
	case !ValidName(e.ID):
		return errors.New("capabilities: entry id " + quote(e.ID) + " is not a valid name")
	case strings.TrimSpace(e.Name) == "":
		return errors.New("capabilities: entry " + e.ID + " has no name")
	case !strings.HasPrefix(e.URL, "https://"):
		return errors.New("capabilities: entry " + e.ID + " must use an https URL")
	case e.Transport != TransportHTTP && e.Transport != TransportSSE:
		return errors.New("capabilities: entry " + e.ID + " has an unknown transport " + quote(e.Transport))
	}
	fields := map[string]bool{}
	for _, f := range e.Fields {
		if !slugRe.MatchString(f.Key) || strings.Contains(f.Key, "-") {
			return errors.New("capabilities: entry " + e.ID + " has a bad field key " + quote(f.Key))
		}
		fields[f.Key] = true
	}
	for h, v := range e.Headers {
		for _, m := range placeholderRe.FindAllStringSubmatch(v, -1) {
			if !fields[m[1]] {
				return errors.New("capabilities: entry " + e.ID + " header " + h + " uses ${" + m[1] + "}, which is not a field")
			}
		}
	}
	if e.DefaultEnabled {
		for _, f := range e.Fields {
			if f.Required {
				return errors.New("capabilities: entry " + e.ID + " is on by default but needs " + f.Key)
			}
		}
	}
	return nil
}

func quote(s string) string { return `"` + s + `"` }

// Expand fills the ${field} placeholders of the entry's headers from values. A placeholder without a value
// leaves its header out, so an optional key that was not given sends no header.
func (e Entry) Expand(values map[string]string) map[string]string {
	out := map[string]string{}
	for h, v := range e.Headers {
		missing := false
		filled := placeholderRe.ReplaceAllStringFunc(v, func(m string) string {
			val, ok := values[placeholderRe.FindStringSubmatch(m)[1]]
			if !ok || val == "" {
				missing = true
			}
			return val
		})
		if !missing {
			out[h] = filled
		}
	}
	return out
}

// Item is one server as a scope sees it: what the catalog and the install say together.
type Item struct {
	// Name is the server name (the tool prefix).
	Name string
	// Entry is the catalog entry, nil for a custom server.
	Entry *Entry
	// Install is the scope's decision, nil when the scope has made none (a catalog entry as shipped).
	Install *Install
	// Enabled says the server is active: turned on (explicitly, or by the catalog's default) and with its
	// required fields filled in.
	Enabled bool
	// Missing names the required fields that are not set; an item with any is never Enabled.
	Missing []string
	// Orphan is an install of a catalog entry that is no longer in the catalog; it is never Enabled.
	Orphan bool
}

// AllowedTools is the allow-list of the item's tools: the install's own, else the entry's default. Nil means
// "only the tools the server marks read-only".
func (it Item) AllowedTools() []string {
	if it.Install != nil && len(it.Install.Tools) > 0 {
		return it.Install.Tools
	}
	if it.Entry != nil && len(it.Entry.Tools) > 0 {
		return it.Entry.Tools
	}
	return nil
}

// Effective merges the catalog with a scope's installs into what the scope sees, sorted by name: every
// catalog entry (on by its default unless an install decides otherwise), then the scope's custom servers
// and its orphaned installs. Installs of other scopes must not be passed in.
func Effective(catalog []Entry, installs []Install) []Item {
	byCatalog := map[string]*Install{}
	var rest []*Install
	for i := range installs {
		in := &installs[i]
		if in.CatalogID == "" {
			rest = append(rest, in)
		} else {
			byCatalog[in.CatalogID] = in
		}
	}
	var items []Item
	known := map[string]bool{}
	for i := range catalog {
		e := &catalog[i]
		known[e.ID] = true
		it := Item{Name: e.ID, Entry: e, Install: byCatalog[e.ID], Enabled: e.DefaultEnabled}
		if it.Install != nil {
			it.Enabled = it.Install.Enabled
		}
		it.Missing = missing(e.Fields, it.Install)
		if len(it.Missing) > 0 {
			it.Enabled = false
		}
		items = append(items, it)
	}
	for id, in := range byCatalog {
		if !known[id] {
			items = append(items, Item{Name: in.Name, Install: in, Orphan: true})
		}
	}
	for _, in := range rest {
		items = append(items, Item{Name: in.Name, Install: in, Enabled: in.Enabled})
	}
	sort.Slice(items, func(a, b int) bool { return items[a].Name < items[b].Name })
	return items
}

func missing(fields []Field, in *Install) []string {
	have := map[string]bool{}
	if in != nil {
		for _, k := range in.SecretKeys {
			have[k] = true
		}
	}
	var out []string
	for _, f := range fields {
		if f.Required && !have[f.Key] {
			out = append(out, f.Key)
		}
	}
	return out
}
