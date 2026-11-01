package product

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/adapters/store/capstore"
	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/capabilities"
	"github.com/7ar-chat/harness/core/store"
)

// capRig is a team rig with the marketplace on: alice owns the project, bob is a member, carol is outside.
type capRig struct {
	*rig
	caps    *capstore.Mem
	team    *teamstore.Mem
	project string
	test    func(context.Context, config.MCPServer) ([]CapabilityTool, error)
	tested  []config.MCPServer
}

func newCapRig(t *testing.T, key []byte) *capRig {
	t.Helper()
	tm := teamstore.NewMem()
	for _, l := range []string{"alice", "bob", "carol"} {
		if _, err := tm.UpsertUser(context.Background(), store.User{Login: l, Name: l, Email: l + "@x.io"}); err != nil {
			t.Fatal(err)
		}
	}
	caps := capstore.NewMem(nil)
	if err := caps.SeedCatalog(context.Background(), Catalog()); err != nil {
		t.Fatal(err)
	}
	cr := &capRig{caps: caps, team: tm}
	tester := func(ctx context.Context, s config.MCPServer) ([]CapabilityTool, error) {
		cr.tested = append(cr.tested, s)
		return cr.test(ctx, s)
	}
	cr.rig = newRig(t, rigOpts{team: true, prod: Options{Team: tm, Capabilities: caps, SecretKey: key, MCPTester: tester}})
	cr.project = cr.json("POST", "/api/projects", "alice", `{"name":"api"}`, 201)["id"].(string)
	cr.json("PUT", "/api/projects/"+cr.project+"/members/bob", "alice", `{"role":"member"}`, 204)
	return cr
}

var capKey = []byte("capabilities-api-test-key-012345")

func (r *capRig) pbase() string { return "/api/projects/" + r.project + "/capabilities" }

// call performs a request and returns the status and the raw body.
func (r *capRig) call(method, path, user, body string) (int, string) {
	r.t.Helper()
	resp := r.do(method, path, user, body)
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// ok expects status want and decodes the body into a view.
func (r *capRig) view(method, path, user, body string, want int) CapabilityView {
	r.t.Helper()
	code, raw := r.call(method, path, user, body)
	if code != want {
		r.t.Fatalf("%s %s: status %d, want %d: %s", method, path, code, want, raw)
	}
	var v CapabilityView
	_ = json.Unmarshal([]byte(raw), &v)
	return v
}

func (r *capRig) errCode(method, path, user, body string, want int) string {
	r.t.Helper()
	code, raw := r.call(method, path, user, body)
	if code != want {
		r.t.Fatalf("%s %s: status %d, want %d: %s", method, path, code, want, raw)
	}
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Code string `json:"code"`
	}
	_ = json.Unmarshal([]byte(raw), &e)
	if e.Error.Code != "" {
		return e.Error.Code
	}
	return e.Code
}

func (r *capRig) list(path, user string) map[string]CapabilityView {
	r.t.Helper()
	code, raw := r.call("GET", path, user, "")
	if code != 200 {
		r.t.Fatalf("GET %s: %d %s", path, code, raw)
	}
	var vs []CapabilityView
	if err := json.Unmarshal([]byte(raw), &vs); err != nil {
		r.t.Fatalf("list: %v: %s", err, raw)
	}
	out := map[string]CapabilityView{}
	for _, v := range vs {
		out[v.Name] = v
	}
	return out
}

func (r *capRig) installs(scope capabilities.Scope, id string) []capabilities.Install {
	r.t.Helper()
	l, err := r.caps.ListInstalls(context.Background(), scope, id)
	if err != nil {
		r.t.Fatal(err)
	}
	return l
}

func (r *capRig) actions() []string {
	var out []string
	entries, _ := r.team.ListAudit(context.Background(), r.project, 100)
	for _, e := range entries {
		if strings.HasPrefix(e.Action, "capability.") {
			out = append(out, e.Action)
		}
	}
	sort.Strings(out) // the log lists the newest first
	return out
}

