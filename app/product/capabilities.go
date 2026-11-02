package product

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/capabilities"
	"github.com/7ar-chat/harness/core/store"
)

// The MCP marketplace over HTTP. The catalog (see Catalog) lists the servers the product ships; a project (its
// owners) or a user (for their personal sessions) turns them on, fills in their keys, narrows their tools, or adds
// a server of their own. What a run can use is decided by NewMCPSource.
//
//	GET    /api/projects/{pid}/capabilities              → [CapabilityView]            (any member)
//	GET    /api/me/capabilities                          → [CapabilityView]
//	PUT    /api/projects/{pid}/capabilities/{name} {enabled?,fields?,headers?,tools?,url?,transport?} → CapabilityView   (owner)
//	PUT    /api/me/capabilities/{name} {…}               → CapabilityView
//	POST   /api/projects/{pid}/capabilities {name,url,transport?,headers?,tools?,enabled?} → 201 CapabilityView   (owner; a custom server)
//	POST   /api/me/capabilities {…}                      → 201 CapabilityView
//	DELETE /api/projects/{pid}/capabilities/{name}       → 204   (owner; a catalog entry returns to its shipped default)
//	DELETE /api/me/capabilities/{name}                   → 204
//	POST   /api/projects/{pid}/capabilities/{name}/test  → CapabilityTestResult   (owner)
//	POST   /api/me/capabilities/{name}/test              → CapabilityTestResult
//
// In a PUT, `fields` (catalog) and `headers` (custom) merge into what is stored and an empty value removes a key;
// `tools` replaces the allow-list and [] clears it (the entry's default, else the server's read-only tools).
// Credentials are sealed before they are stored and never leave the server: a view says only whether a field is set.

// Limits of what a scope can add.
const (
	maxCustomServers = 20
	maxHeaders       = 8
	maxSecretValue   = 4 << 10
	maxURLLength     = 2048
	maxToolsListed   = 100
	maxToolNameLen   = 128
	testTimeout      = 15 * time.Second
)

// CapabilityFieldView is a value the user fills in to enable a catalog server; Set says it has a value, never what it is.
type CapabilityFieldView struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Help     string `json:"help,omitempty"`
	Secret   bool   `json:"secret"`
	Required bool   `json:"required"`
	Set      bool   `json:"set"`
}

// CapabilityView is one MCP server as a project or user sees it.
type CapabilityView struct {
	Name           string                `json:"name"`
	Kind           string                `json:"kind"` // "catalog" or "custom"
	Title          string                `json:"title"`
	Description    string                `json:"description"`
	Category       string                `json:"category,omitempty"`
	Homepage       string                `json:"homepage,omitempty"`
	URL            string                `json:"url"`
	Transport      string                `json:"transport"`
	Enabled        bool                  `json:"enabled"` // active: turned on and with its required fields filled in
	DefaultEnabled bool                  `json:"defaultEnabled"`
	Installed      bool                  `json:"installed"` // the scope has made a decision about it
	Orphan         bool                  `json:"orphan,omitempty"`
	Fields         []CapabilityFieldView `json:"fields,omitempty"`
	HeaderNames    []string              `json:"headerNames,omitempty"` // custom: the stored headers, never their values
	Missing        []string              `json:"missing,omitempty"`
	Tools          []string              `json:"tools,omitempty"` // the allow-list; absent: only the server's read-only tools
}

// CapabilityTool is a tool a server offers, as the test lists it.
type CapabilityTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	ReadOnly    bool   `json:"readOnly"`
}

// CapabilityTestResult is what a connection test found. Error is deliberately generic.
type CapabilityTestResult struct {
	OK    bool             `json:"ok"`
	Error string           `json:"error,omitempty"`
	Tools []CapabilityTool `json:"tools,omitempty"`
}

// Kinds of CapabilityView.
const (
	capabilityCatalog = "catalog"
	capabilityCustom  = "custom"
)

// ErrCredentialsRejected is what an MCPTester returns when the server answered 401 or 403.
var ErrCredentialsRejected = errors.New("the server rejected the credentials")

// MCPTester lists every tool of a database-supplied MCP server through the guarded transport (the product does not
// import the MCP SDK: the command wires features/mcp's ListTools). Any error is shown to the user as a generic
// message; ErrCredentialsRejected selects the one about credentials.
type MCPTester func(ctx context.Context, s config.MCPServer) ([]CapabilityTool, error)

// capScope is who a marketplace request acts for.
type capScope struct {
	scope   capabilities.Scope
	id      string
	caller  Caller // for the audit log and the feed
	project bool
}

