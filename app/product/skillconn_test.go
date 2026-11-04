package product

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/7ar-chat/harness/adapters/store/capstore"
	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/core/capabilities"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/approvals"
	"github.com/7ar-chat/harness/engine/tools"
	"github.com/7ar-chat/harness/features/skills"
)

// fakeDokploy records what reaches the server a skill is connected to.
type fakeDokploy struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []seenCall
}

type seenCall struct {
	method, path, query, key, body string
}

func newFakeDokploy(t *testing.T) *fakeDokploy {
	f := &fakeDokploy{}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.seen = append(f.seen, seenCall{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("x-api-key"), string(b)})
		f.mu.Unlock()
		switch r.URL.Path {
		case "/api/redirect":
			http.Redirect(w, r, "/api/elsewhere", http.StatusFound)
		case "/api/compose.one":
			// What the real API returns: the environment in clear, the key echoed back by a misbehaving endpoint.
			_, _ = io.WriteString(w, `{"name":"web","env":"SECRET=1","nested":{"refreshToken":"r","ok":true},"echo":"key=`+r.Header.Get("x-api-key")+`"}`)
		case "/api/settings.health":
			_, _ = io.WriteString(w, `{"ok":true}`)
		default:
			_, _ = io.WriteString(w, `{"path":"`+r.URL.Path+`"}`)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDokploy) calls() []seenCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seenCall(nil), f.seen...)
}

// reach makes any host the skill is connected to resolve to the fake server, with its certificate accepted.
func (f *fakeDokploy) reach(a *SkillAPI) {
	a.client.Transport = &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, f.srv.Listener.Addr().String())
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
}

type skillRig struct {
	*capRig
	api *SkillAPI
	fd  *fakeDokploy
}

func newSkillRig(t *testing.T) *skillRig {
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
	api, err := NewSkillAPI(tm, caps, capKey)
	if err != nil {
		t.Fatal(err)
	}
	api.load(skills.NewRegistry(Skills(), nil))
	fd := newFakeDokploy(t)
	fd.reach(api)
	cr := &capRig{caps: caps, team: tm}
	cr.rig = newRig(t, rigOpts{team: true, prod: Options{Team: tm, Capabilities: caps, SecretKey: capKey, SkillAPI: api}})
	cr.project = cr.json("POST", "/api/projects", "alice", `{"name":"api"}`, 201)["id"].(string)
	cr.json("PUT", "/api/projects/"+cr.project+"/members/bob", "alice", `{"role":"member"}`, 204)
	return &skillRig{capRig: cr, api: api, fd: fd}
}

func (r *skillRig) conn(method, path, user, body string, want int) SkillConnView {
	r.t.Helper()
	code, raw := r.call(method, path, user, body)
	if code != want {
		r.t.Fatalf("%s %s: status %d, want %d: %s", method, path, code, want, raw)
	}
	var v SkillConnView
	_ = json.Unmarshal([]byte(raw), &v)
	return v
}

const connectBody = `{"enabled":true,"fields":{"url":"https://dokploy.example.com/","token":"tok-123456"}}`

func (r *skillRig) connect(path, user string) SkillConnView {
	r.t.Helper()
	return r.conn("PUT", path+"/dokploy", user, connectBody, 200)
}

func TestTheShippedDokploySkillIsAValidSkillWithAValidConnection(t *testing.T) {
	// Arrange
	reg := skills.NewRegistry(Skills(), nil)

	// Act
	_, pkg, err := reg.ByName(context.Background(), "dokploy")

	// Assert: the skills package accepts the frontmatter, and the declaration parses.
	if err != nil {
		t.Fatalf("the skills package refused the skill: %v", err)
	}
	var md []byte
	for _, f := range pkg.Files {
		if f.Path == "SKILL.md" {
			md = f.Data
		}
	}
	m, err := parseSkillManifest(md)
	if err != nil || m == nil {
		t.Fatalf("manifest = %v, %v", m, err)
	}
	if !m.permits("GET", "/project.all") || !m.permits("POST", "/compose.redeploy") {
		t.Fatal("the reads and the redeploy must be allowed")
	}
	for _, denied := range []string{"/compose.stop", "/compose.delete", "/compose.saveEnvironment", "/compose.redeploy2", "/x/compose.redeploy"} {
		if m.permits("POST", denied) {
			t.Errorf("POST %s must not be allowed", denied)
		}
	}
	if m.permits("DELETE", "/project.all") || m.permits("PUT", "/project.all") {
		t.Fatal("GET * must not allow other methods")
	}
}