func TestTheCatalogListsShippedServersWithTheirStateAndReadableToAnyMember(t *testing.T) {
	// Arrange
	r := newCapRig(t, capKey)

	// Act
	asBob := r.list(r.pbase(), "bob")
	mine := r.list("/api/me/capabilities", "alice")

	// Assert
	exa, gh := asBob["exa"], asBob["github"]
	if exa.Kind != "catalog" || !exa.Enabled || !exa.DefaultEnabled || exa.Installed || exa.URL != "https://mcp.exa.ai/mcp" || exa.Title != "Exa" {
		t.Fatalf("exa = %+v", exa)
	}
	if !equalStrings(exa.Tools, []string{"web_search_exa", "web_fetch_exa"}) {
		t.Fatalf("exa tools = %v", exa.Tools)
	}
	if gh.Enabled || len(gh.Missing) != 1 || gh.Missing[0] != "token" || len(gh.Fields) != 1 || gh.Fields[0].Set || !gh.Fields[0].Secret || !gh.Fields[0].Required {
		t.Fatalf("github = %+v", gh)
	}
	if len(mine) != len(asBob) {
		t.Fatalf("the user's own list differs: %d vs %d", len(mine), len(asBob))
	}
	r.errCode("GET", r.pbase(), "carol", "", 404) // an outsider does not know the project exists
}

func equalStrings(a, b []string) bool { return strings.Join(a, "\x00") == strings.Join(b, "\x00") }

func TestFillingARequiredFieldEnablesAServerAndTheSecretIsSealedAtRest(t *testing.T) {
	// Arrange
	r := newCapRig(t, capKey)

	// Act
	code, raw := r.call("PUT", r.pbase()+"/github", "alice", `{"enabled":true,"fields":{"token":"ghp_SUPERSECRET"}}`)
	listed := rawBodyOf(r.call("GET", r.pbase(), "bob", ""))

	// Assert: active, field reported as set, and the value is nowhere in an answer or in the store
	if code != 200 {
		t.Fatalf("PUT: %d %s", code, raw)
	}
	var v CapabilityView
	_ = json.Unmarshal([]byte(raw), &v)
	if !v.Enabled || !v.Installed || len(v.Missing) != 0 || !v.Fields[0].Set {
		t.Fatalf("view = %+v", v)
	}
	if strings.Contains(raw, "SUPERSECRET") || strings.Contains(listed, "SUPERSECRET") {
		t.Fatal("a secret value is in an API answer")
	}
	in := r.installs(capabilities.ScopeProject, r.project)
	if len(in) != 1 || in[0].CatalogID != "github" || !equalStrings(in[0].SecretKeys, []string{"token"}) || bytes.Contains(in[0].Secrets, []byte("SUPERSECRET")) || len(in[0].Secrets) == 0 {
		t.Fatalf("stored install = %+v", in)
	}
	if !equalStrings(r.actions(), []string{"capability.enable"}) {
		t.Fatalf("audit = %v", r.actions())
	}
	// The run-time source sees the expanded header for project threads.
	src, _ := NewMCPSource(r.team, r.caps, nil, capKey)
	if err := r.team.SetThreadMeta(context.Background(), store.ThreadMeta{ThreadID: "pt", ProjectID: r.project, Owner: "alice", CreatedBy: "alice", Kind: store.ThreadChat}); err != nil {
		t.Fatal(err)
	}
	servers, _ := src(context.Background(), "pt", "alice")
	found := false
	for _, s := range servers {
		if s.Name == "github" && s.Headers["Authorization"] == "Bearer ghp_SUPERSECRET" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the source does not serve the configured server: %+v", servers)
	}
}

func rawBodyOf(_ int, body string) string { return body }

func TestFieldsMergeAndAnEmptyValueRemovesAKey(t *testing.T) {
	// Arrange
	r := newCapRig(t, capKey)
	r.view("PUT", r.pbase()+"/exa", "alice", `{"fields":{"api_key":"K1"}}`, 200)

	// Act: a call without fields keeps the key; an empty string removes it
	kept := r.view("PUT", r.pbase()+"/exa", "alice", `{"tools":["web_search_exa"]}`, 200)
	removed := r.view("PUT", r.pbase()+"/exa", "alice", `{"fields":{"api_key":""}}`, 200)

	// Assert
	if !kept.Fields[0].Set || !equalStrings(kept.Tools, []string{"web_search_exa"}) {
		t.Fatalf("kept = %+v", kept)
	}
	if removed.Fields[0].Set || !removed.Enabled { // optional: still on (by default) without the key
		t.Fatalf("removed = %+v", removed)
	}
	if in := r.installs(capabilities.ScopeProject, r.project); len(in) != 1 || len(in[0].Secrets) != 0 || len(in[0].SecretKeys) != 0 {
		t.Fatalf("the removed key is still stored: %+v", in)
	}
}