func (s *server) routeCapabilities() {
	if s.opt.Capabilities == nil {
		return
	}
	for _, p := range []struct {
		prefix  string
		project bool
	}{{"/projects/{pid}", true}, {"/me", false}} {
		s.Route("GET "+p.prefix+"/capabilities", s.capRoute(p.project, false, s.listCapabilities))
		s.Route("POST "+p.prefix+"/capabilities", s.capRoute(p.project, true, s.addCapability))
		s.Route("PUT "+p.prefix+"/capabilities/{name}", s.capRoute(p.project, true, s.putCapability))
		s.Route("DELETE "+p.prefix+"/capabilities/{name}", s.capRoute(p.project, true, s.deleteCapability))
		s.Route("POST "+p.prefix+"/capabilities/{name}/test", s.capRoute(p.project, true, s.testCapability))
	}
}

// capRoute resolves the scope of the route: the project of the path (an owner to write, any member to read) or the
// caller's own installs.
func (s *server) capRoute(project, write bool, h func(http.ResponseWriter, *http.Request, capScope)) handler {
	return func(w http.ResponseWriter, r *http.Request, c Caller) {
		if !project {
			h(w, r, capScope{scope: capabilities.ScopeUser, id: c.Actor, caller: c})
			return
		}
		need := store.RoleViewer
		if write {
			need = store.RoleOwner
		}
		p, _, ok := s.project(w, r, c, need)
		if !ok {
			return
		}
		h(w, r, capScope{scope: capabilities.ScopeProject, id: p.ID, caller: inProject(c, p.ID), project: true})
	}
}

// audit records a change of a project's marketplace; a user's own servers are theirs alone and leave no trace in a
// project's log.
func (s *server) capAudit(sc capScope, action, name string, detail map[string]any) {
	if !sc.project {
		return
	}
	d := map[string]any{"server": name}
	for k, v := range detail {
		d[k] = v
	}
	s.note(sc.caller, "", action, d, FeedEvent{Type: "project.changed"})
}

// capState reads the catalog and the scope's installs and merges them.
func (s *server) capState(ctx context.Context, sc capScope) ([]capabilities.Item, error) {
	catalog, err := s.opt.Capabilities.ListCatalog(ctx)
	if err != nil {
		return nil, err
	}
	installs, err := s.opt.Capabilities.ListInstalls(ctx, sc.scope, sc.id)
	if err != nil {
		return nil, err
	}
	return capabilities.Effective(catalog, installs), nil
}

func itemNamed(items []capabilities.Item, name string) (capabilities.Item, bool) {
	for _, it := range items {
		if it.Name == name {
			return it, true
		}
	}
	return capabilities.Item{}, false
}

func capabilityView(it capabilities.Item) CapabilityView {
	v := CapabilityView{Name: it.Name, Kind: capabilityCustom, Title: it.Name, Enabled: it.Enabled, Installed: it.Install != nil,
		Orphan: it.Orphan, Missing: it.Missing, Tools: it.AllowedTools()}
	if in := it.Install; in != nil {
		v.URL, v.Transport = in.URL, in.Transport
		if in.CatalogID != "" {
			v.Kind = capabilityCatalog
		} else {
			v.HeaderNames = in.SecretKeys
		}
	}
	if e := it.Entry; e != nil {
		v.Kind, v.Title, v.Description, v.Category, v.Homepage = capabilityCatalog, e.Name, e.Description, e.Category, e.Homepage
		v.URL, v.Transport, v.DefaultEnabled = e.URL, e.Transport, e.DefaultEnabled
		for _, f := range e.Fields {
			set := it.Install != nil && slices.Contains(it.Install.SecretKeys, f.Key)
			v.Fields = append(v.Fields, CapabilityFieldView{Key: f.Key, Label: f.Label, Help: f.Help, Secret: f.Secret, Required: f.Required, Set: set})
		}
	}
	return v
}

// answer writes the view of the named server as the scope now sees it.
func (s *server) capAnswer(w http.ResponseWriter, r *http.Request, sc capScope, name string, status int) {
	items, err := s.capState(r.Context(), sc)
	if err != nil {
		fail(w, err)
		return
	}
	it, ok := itemNamed(items, name)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "No such server.")
		return
	}
	writeJSON(w, status, capabilityView(it))
}

func (s *server) listCapabilities(w http.ResponseWriter, r *http.Request, sc capScope) {
	items, err := s.capState(r.Context(), sc)
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]CapabilityView, 0, len(items))
	for _, it := range items {
		out = append(out, capabilityView(it))
	}
	writeJSON(w, http.StatusOK, out)
}