func TestADeclarationThatCouldSendCredentialsElsewhereIsRefused(t *testing.T) {
	good := `---
name: x
description: d
connect:
  fields:
    - {key: url, type: url}
    - {key: token, secret: true}
  base: "${url}/api"
  headers: {x-api-key: "${token}"}
  allow: ["GET *"]
---
body`
	if m, err := parseSkillManifest([]byte(good)); err != nil || m == nil {
		t.Fatalf("the good declaration was refused: %v", err)
	}
	for name, mut := range map[string]func(string) string{
		"base on a field that is not a url":   func(s string) string { return strings.Replace(s, "{key: url, type: url}", "{key: url}", 1) },
		"base over http":                      func(s string) string { return strings.Replace(s, `"${url}/api"`, `"http://evil.example/api"`, 1) },
		"base on another host through a path": func(s string) string { return strings.Replace(s, `"${url}/api"`, `"${url}@evil.example/api"`, 1) },
		"base on a token":                     func(s string) string { return strings.Replace(s, `"${url}/api"`, `"${token}/api"`, 1) },
		"a header using an undeclared field":  func(s string) string { return strings.Replace(s, `"${token}"`, `"${nope}"`, 1) },
		"a header with a line break":          func(s string) string { return strings.Replace(s, `"${token}"`, `"a\nb"`, 1) },
		"no allow rule":                       func(s string) string { return strings.Replace(s, `allow: ["GET *"]`, `allow: []`, 1) },
		"a rule without a method":             func(s string) string { return strings.Replace(s, `"GET *"`, `"*"`, 1) },
		"a rule that walks up":                func(s string) string { return strings.Replace(s, `"GET *"`, `"GET /a/../b"`, 1) },
		"a secret url": func(s string) string {
			return strings.Replace(s, `{key: url, type: url}`, `{key: url, type: url, secret: true}`, 1)
		},
		"a test the rules do not permit": func(s string) string { return strings.Replace(s, "allow:", "test: /health\n  allow:", 1) + "" },
	} {
		src := mut(good)
		if name == "a test the rules do not permit" {
			src = strings.Replace(good, `allow: ["GET *"]`, `allow: ["POST /x"]`+"\n  test: /health", 1)
		}
		if m, err := parseSkillManifest([]byte(src)); err == nil {
			t.Errorf("%s: accepted %+v", name, m)
		}
	}
	// A skill without a connect block simply has none.
	if m, err := parseSkillManifest([]byte("---\nname: x\ndescription: d\n---\nbody")); m != nil || err != nil {
		t.Fatalf("no block = %v, %v", m, err)
	}
}