func TestToolsReplaceTheAllowListAndAnEmptyListReturnsToTheDefault(t *testing.T) {
	// Arrange
	r := newCapRig(t, capKey)

	// Act
	narrowed := r.view("PUT", r.pbase()+"/exa", "alice", `{"tools":["web_fetch_exa"]}`, 200)
	cleared := r.view("PUT", r.pbase()+"/exa", "alice", `{"tools":[]}`, 200)

	// Assert
	if !equalStrings(narrowed.Tools, []string{"web_fetch_exa"}) || !equalStrings(cleared.Tools, []string{"web_search_exa", "web_fetch_exa"}) {
		t.Fatalf("narrowed %v, cleared %v", narrowed.Tools, cleared.Tools)
	}
}

func TestDisablingADefaultOnServerIsRecordedAndOnlyOwnersMayWrite(t *testing.T) {
	// Arrange
	r := newCapRig(t, capKey)

	// Act
	off := r.view("PUT", r.pbase()+"/exa", "alice", `{"enabled":false}`, 200)

	// Assert
	if off.Enabled || !off.Installed || !equalStrings(r.actions(), []string{"capability.disable"}) {
		t.Fatalf("off = %+v, audit %v", off, r.actions())
	}
	for _, c := range []struct{ method, path, body string }{
		{"PUT", r.pbase() + "/exa", `{"enabled":true}`},
		{"POST", r.pbase(), `{"name":"x","url":"https://x.example/mcp"}`},
		{"DELETE", r.pbase() + "/exa", ""},
		{"POST", r.pbase() + "/exa/test", ""},
	} {
		if got, _ := r.call(c.method, c.path, "bob", c.body); got != 403 {
			t.Errorf("a member %s %s = %d, want 403", c.method, c.path, got)
		}
		if got, _ := r.call(c.method, c.path, "carol", c.body); got != 404 {
			t.Errorf("an outsider %s %s = %d, want 404", c.method, c.path, got)
		}
	}
}

func TestPutRejectsWhatItCannotApply(t *testing.T) {
	// Arrange
	r := newCapRig(t, capKey)
	r.view("POST", r.pbase(), "alice", `{"name":"mine","url":"https://mine.example/mcp"}`, 201)
	cases := []struct {
		name, path, body, code string
		status                 int
	}{
		{"unknown server", "/nope", `{"enabled":true}`, "not_found", 404},
		{"bad name", "/Bad__Name", `{}`, "invalid_name", 400},
		{"field not in the entry", "/exa", `{"fields":{"other":"x"}}`, "unknown_field", 400},
		{"catalog url is fixed", "/exa", `{"url":"https://evil.example/mcp"}`, "invalid_request", 400},
		{"catalog headers are fixed", "/exa", `{"headers":{"X":"y"}}`, "invalid_request", 400},
		{"catalog transport is fixed", "/exa", `{"transport":"sse"}`, "invalid_request", 400},
		{"fields on a custom server", "/mine", `{"fields":{"a":"b"}}`, "unknown_field", 400},
		{"custom url must be https", "/mine", `{"url":"http://mine.example/mcp"}`, "invalid_url", 400},
		{"custom transport", "/mine", `{"transport":"stdio"}`, "invalid_transport", 400},
		{"reserved header", "/mine", `{"headers":{"Host":"x"}}`, "invalid_header", 400},
		{"value too long", "/exa", `{"fields":{"api_key":"` + strings.Repeat("x", 4097) + `"}}`, "invalid_value", 400},
		{"control character", "/exa", `{"fields":{"api_key":"a\nb"}}`, "invalid_value", 400},
		{"empty tool name", "/exa", `{"tools":[" "]}`, "invalid_tools", 400},
		{"too many tools", "/exa", `{"tools":[` + strings.TrimSuffix(strings.Repeat(`"t",`, 101), ",") + `]}`, "invalid_tools", 400},
	}

	// Act / Assert
	for _, c := range cases {
		if got := r.errCode("PUT", r.pbase()+c.path, "alice", c.body, c.status); got != c.code {
			t.Errorf("%s: code %q, want %q", c.name, got, c.code)
		}
	}
	if in := r.installs(capabilities.ScopeProject, r.project); len(in) != 1 {
		t.Fatalf("a refused request left an install behind: %+v", in)
	}
}