// capabilityBody is the body of a PUT and of a POST. Absent and empty differ for tools (nil: keep, []: clear).
type capabilityBody struct {
	Name      string            `json:"name"`
	Enabled   *bool             `json:"enabled"`
	Fields    map[string]string `json:"fields"`
	Headers   map[string]string `json:"headers"`
	Tools     *[]string         `json:"tools"`
	URL       *string           `json:"url"`
	Transport *string           `json:"transport"`
}

func (s *server) putCapability(w http.ResponseWriter, r *http.Request, sc capScope) {
	name := r.PathValue("name")
	if !capabilities.ValidName(name) {
		writeError(w, http.StatusBadRequest, "invalid_name", "That is not a valid server name.")
		return
	}
	var body capabilityBody
	if !decode(w, r, &body) {
		return
	}
	items, err := s.capState(r.Context(), sc)
	if err != nil {
		fail(w, err)
		return
	}
	it, ok := itemNamed(items, name)
	if !ok || it.Orphan {
		writeError(w, http.StatusNotFound, "not_found", "No such server.")
		return
	}
	in := capabilities.Install{Scope: sc.scope, ScopeID: sc.id, Name: name, CreatedBy: sc.caller.Actor}
	if it.Install != nil {
		in = *it.Install
	} else {
		in.CatalogID, in.Enabled = name, it.Entry != nil && it.Entry.DefaultEnabled
	}
	var values map[string]string
	if values, ok = s.capOpen(w, in); !ok {
		return
	}
	var changed []string // the keys touched, for the audit log (never their values)
	if it.Entry != nil {
		if body.URL != nil || body.Transport != nil || body.Headers != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "The address and the headers of a catalog server are fixed; set its fields.")
			return
		}
		if !s.capMerge(w, body.Fields, values, func(k string) bool {
			return slices.ContainsFunc(it.Entry.Fields, func(f capabilities.Field) bool { return f.Key == k })
		}) {
			return
		}
		changed = keysOf(body.Fields)
	} else {
		if body.Fields != nil {
			writeError(w, http.StatusBadRequest, "unknown_field", "A custom server has headers, not fields.")
			return
		}
		headers, ok := canonicalHeaders(w, body.Headers)
		if !ok {
			return
		}
		if !s.capMerge(w, headers, values, func(string) bool { return true }) {
			return
		}
		if len(values) > maxHeaders {
			writeError(w, http.StatusBadRequest, "too_many", "A server can have at most 8 headers.")
			return
		}
		changed = keysOf(headers)
		if body.URL != nil {
			if err := checkServerURL(*body.URL); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_url", err.Error())
				return
			}
			in.URL = *body.URL
		}
		if body.Transport != nil {
			if !validTransport(*body.Transport) {
				writeError(w, http.StatusBadRequest, "invalid_transport", "transport must be http or sse.")
				return
			}
			in.Transport = *body.Transport
		}
	}
	if body.Tools != nil {
		if !checkTools(w, *body.Tools) {
			return
		}
		in.Tools = *body.Tools
	}
	action := "capability.update"
	if body.Enabled != nil {
		in.Enabled = *body.Enabled
		action = "capability.disable"
		if in.Enabled {
			action = "capability.enable"
		}
	}
	if !s.capSeal(w, &in, values) {
		return
	}
	if _, err := s.opt.Capabilities.PutInstall(r.Context(), in); err != nil {
		s.capStoreError(w, err)
		return
	}
	s.capAudit(sc, action, name, map[string]any{"keys": changed})
	s.capAnswer(w, r, sc, name, http.StatusOK)
}