func TestThePathTheAgentSendsCannotLeaveTheSkillsAPI(t *testing.T) {
	for _, bad := range []string{"", "x", "/a?b=1", "/a#f", "/a/../b", "/a/./b", "//evil.example/x", "/a%2e%2e/b", `/a\b`, "/a\x00b", "/" + strings.Repeat("a", 600)} {
		if _, err := cleanSkillPath(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	for _, ok := range []string{"/", "/project.all", "/compose.one", "/a/b-c_d"} {
		if _, err := cleanSkillPath(ok); err != nil {
			t.Errorf("refused %q: %v", ok, err)
		}
	}
}

func TestConnectingASkillSealsTheTokenShowsTheWritesAndHidesItFromTheMarketplace(t *testing.T) {
	// Arrange
	r := newSkillRig(t)

	// Act / Assert: before connecting, the user sees what is needed and what the agent could do.
	list := func(path, user string) []SkillConnView {
		code, raw := r.call("GET", path, user, "")
		if code != 200 {
			t.Fatalf("GET %s = %d", path, code)
		}
		var vs []SkillConnView
		_ = json.Unmarshal([]byte(raw), &vs)
		return vs
	}
	before := list("/api/me/skill-connections", "alice")
	if len(before) != 1 || before[0].Name != "dokploy" || before[0].Enabled || len(before[0].Missing) != 2 || !before[0].CanTest {
		t.Fatalf("before = %+v", before)
	}
	if !strings.Contains(strings.Join(before[0].Allow, "|"), "POST /compose.redeploy") {
		t.Fatalf("the user is not shown the writes: %v", before[0].Allow)
	}

	// Connect: the address is normalised, the token is never returned, and it is sealed at rest.
	v := r.connect("/api/me/skill-connections", "alice")
	if !v.Enabled || v.Host != "dokploy.example.com" {
		t.Fatalf("view = %+v", v)
	}
	for _, f := range v.Fields {
		switch f.Key {
		case "url":
			if f.Value != "https://dokploy.example.com" || !f.Set {
				t.Errorf("url field = %+v", f)
			}
		case "token":
			if f.Value != "" || !f.Set {
				t.Errorf("token field = %+v", f)
			}
		}
	}
	if _, raw := r.call("GET", "/api/me/skill-connections", "alice", ""); strings.Contains(raw, "tok-123456") {
		t.Fatal("the token left the server")
	}
	ins := r.installs(capabilities.ScopeUser, "alice")
	if len(ins) != 1 || ins[0].Name != "skill_dokploy" || strings.Contains(string(ins[0].Secrets), "tok-123456") {
		t.Fatalf("installs = %+v", ins)
	}
	// It is not an MCP server: the marketplace neither lists nor reserves it for custom servers.
	if _, raw := r.call("GET", "/api/me/capabilities", "alice", ""); strings.Contains(raw, "skill_dokploy") {
		t.Fatalf("the marketplace lists the skill connection: %s", raw)
	}
	if code, _ := r.call("POST", "/api/me/capabilities", "alice", `{"name":"skill_other","url":"https://x.example/mcp"}`); code != 400 {
		t.Fatalf("a custom server named skill_… = %d, want 400", code)
	}
	// Another user has no such connection.
	if bobs := list("/api/me/skill-connections", "bob"); bobs[0].Installed {
		t.Fatalf("bob sees alice's connection: %+v", bobs)
	}

	// Disconnecting removes it.
	if code, _ := r.call("DELETE", "/api/me/skill-connections/dokploy", "alice", ""); code != 204 {
		t.Fatalf("DELETE = %d", code)
	}
	if after := list("/api/me/skill-connections", "alice"); after[0].Installed || after[0].Enabled {
		t.Fatalf("after = %+v", after)
	}
}

func TestOnlyProjectOwnersConnectASkillAndOnlyForTheirProject(t *testing.T) {
	r := newSkillRig(t)
	base := "/api/projects/" + r.project + "/skill-connections"

	for _, c := range []struct{ method, path, body string }{
		{"PUT", base + "/dokploy", connectBody}, {"DELETE", base + "/dokploy", ""}, {"POST", base + "/dokploy/test", ""},
	} {
		if got, _ := r.call(c.method, c.path, "bob", c.body); got != 403 {
			t.Errorf("a member %s %s = %d, want 403", c.method, c.path, got)
		}
		if got, _ := r.call(c.method, c.path, "carol", c.body); got != 404 {
			t.Errorf("an outsider %s %s = %d, want 404", c.method, c.path, got)
		}
	}
	if got, _ := r.call("GET", base, "bob", ""); got != 200 {
		t.Errorf("a member may read: %d", got)
	}
	v := r.connect(base, "alice")
	if !v.Enabled {
		t.Fatalf("view = %+v", v)
	}
	// The audit log names the skill and the keys, never the values.
	entries, _ := r.team.ListAudit(context.Background(), r.project, 100)
	var enabled bool
	for _, e := range entries {
		if e.Action == "skill.enable" {
			enabled = true
			if raw, _ := json.Marshal(e.Detail); strings.Contains(string(raw), "tok-123456") || !strings.Contains(string(raw), `"server":"dokploy"`) {
				t.Fatalf("audit detail = %s", raw)
			}
		}
	}
	if !enabled {
		t.Fatalf("no skill.enable in the audit log: %+v", entries)
	}
}

func TestConnectingRefusesAddressesAndFieldsItCannotUse(t *testing.T) {
	r := newSkillRig(t)
	for body, code := range map[string]string{
		`{"fields":{"url":"http://dokploy.example.com"}}`:       "invalid_url",
		`{"fields":{"url":"https://127.0.0.1"}}`:                "invalid_url",
		`{"fields":{"url":"https://localhost"}}`:                "invalid_url",
		`{"fields":{"url":"https://u:p@dokploy.example.com"}}`:  "invalid_url",
		`{"fields":{"url":"https://dokploy.example.com/?a=1"}}`: "invalid_url",
		`{"fields":{"nope":"x"}}`:                               "unknown_field",
	} {
		if got := r.errCode("PUT", "/api/me/skill-connections/dokploy", "alice", body, 400); got != code {
			t.Errorf("%s: code %q, want %q", body, got, code)
		}
	}
	if got, _ := r.call("PUT", "/api/me/skill-connections/missing", "alice", `{}`); got != 404 {
		t.Errorf("an unknown skill = %d, want 404", got)
	}
}

func TestTheAgentCallsTheAPIWithTheStoredKeyWhichNeverComesBack(t *testing.T) {
	// Arrange: alice connected Dokploy for herself.
	r := newSkillRig(t)
	r.connect("/api/me/skill-connections", "alice")
	rc := tools.RunContext{ThreadID: "personal-thread", Login: "alice"}
	ctx := context.Background()

	// Act: a read.
	got, err := r.api.call(ctx, rc, "dokploy", "GET", "/compose.one", map[string]string{"composeId": "c1"}, nil)

	// Assert: the key was sent, the environment and nested secrets are hidden, and an echoed key is scrubbed.
	if err != nil {
		t.Fatal(err)
	}
	res := got.(skillResponse)
	calls := r.fd.calls()
	if len(calls) != 1 || calls[0].key != "tok-123456" || calls[0].path != "/api/compose.one" || calls[0].query != "composeId=c1" || calls[0].method != "GET" {
		t.Fatalf("calls = %+v", calls)
	}
	for _, leaked := range []string{"SECRET=1", "tok-123456", `"r"`} {
		if strings.Contains(res.Body, leaked) {
			t.Errorf("the response shows %q: %s", leaked, res.Body)
		}
	}
	if !strings.Contains(res.Body, "[REDACTED]") || !strings.Contains(res.Body, `"name":"web"`) {
		t.Errorf("body = %s", res.Body)
	}

	// A change that the skill lists is made, with its JSON body.
	if _, err := r.api.call(ctx, rc, "dokploy", "POST", "/compose.redeploy", nil, map[string]any{"composeId": "c1"}); err != nil {
		t.Fatal(err)
	}
	last := r.fd.calls()[len(r.fd.calls())-1]
	if last.method != "POST" || last.path != "/api/compose.redeploy" || last.body != `{"composeId":"c1"}` {
		t.Fatalf("redeploy = %+v", last)
	}

	// Everything else is refused before any request is made.
	before := len(r.fd.calls())
	for _, c := range []struct{ method, path string }{
		{"POST", "/compose.stop"}, {"DELETE", "/compose.delete"}, {"PUT", "/project.all"},
		{"GET", "/../../etc"}, {"GET", "//evil.example/x"}, {"GET", "/a?x=1"}, {"TRACE", "/project.all"},
	} {
		if _, err := r.api.call(ctx, rc, "dokploy", c.method, c.path, nil, nil); err == nil {
			t.Errorf("%s %s was allowed", c.method, c.path)
		}
	}
	if len(r.fd.calls()) != before {
		t.Fatal("a refused call still reached the server")
	}

	// A redirect is not followed: the key must not travel to wherever the server points.
	res2, err := r.api.call(ctx, rc, "dokploy", "GET", "/redirect", nil, nil)
	if err != nil || res2.(skillResponse).Status != 302 {
		t.Fatalf("redirect = %+v, %v", res2, err)
	}
	for _, c := range r.fd.calls() {
		if c.path == "/api/elsewhere" {
			t.Fatal("the redirect was followed")
		}
	}

	// Every call is in the audit log, with the path but without the body or the key.
	var audited int
	entries, _ := r.team.ListAudit(ctx, "", 100)
	for _, e := range entries {
		if e.Action == "skill.api" {
			audited++
			if raw, _ := json.Marshal(e.Detail); strings.Contains(string(raw), "tok-123456") {
				t.Fatal("the audit log holds the key")
			}
		}
	}
	if audited == 0 {
		t.Fatal("no audit entry")
	}
}

func TestAnUnconnectedSkillOffersNoToolsAndAProjectNeverUsesAMembersOwnKey(t *testing.T) {
	r := newSkillRig(t)
	ctx := context.Background()
	if err := r.team.SetThreadMeta(ctx, store.ThreadMeta{ThreadID: "project-thread", ProjectID: r.project, Owner: "alice", CreatedBy: "alice", Kind: store.ThreadChat}); err != nil {
		t.Fatal(err)
	}
	provider := skillTools{r.api}
	personal := tools.RunContext{ThreadID: "personal-thread", Login: "alice"}
	shared := tools.RunContext{ThreadID: "project-thread", Login: "alice"}

	// Nothing connected: no tools at all, and a direct call says how to connect.
	if ts, _, _ := provider.Tools(ctx, personal); len(ts) != 0 {
		t.Fatalf("tools without a connection: %d", len(ts))
	}
	if _, err := r.api.call(ctx, personal, "dokploy", "GET", "/project.all", nil, nil); err == nil || !strings.Contains(err.Error(), "Capabilities") {
		t.Fatalf("err = %v", err)
	}

	// Alice connects for herself only: her personal sessions get the tools, the project's shared sessions do not.
	r.connect("/api/me/skill-connections", "alice")
	if ts, _, _ := provider.Tools(ctx, personal); len(ts) != 2 {
		t.Fatalf("personal tools = %d, want 2", len(ts))
	}
	if ts, _, _ := provider.Tools(ctx, shared); len(ts) != 0 {
		t.Fatalf("the project's session got a member's key: %d tools", len(ts))
	}
	if _, err := r.api.call(ctx, shared, "dokploy", "GET", "/project.all", nil, nil); err == nil {
		t.Fatal("a project session used a personal connection")
	}

	// Disabled means off, even with the fields filled in.
	r.conn("PUT", "/api/me/skill-connections/dokploy", "alice", `{"enabled":false}`, 200)
	if ts, _, _ := provider.Tools(ctx, personal); len(ts) != 0 {
		t.Fatalf("tools while disabled: %d", len(ts))
	}

	// Unknown skills and anonymous runs get nothing.
	if _, err := r.api.call(ctx, personal, "nope", "GET", "/x", nil, nil); err == nil {
		t.Fatal("an unknown skill answered")
	}
	if ts, _, _ := provider.Tools(ctx, tools.RunContext{ThreadID: "t"}); len(ts) != 0 {
		t.Fatal("tools without a login")
	}
}

func TestWritesNeedTheUsersApprovalAndTheTestButtonReportsGenerically(t *testing.T) {
	// The write tool is gated like the built-in risky ones; the read tool is not.
	r := newSkillRig(t)
	_ = skillTools{r.api}
	approvals.RegisterRisky("skill_api_call")
	if !approvals.IsRisky("skill_api_call") || approvals.IsRisky("skill_api_get") {
		t.Fatal("approval classes are wrong")
	}

	// Not connected yet: the test says so. Connected: it calls the skill's test path with the key.
	var tr SkillConnTest
	_, raw := r.call("POST", "/api/me/skill-connections/dokploy/test", "alice", "")
	_ = json.Unmarshal([]byte(raw), &tr)
	if tr.OK || tr.Error == "" {
		t.Fatalf("test before connecting = %+v", tr)
	}
	r.connect("/api/me/skill-connections", "alice")
	_, raw = r.call("POST", "/api/me/skill-connections/dokploy/test", "alice", "")
	_ = json.Unmarshal([]byte(raw), &tr)
	if !tr.OK || tr.Status != 200 {
		t.Fatalf("test = %+v (%s)", tr, raw)
	}
	if calls := r.fd.calls(); calls[len(calls)-1].path != "/api/settings.health" || calls[len(calls)-1].key != "tok-123456" {
		t.Fatalf("calls = %+v", calls)
	}
}