func TestACustomServerIsAddedUpdatedAndRemovedWithoutEverShowingItsHeaders(t *testing.T) {
	// Arrange
	r := newCapRig(t, capKey)

	// Act
	added := r.view("POST", r.pbase(), "alice", `{"name":"wiki","url":"https://wiki.example/mcp","transport":"sse","headers":{"x-token":"T0PSECRET","Authorization":"Bearer B"},"tools":["read"]}`, 201)
	_, raw := r.call("GET", r.pbase(), "bob", "")
	merged := r.view("PUT", r.pbase()+"/wiki", "alice", `{"headers":{"X-Token":"","X-Other":"o"},"url":"https://wiki2.example/mcp","enabled":false}`, 200)
	r.view("PUT", r.pbase()+"/wiki", "alice", `{"enabled":true,"tools":[]}`, 200)
	removed, _ := r.call("DELETE", r.pbase()+"/wiki", "alice", "")
	again, _ := r.call("DELETE", r.pbase()+"/wiki", "alice", "")

	// Assert
	if added.Kind != "custom" || !added.Enabled || added.Transport != "sse" || !equalStrings(added.HeaderNames, []string{"Authorization", "X-Token"}) || !equalStrings(added.Tools, []string{"read"}) || added.Title != "wiki" {
		t.Fatalf("added = %+v", added)
	}
	if strings.Contains(raw, "T0PSECRET") || strings.Contains(raw, "Bearer B") {
		t.Fatal("a stored header value is in the listing")
	}
	if merged.Enabled || merged.URL != "https://wiki2.example/mcp" || !equalStrings(merged.HeaderNames, []string{"Authorization", "X-Other"}) {
		t.Fatalf("merged = %+v", merged)
	}
	if removed != 204 || again != 404 || len(r.installs(capabilities.ScopeProject, r.project)) != 0 {
		t.Fatalf("delete: %d then %d", removed, again)
	}
	if !equalStrings(r.actions(), []string{"capability.add", "capability.disable", "capability.enable", "capability.remove"}) {
		t.Fatalf("audit = %v", r.actions())
	}
}

func TestAddingACustomServerRefusesBadInput(t *testing.T) {
	// Arrange
	r := newCapRig(t, capKey)
	hdrs := func(n int) string {
		var parts []string
		for i := 0; i < n; i++ {
			parts = append(parts, `"X-H`+strings.Repeat("a", i)+`":"v"`)
		}
		return `{` + strings.Join(parts, ",") + `}`
	}
	cases := []struct {
		name, body, code string
		status           int
	}{
		{"no name", `{"url":"https://a.example/mcp"}`, "invalid_name", 400},
		{"upper case", `{"name":"Wiki","url":"https://a.example/mcp"}`, "invalid_name", 400},
		{"double underscore", `{"name":"a__b","url":"https://a.example/mcp"}`, "invalid_name", 400},
		{"catalog name", `{"name":"exa","url":"https://a.example/mcp"}`, "name_taken", 409},
		{"no url", `{"name":"a"}`, "invalid_url", 400},
		{"http", `{"name":"a","url":"http://a.example/mcp"}`, "invalid_url", 400},
		{"userinfo", `{"name":"a","url":"https://u:p@a.example/mcp"}`, "invalid_url", 400},
		{"loopback", `{"name":"a","url":"https://127.0.0.1/mcp"}`, "invalid_url", 400},
		{"private", `{"name":"a","url":"https://10.1.2.3/mcp"}`, "invalid_url", 400},
		{"metadata", `{"name":"a","url":"https://169.254.169.254/latest"}`, "invalid_url", 400},
		{"ipv6 loopback", `{"name":"a","url":"https://[::1]/mcp"}`, "invalid_url", 400},
		{"mapped ipv4", `{"name":"a","url":"https://[::ffff:10.0.0.1]/mcp"}`, "invalid_url", 400},
		{"cgnat", `{"name":"a","url":"https://100.64.1.1/mcp"}`, "invalid_url", 400},
		{"localhost", `{"name":"a","url":"https://localhost/mcp"}`, "invalid_url", 400},
		{"subdomain of localhost", `{"name":"a","url":"https://x.localhost./mcp"}`, "invalid_url", 400},
		{"not a url", `{"name":"a","url":"https://"}`, "invalid_url", 400},
		{"transport", `{"name":"a","url":"https://a.example/mcp","transport":"ws"}`, "invalid_transport", 400},
		{"fields", `{"name":"a","url":"https://a.example/mcp","fields":{"k":"v"}}`, "unknown_field", 400},
		{"nine headers", `{"name":"a","url":"https://a.example/mcp","headers":` + hdrs(9) + `}`, "too_many", 400},
		{"bad header name", `{"name":"a","url":"https://a.example/mcp","headers":{"bad name":"v"}}`, "invalid_header", 400},
		{"mcp header", `{"name":"a","url":"https://a.example/mcp","headers":{"Mcp-Session-Id":"v"}}`, "invalid_header", 400},
		{"duplicate header", `{"name":"a","url":"https://a.example/mcp","headers":{"x-a":"1","X-A":"2"}}`, "invalid_header", 400},
		{"long value", `{"name":"a","url":"https://a.example/mcp","headers":{"X-A":"` + strings.Repeat("v", 4097) + `"}}`, "invalid_value", 400},
		{"tools", `{"name":"a","url":"https://a.example/mcp","tools":[""]}`, "invalid_tools", 400},
	}

	// Act / Assert
	for _, c := range cases {
		if got := r.errCode("POST", r.pbase(), "alice", c.body, c.status); got != c.code {
			t.Errorf("%s: code %q, want %q", c.name, got, c.code)
		}
	}
	ok := r.view("POST", r.pbase(), "alice", `{"name":"eight","url":"https://a.example/mcp","headers":`+hdrs(8)+`}`, 201)
	if len(ok.HeaderNames) != 8 {
		t.Fatalf("eight headers are allowed: %+v", ok)
	}
	r.errCode("POST", r.pbase(), "alice", `{"name":"eight","url":"https://a.example/mcp"}`, 409) // taken by a custom server
	r.errCode("PUT", r.pbase()+"/eight", "alice", `{"headers":`+hdrs(9)+`}`, 400)
}