func (s *server) addCapability(w http.ResponseWriter, r *http.Request, sc capScope) {
	var body capabilityBody
	if !decode(w, r, &body) {
		return
	}
	if !capabilities.ValidName(body.Name) {
		writeError(w, http.StatusBadRequest, "invalid_name", "A server name is lower case, starts with a letter and has at most 32 letters, digits, - or _ (no double underscore).")
		return
	}
	url := ""
	if body.URL != nil {
		url = *body.URL
	}
	if err := checkServerURL(url); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_url", err.Error())
		return
	}
	transport := capabilities.TransportHTTP
	if body.Transport != nil {
		transport = *body.Transport
	}
	if !validTransport(transport) {
		writeError(w, http.StatusBadRequest, "invalid_transport", "transport must be http or sse.")
		return
	}
	if body.Fields != nil {
		writeError(w, http.StatusBadRequest, "unknown_field", "A custom server has headers, not fields.")
		return
	}
	headers, ok := canonicalHeaders(w, body.Headers)
	if !ok {
		return
	}
	values := map[string]string{}
	if !s.capMerge(w, headers, values, func(string) bool { return true }) {
		return
	}
	if len(values) > maxHeaders {
		writeError(w, http.StatusBadRequest, "too_many", "A server can have at most 8 headers.")
		return
	}
	var tools []string
	if body.Tools != nil {
		if !checkTools(w, *body.Tools) {
			return
		}
		tools = *body.Tools
	}
	installs, err := s.opt.Capabilities.ListInstalls(r.Context(), sc.scope, sc.id)
	if err != nil {
		fail(w, err)
		return
	}
	customs := 0
	for _, in := range installs {
		if in.CatalogID == "" {
			customs++
		}
	}
	if customs >= maxCustomServers {
		writeError(w, http.StatusBadRequest, "too_many", "At most 20 custom servers; remove one first.")
		return
	}
	in := capabilities.Install{Scope: sc.scope, ScopeID: sc.id, Name: body.Name, URL: url, Transport: transport, Tools: tools,
		Enabled: body.Enabled == nil || *body.Enabled, CreatedBy: sc.caller.Actor}
	if !s.capSeal(w, &in, values) {
		return
	}
	if _, err := s.opt.Capabilities.PutInstall(r.Context(), in); err != nil {
		s.capStoreError(w, err)
		return
	}
	s.capAudit(sc, "capability.add", body.Name, map[string]any{"url": url, "headers": keysOf(headers)})
	s.capAnswer(w, r, sc, body.Name, http.StatusCreated)
}

func (s *server) deleteCapability(w http.ResponseWriter, r *http.Request, sc capScope) {
	name := r.PathValue("name")
	items, err := s.capState(r.Context(), sc)
	if err != nil {
		fail(w, err)
		return
	}
	it, ok := itemNamed(items, name)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "No such server.")
		return
	}
	if it.Install != nil {
		if err := s.opt.Capabilities.DeleteInstall(r.Context(), it.Install.ID); err != nil && !errors.Is(err, capabilities.ErrNotFound) {
			fail(w, err)
			return
		}
		s.capAudit(sc, "capability.remove", name, nil)
	}
	w.WriteHeader(http.StatusNoContent)
}

// testCapability connects to the server with its stored credentials and lists its tools. Whatever goes wrong the
// user learns only a generic reason: an upstream body, or whether an address was refused by the guard rather than
// timing out, would let an owner use the server to probe the network it runs in.
func (s *server) testCapability(w http.ResponseWriter, r *http.Request, sc capScope) {
	if s.opt.MCPTester == nil {
		writeError(w, http.StatusNotImplemented, "not_configured", "Testing a server is not available on this deployment.")
		return
	}
	items, err := s.capState(r.Context(), sc)
	if err != nil {
		fail(w, err)
		return
	}
	it, ok := itemNamed(items, r.PathValue("name"))
	if !ok || it.Orphan {
		writeError(w, http.StatusNotFound, "not_found", "No such server.")
		return
	}
	if len(it.Missing) > 0 {
		writeJSON(w, http.StatusOK, CapabilityTestResult{Error: "fill in the required settings first"})
		return
	}
	srv, err := serverOf(s.capBox, it)
	if err != nil {
		writeJSON(w, http.StatusOK, CapabilityTestResult{Error: "the saved credentials cannot be read; set them again"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), testTimeout)
	defer cancel()
	tools, err := s.opt.MCPTester(ctx, srv)
	switch {
	case errors.Is(err, ErrCredentialsRejected):
		writeJSON(w, http.StatusOK, CapabilityTestResult{Error: "the server rejected the credentials"})
	case err != nil:
		writeJSON(w, http.StatusOK, CapabilityTestResult{Error: "could not connect"})
	default:
		writeJSON(w, http.StatusOK, CapabilityTestResult{OK: true, Tools: tools})
	}
}

// ---- helpers ----

// capOpen opens the credentials an install already holds, to merge into them. A box that cannot open them (the
// secret key changed) refuses the write: merging into nothing would silently drop what was there.
func (s *server) capOpen(w http.ResponseWriter, in capabilities.Install) (map[string]string, bool) {
	values, err := openSecrets(s.capBox, &in)
	if err != nil {
		writeError(w, http.StatusConflict, "secrets_unreadable", "The stored credentials cannot be read: remove the server and set it up again.")
		return nil, false
	}
	if values == nil {
		values = map[string]string{}
	}
	return values, true
}

// capMerge applies the changes (an empty value removes a key) to values after checking each key with known.
func (s *server) capMerge(w http.ResponseWriter, changes, values map[string]string, known func(string) bool) bool {
	for _, k := range keysOf(changes) {
		v := changes[k]
		switch {
		case !known(k):
			writeError(w, http.StatusBadRequest, "unknown_field", "This server has no field "+k+".")
			return false
		case len(v) > maxSecretValue:
			writeError(w, http.StatusBadRequest, "invalid_value", "A value can be at most 4 KiB.")
			return false
		case hasControl(v):
			writeError(w, http.StatusBadRequest, "invalid_value", "A value cannot contain control characters.")
			return false
		case v == "":
			delete(values, k)
		default:
			values[k] = v
		}
	}
	return true
}

// capSeal stores values in the install, sealed, and names the keys that are set.
func (s *server) capSeal(w http.ResponseWriter, in *capabilities.Install, values map[string]string) bool {
	in.Secrets, in.SecretKeys = nil, nil
	if len(values) == 0 {
		return true
	}
	if s.capBox == nil {
		writeError(w, http.StatusNotImplemented, "not_configured", "Storing credentials needs a secret key on the server (HARNESS_SECRET_KEY).")
		return false
	}
	plain, _ := json.Marshal(values)
	in.Secrets, in.SecretKeys = s.capBox.seal(plain), keysOf(values)
	return true
}

func (s *server) capStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, capabilities.ErrConflict):
		writeError(w, http.StatusConflict, "name_taken", "That name is already used by a server or a catalog entry.")
	case errors.Is(err, capabilities.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "No such server.")
	default:
		fail(w, err)
	}
}