func TestAScopeCanHaveTwentyCustomServersNoMore(t *testing.T) {
	// Arrange
	r := newCapRig(t, capKey)
	add := func(i int) (int, string) {
		return r.call("POST", "/api/me/capabilities", "alice", `{"name":"s`+strings.Repeat("a", i)+`","url":"https://a.example/mcp"}`)
	}

	// Act
	for i := 0; i < 20; i++ {
		if code, body := add(i); code != 201 {
			t.Fatalf("server %d: %d %s", i, code, body)
		}
	}
	code, body := add(20)

	// Assert: the 21st is refused, another scope is unaffected, and removing one makes room
	if code != 400 || !strings.Contains(body, "too_many") {
		t.Fatalf("21st server: %d %s", code, body)
	}
	if code, _ := r.call("POST", r.pbase(), "alice", `{"name":"fine","url":"https://a.example/mcp"}`); code != 201 {
		t.Fatalf("the project's own scope is not independent: %d", code)
	}
	r.call("DELETE", "/api/me/capabilities/s", "alice", "")
	if code, _ := add(20); code != 201 {
		t.Fatalf("no room after a removal: %d", code)
	}
}

func TestDeletingACatalogInstallRestoresTheShippedDefault(t *testing.T) {
	// Arrange
	r := newCapRig(t, capKey)
	r.view("PUT", r.pbase()+"/exa", "alice", `{"enabled":false,"tools":["web_fetch_exa"],"fields":{"api_key":"K"}}`, 200)
	r.view("PUT", r.pbase()+"/github", "alice", `{"fields":{"token":"T"}}`, 200)

	// Act
	code, _ := r.call("DELETE", r.pbase()+"/exa", "alice", "")
	code2, _ := r.call("DELETE", r.pbase()+"/exa", "alice", "") // nothing left to reset: still fine
	code3, _ := r.call("DELETE", r.pbase()+"/nope", "alice", "")
	l := r.list(r.pbase(), "alice")

	// Assert
	exa := l["exa"]
	if code != 204 || code2 != 204 || code3 != 404 {
		t.Fatalf("statuses %d %d %d", code, code2, code3)
	}
	if !exa.Enabled || exa.Installed || exa.Fields[0].Set || len(exa.Tools) != 2 {
		t.Fatalf("exa after reset = %+v", exa)
	}
	if !l["github"].Installed {
		t.Fatal("deleting one server reset another")
	}
}

func TestUserAndProjectScopesAreSeparate(t *testing.T) {
	// Arrange
	r := newCapRig(t, capKey)

	// Act: alice configures her own copy; bob configures his; the project is untouched
	r.view("PUT", "/api/me/capabilities/github", "alice", `{"enabled":true,"fields":{"token":"ALICE"}}`, 200)
	r.view("POST", "/api/me/capabilities", "bob", `{"name":"bobs","url":"https://b.example/mcp"}`, 201)

	// Assert
	if !r.list("/api/me/capabilities", "alice")["github"].Enabled {
		t.Fatal("alice's own token did not enable her github")
	}
	if _, ok := r.list("/api/me/capabilities", "alice")["bobs"]; ok {
		t.Fatal("bob's server shows for alice")
	}
	if l := r.list(r.pbase(), "alice"); l["github"].Installed || l["bobs"].Name != "" {
		t.Fatalf("a personal install reached the project: %+v", l)
	}
	if len(r.actions()) != 0 {
		t.Fatalf("a user's own changes were written to the project's audit log: %v", r.actions())
	}
	r.errCode("PUT", "/api/me/capabilities/bobs", "alice", `{"enabled":false}`, 404)
}

func TestTheConnectionTestAnswersGenericallyAndSendsTheStoredCredentials(t *testing.T) {
	// Arrange
	r := newCapRig(t, capKey)
	r.view("PUT", r.pbase()+"/github", "alice", `{"fields":{"token":"TKN"}}`, 200)
	r.view("POST", r.pbase(), "alice", `{"name":"wiki","url":"https://wiki.example/mcp","headers":{"X-Token":"HDR"}}`, 201)
	post := func(name string) CapabilityTestResult {
		code, raw := r.call("POST", r.pbase()+"/"+name+"/test", "alice", "")
		if code != 200 {
			t.Fatalf("test %s: %d %s", name, code, raw)
		}
		var res CapabilityTestResult
		_ = json.Unmarshal([]byte(raw), &res)
		if strings.Contains(raw, "10.0.0.7") || strings.Contains(raw, "secret-body") {
			t.Fatalf("an upstream detail leaked: %s", raw)
		}
		return res
	}

	// Act / Assert: success lists tools, with the expanded credentials sent
	r.test = func(context.Context, config.MCPServer) ([]CapabilityTool, error) {
		return []CapabilityTool{{Name: "a", Description: "d", ReadOnly: true}}, nil
	}
	if res := post("github"); !res.OK || len(res.Tools) != 1 || !res.Tools[0].ReadOnly {
		t.Fatalf("success = %+v", res)
	}
	if s := r.tested[0]; s.URL != "https://api.githubcopilot.com/mcp/" || s.Headers["Authorization"] != "Bearer TKN" || !s.Untrusted || s.Name != "github" {
		t.Fatalf("tester got %+v", s)
	}
	r.test = func(context.Context, config.MCPServer) ([]CapabilityTool, error) { return nil, nil }
	if res := post("wiki"); !res.OK || len(res.Tools) != 0 {
		t.Fatalf("no tools = %+v", res)
	}
	if s := r.tested[1]; s.Headers["X-Token"] != "HDR" || s.Type != "http" {
		t.Fatalf("custom tester got %+v", s)
	}

	// Rejected credentials have their own message; every other error is the same generic one.
	r.test = func(context.Context, config.MCPServer) ([]CapabilityTool, error) {
		return nil, errors.New("dial 10.0.0.7: destination not allowed; secret-body")
	}
	if res := post("wiki"); res.OK || res.Error != "could not connect" {
		t.Fatalf("generic = %+v", res)
	}
	r.test = func(context.Context, config.MCPServer) ([]CapabilityTool, error) {
		return nil, errors.Join(ErrCredentialsRejected, errors.New("secret-body"))
	}
	if res := post("wiki"); res.OK || res.Error != "the server rejected the credentials" {
		t.Fatalf("rejected = %+v", res)
	}
}

func TestTheConnectionTestRefusesWhatCannotBeTested(t *testing.T) {
	// Arrange
	r := newCapRig(t, capKey)
	r.test = func(context.Context, config.MCPServer) ([]CapabilityTool, error) {
		t.Error("the server was contacted")
		return nil, nil
	}

	// Act
	_, missing := r.call("POST", r.pbase()+"/github/test", "alice", "") // token not set
	r.errCode("POST", r.pbase()+"/nope/test", "alice", "", 404)

	// Assert
	if !strings.Contains(missing, "required") || strings.Contains(missing, `"ok":true`) {
		t.Fatalf("missing = %s", missing)
	}
}