func keysOf(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 && r != '\t' || r == 0x7f })
}

func validTransport(t string) bool {
	return t == capabilities.TransportHTTP || t == capabilities.TransportSSE
}

func checkTools(w http.ResponseWriter, tools []string) bool {
	if len(tools) > maxToolsListed {
		writeError(w, http.StatusBadRequest, "invalid_tools", "At most 100 tools can be listed.")
		return false
	}
	for _, t := range tools {
		if strings.TrimSpace(t) == "" || len(t) > maxToolNameLen {
			writeError(w, http.StatusBadRequest, "invalid_tools", "A tool name is 1 to 128 characters.")
			return false
		}
	}
	return true
}

var headerNameRe = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]{1,64}$")

// reservedHeaders are the headers the transport owns: a custom server cannot override them.
var reservedHeaders = map[string]bool{"Host": true, "Content-Length": true, "Content-Type": true, "Accept": true,
	"Transfer-Encoding": true, "Connection": true, "Upgrade": true, "Te": true, "Trailer": true}

// canonicalHeaders checks the header names of a request and spells them canonically, so that "x-api-key" and
// "X-Api-Key" are one header.
func canonicalHeaders(w http.ResponseWriter, in map[string]string) (map[string]string, bool) {
	if len(in) == 0 {
		return nil, true
	}
	out := make(map[string]string, len(in))
	for _, name := range keysOf(in) {
		c := http.CanonicalHeaderKey(name)
		if !headerNameRe.MatchString(name) || reservedHeaders[c] || strings.HasPrefix(c, "Mcp-") {
			writeError(w, http.StatusBadRequest, "invalid_header", "The header "+name+" cannot be set.")
			return nil, false
		}
		if _, dup := out[c]; dup {
			writeError(w, http.StatusBadRequest, "invalid_header", "The header "+c+" is listed twice.")
			return nil, false
		}
		out[c] = in[name]
	}
	return out, true
}

// checkServerURL is the early, friendly check of a custom server's address: https, no credentials in the URL, not
// a local or private address given as a literal or as "localhost". The real defence is the dialer, which also
// judges what a name resolves to.
func checkServerURL(raw string) error {
	if raw == "" || len(raw) > maxURLLength {
		return errors.New("a URL of at most 2048 characters is required")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("that is not a valid URL")
	}
	if u.Scheme != "https" {
		return errors.New("the URL must use https")
	}
	if u.User != nil {
		return errors.New("the URL must not contain a user name or password")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return errors.New("that address is not allowed")
	}
	if a, err := netip.ParseAddr(host); err == nil && blockedLiteral(a) {
		return errors.New("that address is not allowed")
	}
	return nil
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func blockedLiteral(a netip.Addr) bool {
	a = a.WithZone("").Unmap()
	return a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsMulticast() ||
		a.IsUnspecified() || cgnat.Contains(a)
}