func TestSettingCredentialsNeedsASecretKeyButEnablingDoesNot(t *testing.T) {
	// Arrange
	r := newCapRig(t, nil)

	// Act / Assert
	if got := r.errCode("PUT", r.pbase()+"/github", "alice", `{"fields":{"token":"x"}}`, 501); got != "not_configured" {
		t.Fatalf("code %q", got)
	}
	r.errCode("POST", r.pbase(), "alice", `{"name":"k","url":"https://a.example/mcp","headers":{"X-A":"b"}}`, 501)
	r.view("PUT", r.pbase()+"/exa", "alice", `{"enabled":false}`, 200)
	r.view("POST", r.pbase(), "alice", `{"name":"open","url":"https://a.example/mcp"}`, 201)
	if len(r.installs(capabilities.ScopeProject, r.project)) != 2 {
		t.Fatal("keyless installs were not stored")
	}
}

func TestCredentialsSealedWithAnotherKeyAreNotSilentlyOverwritten(t *testing.T) {
	// Arrange: an install sealed under a key this server no longer has
	r := newCapRig(t, capKey)
	old, _ := newSecretBoxFor(capabilitiesDomain, []byte("the-previous-secret-key-0123456"))
	if _, err := r.caps.PutInstall(context.Background(), capabilities.Install{Scope: capabilities.ScopeProject, ScopeID: r.project,
		CatalogID: "github", Enabled: true, Secrets: old.seal([]byte(`{"token":"x"}`)), SecretKeys: []string{"token"}}); err != nil {
		t.Fatal(err)
	}

	// Act / Assert: the view says it is set, a merge is refused, the test says why, and removing starts over
	if !r.list(r.pbase(), "alice")["github"].Fields[0].Set {
		t.Fatal("the stored field is not shown as set")
	}
	if got := r.errCode("PUT", r.pbase()+"/github", "alice", `{"fields":{"token":"y"}}`, 409); got != "secrets_unreadable" {
		t.Fatalf("code %q", got)
	}
	r.test = func(context.Context, config.MCPServer) ([]CapabilityTool, error) { return nil, nil }
	if _, body := r.call("POST", r.pbase()+"/github/test", "alice", ""); !strings.Contains(body, "cannot be read") {
		t.Fatalf("test = %s", body)
	}
	r.call("DELETE", r.pbase()+"/github", "alice", "")
	r.view("PUT", r.pbase()+"/github", "alice", `{"fields":{"token":"y"}}`, 200)
}

func TestWithoutTheStoreOrATesterTheMarketplaceDegradesCleanly(t *testing.T) {
	// Arrange: no store, no routes
	tm := teamstore.NewMem()
	_, _ = tm.UpsertUser(context.Background(), store.User{Login: "alice", Email: "a@x.io"})
	plain := newRig(t, rigOpts{team: true, prod: Options{Team: tm}})

	// Act / Assert
	if got := plain.do("GET", "/api/me/capabilities", "alice", "").StatusCode; got != http.StatusNotFound && got != http.StatusMethodNotAllowed {
		t.Fatalf("routes exist without a store: %d", got)
	}

	// A store without a tester: everything works but the test action.
	caps := capstore.NewMem(nil)
	_ = caps.SeedCatalog(context.Background(), Catalog())
	tm2 := teamstore.NewMem()
	_, _ = tm2.UpsertUser(context.Background(), store.User{Login: "alice", Email: "a@x.io"})
	r := newRig(t, rigOpts{team: true, prod: Options{Team: tm2, Capabilities: caps, SecretKey: capKey}})
	if got := (&capRig{rig: r}).errCode("POST", "/api/me/capabilities/exa/test", "alice", "", 501); got != "not_configured" {
		t.Fatalf("code %q", got)
	}
}

func TestDeletingAProjectDeletesItsInstalls(t *testing.T) {
	// Arrange
	r := newCapRig(t, capKey)
	r.view("PUT", r.pbase()+"/github", "alice", `{"fields":{"token":"T"}}`, 200)
	r.view("POST", "/api/me/capabilities", "alice", `{"name":"mine","url":"https://a.example/mcp"}`, 201)

	// Act
	if code, body := r.call("DELETE", "/api/projects/"+r.project, "alice", ""); code != 204 {
		t.Fatalf("delete: %d %s", code, body)
	}

	// Assert: the project's keys are gone, the owner's personal servers are not
	if n := len(r.installs(capabilities.ScopeProject, r.project)); n != 0 {
		t.Fatalf("%d installs survive the project", n)
	}
	if n := len(r.installs(capabilities.ScopeUser, "alice")); n != 1 {
		t.Fatalf("personal installs = %d", n)
	}
}

func TestTheProjectIsDeletedEvenIfItsInstallsCannotBe(t *testing.T) {
	// Arrange
	tm := teamstore.NewMem()
	_, _ = tm.UpsertUser(context.Background(), store.User{Login: "alice", Email: "a@x.io"})
	caps := msFaultyDelete{capstore.NewMem(nil)}
	r := newRig(t, rigOpts{team: true, prod: Options{Team: tm, Capabilities: caps, SecretKey: capKey}})
	id := r.json("POST", "/api/projects", "alice", `{"name":"p"}`, 201)["id"].(string)

	// Act / Assert
	if code, body := (&capRig{rig: r}).call("DELETE", "/api/projects/"+id, "alice", ""); code != 204 {
		t.Fatalf("delete: %d %s", code, body)
	}
	r.json("GET", "/api/projects/"+id, "alice", "", 404)
}

type msFaultyDelete struct{ *capstore.Mem }

func (msFaultyDelete) DeleteInstalls(context.Context, capabilities.Scope, string) error {
	return errors.New("boom")
}

func TestStoreFailuresAnswerInternalWithoutLeakingTheCause(t *testing.T) {
	// Arrange
	tm := teamstore.NewMem()
	_, _ = tm.UpsertUser(context.Background(), store.User{Login: "alice", Email: "a@x.io"})
	faulty := &msFailingCaps{Mem: capstore.NewMem(nil)}
	_ = faulty.SeedCatalog(context.Background(), Catalog())
	r := newRig(t, rigOpts{team: true, prod: Options{Team: tm, Capabilities: faulty, SecretKey: capKey, MCPTester: func(context.Context, config.MCPServer) ([]CapabilityTool, error) { return nil, nil }}})
	cr := &capRig{rig: r}
	body := `{"name":"a","url":"https://a.example/mcp"}`

	// Act / Assert
	faulty.fail = "ListInstalls"
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/me/capabilities", ""}, {"PUT", "/api/me/capabilities/exa", `{"enabled":true}`}, {"POST", "/api/me/capabilities", body},
		{"DELETE", "/api/me/capabilities/exa", ""}, {"POST", "/api/me/capabilities/exa/test", ""},
	} {
		code, raw := cr.call(c.method, c.path, "alice", c.body)
		if code != 500 || strings.Contains(raw, "db-secret") {
			t.Errorf("%s %s: %d %s", c.method, c.path, code, raw)
		}
	}
	faulty.fail = "ListCatalog"
	if code, raw := cr.call("GET", "/api/me/capabilities", "alice", ""); code != 500 || strings.Contains(raw, "db-secret") {
		t.Errorf("catalog failure: %d %s", code, raw)
	}
	faulty.fail = "PutInstall"
	if code, raw := cr.call("PUT", "/api/me/capabilities/exa", "alice", `{"enabled":false}`); code != 500 || strings.Contains(raw, "db-secret") {
		t.Errorf("put failure: %d %s", code, raw)
	}
	faulty.fail = ""
	cr.call("PUT", "/api/me/capabilities/exa", "alice", `{"enabled":false}`)
	faulty.fail = "DeleteInstall"
	if code, raw := cr.call("DELETE", "/api/me/capabilities/exa", "alice", ""); code != 500 || strings.Contains(raw, "db-secret") {
		t.Errorf("delete failure: %d %s", code, raw)
	}
}

// msFailingCaps fails the named operation with an error whose text must never reach a client.
type msFailingCaps struct {
	*capstore.Mem
	fail string
}

func (f *msFailingCaps) err(op string) error {
	if f.fail == op {
		return errors.New("db-secret " + op)
	}
	return nil
}

func (f *msFailingCaps) ListCatalog(ctx context.Context) ([]capabilities.Entry, error) {
	if err := f.err("ListCatalog"); err != nil {
		return nil, err
	}
	return f.Mem.ListCatalog(ctx)
}

func (f *msFailingCaps) ListInstalls(ctx context.Context, s capabilities.Scope, id string) ([]capabilities.Install, error) {
	if err := f.err("ListInstalls"); err != nil {
		return nil, err
	}
	return f.Mem.ListInstalls(ctx, s, id)
}

func (f *msFailingCaps) PutInstall(ctx context.Context, in capabilities.Install) (*capabilities.Install, error) {
	if err := f.err("PutInstall"); err != nil {
		return nil, err
	}
	return f.Mem.PutInstall(ctx, in)
}

func (f *msFailingCaps) DeleteInstall(ctx context.Context, id string) error {
	if err := f.err("DeleteInstall"); err != nil {
		return err
	}
	return f.Mem.DeleteInstall(ctx, id)
}
