package product

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/rolestore"
	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/roles"
	"github.com/7ar-chat/harness/core/store"
)

// roleRig is a team rig with roles on: alice owns the project, bob is a member, vic a viewer, carol is outside.
type roleRig struct {
	*rig
	rs      *rolestore.Mem
	team    *teamstore.Mem
	model   *llmtest.Model
	project string
}

func newRoleRig(t *testing.T) *roleRig {
	t.Helper()
	tm := teamstore.NewMem()
	for _, l := range []string{"alice", "bob", "vic", "carol"} {
		if _, err := tm.UpsertUser(context.Background(), store.User{Login: l, Name: l, Email: l + "@x.io"}); err != nil {
			t.Fatal(err)
		}
	}
	rs := rolestore.NewMem(nil)
	model := llmtest.New(llmtest.Turn{Chunks: llmtest.Text("ok")}, llmtest.Turn{Chunks: llmtest.Text("Title")})
	rr := &roleRig{rs: rs, team: tm, model: model}
	rr.rig = newRig(t, rigOpts{team: true, model: model, prod: Options{Team: tm, Roles: rs},
		mod: func(o *harness.Options) { o.Roles = NewRoleRegistry(tm, rs) }})
	rr.project = rr.json("POST", "/api/projects", "alice", `{"name":"api"}`, 201)["id"].(string)
	rr.json("PUT", "/api/projects/"+rr.project+"/members/bob", "alice", `{"role":"member"}`, 204)
	rr.json("PUT", "/api/projects/"+rr.project+"/members/vic", "alice", `{"role":"viewer"}`, 204)
	return rr
}

func (r *roleRig) base() string { return "/api/projects/" + r.project + "/roles" }

func (r *roleRig) call(method, path, user, body string) (int, string) {
	r.t.Helper()
	resp := r.do(method, path, user, body)
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (r *roleRig) view(method, path, user, body string, want int) RoleView {
	r.t.Helper()
	code, raw := r.call(method, path, user, body)
	if code != want {
		r.t.Fatalf("%s %s: status %d, want %d: %s", method, path, code, want, raw)
	}
	var v RoleView
	_ = json.Unmarshal([]byte(raw), &v)
	return v
}

func (r *roleRig) list(path, user string) map[string]RoleView {
	r.t.Helper()
	code, raw := r.call("GET", path, user, "")
	if code != 200 {
		r.t.Fatalf("GET %s: %d %s", path, code, raw)
	}
	var vs []RoleView
	if err := json.Unmarshal([]byte(raw), &vs); err != nil {
		r.t.Fatal(err)
	}
	out := map[string]RoleView{}
	for _, v := range vs {
		out[v.Name] = v
	}
	return out
}

const docsBody = `{"name":"docs","title":"Docs writer","description":"writes docs","prompt":"WRITE-DOCS-ONLY","model":"gpt-test","denyTools":["ping"],"skills":["writing"]}`

func TestRolesListMergesBuiltinsAndProjectRolesAndFlagsThem(t *testing.T) {
	// Arrange
	r := newRoleRig(t)
	r.view("POST", r.base(), "bob", docsBody, 201)
	r.view("POST", r.base(), "bob", `{"name":"coder","prompt":"my coder"}`, 201)

	// Act
	all := r.list(r.base(), "vic")

	// Assert
	for _, n := range []string{"planner", "reviewer", "tester", "explorer"} {
		if v := all[n]; !v.Builtin || v.Editable || v.Scope != roles.ScopeBuiltin {
			t.Errorf("%s = %+v", n, v)
		}
	}
	if d := all["docs"]; d.Builtin || d.Overrides || d.Scope != roles.ScopeProject || d.Editable {
		t.Errorf("docs (viewer) = %+v", d)
	}
	if c := all["coder"]; c.Builtin || !c.Overrides || c.Prompt != "my coder" {
		t.Errorf("coder = %+v", c)
	}
	if d := r.list(r.base(), "bob")["docs"]; !d.Editable {
		t.Error("a member cannot edit")
	}
	if len(all) != 6 {
		t.Errorf("%d roles, want the 5 builtins + docs", len(all))
	}
}

func TestRolesWritePermissions(t *testing.T) {
	r := newRoleRig(t)
	r.call("POST", r.base(), "bob", docsBody)
	cases := []struct {
		user, method, path, body string
		want                     int
	}{
		{"vic", "GET", r.base(), "", 200},
		{"vic", "POST", r.base(), docsBody, 403},
		{"vic", "PUT", r.base() + "/docs", docsBody, 403},
		{"vic", "DELETE", r.base() + "/docs", "", 403},
		{"vic", "POST", r.base() + "/planner/duplicate", `{}`, 403},
		{"carol", "GET", r.base(), "", 404},
		{"carol", "POST", r.base(), docsBody, 404},
		{"bob", "PUT", r.base() + "/docs", docsBody, 200},
		{"alice", "DELETE", r.base() + "/docs", "", 204},
	}
	for _, c := range cases {
		if code, raw := r.call(c.method, c.path, c.user, c.body); code != c.want {
			t.Errorf("%s %s %s: %d, want %d: %s", c.user, c.method, c.path, code, c.want, raw)
		}
	}
}

func TestRolesCreateValidatesAndRefusesDuplicates(t *testing.T) {
	r := newRoleRig(t)
	r.view("POST", r.base(), "bob", docsBody, 201)
	for name, c := range map[string]struct {
		body string
		want int
	}{
		"duplicate":    {docsBody, 409},
		"bad name":     {`{"name":"Bad Name","prompt":"x"}`, 400},
		"empty prompt": {`{"name":"x","prompt":" "}`, 400},
		"bad profile":  {`{"name":"x","prompt":"p","toolProfile":"root"}`, 400},
		"plan+write":   {`{"name":"x","prompt":"p","toolProfile":"write","permissionMode":"plan"}`, 400},
	} {
		if code, raw := r.call("POST", r.base(), "bob", c.body); code != c.want {
			t.Errorf("%s: %d, want %d: %s", name, code, c.want, raw)
		}
	}
}

func TestRolesUpdateKeepsHistoryAndRejectsAStaleVersion(t *testing.T) {
	// Arrange
	r := newRoleRig(t)
	v1 := r.view("POST", r.base(), "bob", docsBody, 201)

	// Act
	v2 := r.view("PUT", r.base()+"/docs", "alice", `{"prompt":"SECOND","version":1}`, 200)
	code, _ := r.call("PUT", r.base()+"/docs", "bob", `{"prompt":"third","version":1}`)
	disabled := r.view("PUT", r.base()+"/docs", "bob", `{"prompt":"third","enabled":false,"version":2}`, 200)
	kept := r.view("PUT", r.base()+"/docs", "bob", `{"prompt":"fourth","version":3}`, 200)
	code2, raw := r.call("GET", r.base()+"/docs/history", "vic", "")

	// Assert
	if v1.Version != 1 || v2.Version != 2 || v2.Prompt != "SECOND" || v2.UpdatedBy != "alice" || v2.CreatedBy != "bob" {
		t.Fatalf("v1 = %+v, v2 = %+v", v1, v2)
	}
	if code != 409 {
		t.Errorf("a stale update = %d, want 409", code)
	}
	if disabled.Enabled || kept.Enabled {
		t.Errorf("enabled should be kept when omitted: disabled=%v kept=%v", disabled.Enabled, kept.Enabled)
	}
	var hist []RoleVersion
	_ = json.Unmarshal([]byte(raw), &hist)
	if code2 != 200 || len(hist) != 3 || hist[0].Version != 3 || hist[2].Role.Prompt != "WRITE-DOCS-ONLY" {
		t.Errorf("history = %d %+v", code2, hist)
	}

	// Act: restore the first version
	restored := r.view("POST", r.base()+"/docs/restore", "bob", `{"version":1}`, 200)
	if restored.Prompt != "WRITE-DOCS-ONLY" || restored.Version != 5 {
		t.Errorf("restored = %+v", restored)
	}
	if code, _ := r.call("POST", r.base()+"/docs/restore", "bob", `{"version":99}`); code != 404 {
		t.Errorf("restoring an unknown version = %d", code)
	}
}

func TestRolesDuplicateABuiltinOverridesItAndDeleteBringsItBack(t *testing.T) {
	// Arrange
	r := newRoleRig(t)

	// Act
	cp := r.view("POST", r.base()+"/planner/duplicate", "bob", `{}`, 201)
	all := r.list(r.base(), "bob")
	named := r.view("POST", r.base()+"/planner/duplicate", "bob", `{"name":"planner-strict"}`, 201)
	again, _ := r.call("POST", r.base()+"/planner/duplicate", "bob", `{"name":"planner"}`)
	r.call("DELETE", r.base()+"/planner", "bob", "")
	back := r.list(r.base(), "bob")["planner"]

	// Assert
	if cp.Name != "planner" || cp.Builtin || !cp.Overrides || cp.Scope != roles.ScopeProject || cp.Prompt != harness.BuiltinRoles()[2].Prompt {
		t.Errorf("copy = %+v", cp)
	}
	if !all["planner"].Overrides || all["planner"].Builtin {
		t.Errorf("listing = %+v", all["planner"])
	}
	if named.Name != "planner-strict" || named.Overrides {
		t.Errorf("named copy = %+v", named)
	}
	if again != 409 {
		t.Errorf("a second override = %d, want 409", again)
	}
	if !back.Builtin {
		t.Errorf("after deleting the override: %+v", back)
	}
	if code, _ := r.call("DELETE", r.base()+"/planner", "bob", ""); code != 404 {
		t.Errorf("deleting a builtin = %d, want 404", code)
	}
}

func TestRolesPreviewShowsThePromptAndModeWithoutStoring(t *testing.T) {
	r := newRoleRig(t)

	code, raw := r.call("POST", r.base()+"/preview", "vic", `{"name":"p","title":"Planner","prompt":"PLAN-IT","toolProfile":"read-only","permissionMode":"ask","mcp":["github"]}`)

	var p RolePreview
	_ = json.Unmarshal([]byte(raw), &p)
	if code != 200 || !strings.Contains(p.Prompt, "## Role: Planner") || !strings.Contains(p.Prompt, "PLAN-IT") || p.Mode != "plan" || !p.AskFirst || len(p.Notes) < 3 {
		t.Errorf("preview = %d %+v", code, p)
	}
	if l, _ := r.rs.List(context.Background(), roles.ScopeProject, r.project); len(l) != 0 {
		t.Errorf("preview stored %v", l)
	}
	if code, _ := r.call("POST", r.base()+"/preview", "vic", `{"prompt":""}`); code != 400 {
		t.Errorf("an invalid preview = %d", code)
	}
}

func TestPersonalRolesAreTheCallersOwnAndOverrideTheProjectsInItsThreads(t *testing.T) {
	// Arrange
	r := newRoleRig(t)
	r.view("POST", "/api/me/roles", "bob", `{"name":"docs","prompt":"BOB-DOCS"}`, 201)
	r.view("POST", r.base(), "alice", docsBody, 201)

	// Act
	bobsView := r.list(r.base(), "bob")["docs"]
	alicesView := r.list(r.base(), "alice")["docs"]
	mine := r.list("/api/me/roles", "bob")
	carols := r.list("/api/me/roles", "carol")

	// Assert
	if bobsView.Scope != roles.ScopeUser || bobsView.Prompt != "BOB-DOCS" || bobsView.Editable {
		t.Errorf("bob in the project = %+v", bobsView)
	}
	if alicesView.Scope != roles.ScopeProject || alicesView.Prompt != "WRITE-DOCS-ONLY" {
		t.Errorf("alice in the project = %+v", alicesView)
	}
	if mine["docs"].Scope != roles.ScopeUser || !mine["docs"].Editable {
		t.Errorf("bob's own = %+v", mine["docs"])
	}
	if _, ok := carols["docs"]; ok {
		t.Error("carol sees bob's personal role")
	}
}

func TestARunInAProjectThreadTakesTheRoleFromTheComposer(t *testing.T) {
	// Arrange
	r := newRoleRig(t)
	r.view("POST", r.base(), "bob", docsBody, 201)
	th := r.json("POST", "/api/threads", "bob", `{"projectId":"`+r.project+`"}`, 201)["id"].(string)

	// Act
	run := r.json("POST", "/api/threads/"+th+"/runs", "bob", `{"message":"write it","role":"docs"}`, 202)["id"].(string)
	r.stream(run, "bob")
	bad := r.json("POST", "/api/threads/"+th+"/runs", "bob", `{"message":"again","role":"nope"}`, 400)

	// Assert
	var sys string
	for _, m := range r.model.Calls()[0].Messages {
		if m.Role == "system" {
			sys += m.Content
		}
	}
	if !strings.Contains(sys, "WRITE-DOCS-ONLY") || !strings.Contains(sys, "## Role: Docs writer") {
		t.Errorf("the role was not applied:\n%s", sys)
	}
	if bad["error"] == nil {
		t.Errorf("unknown role: %v", bad)
	}
}

func TestADisabledProjectRoleHidesTheBuiltinOfTheSameNameFromRuns(t *testing.T) {
	r := newRoleRig(t)
	r.view("POST", r.base(), "bob", `{"name":"planner","prompt":"x","enabled":false}`, 201)
	th := r.json("POST", "/api/threads", "bob", `{"projectId":"`+r.project+`"}`, 201)["id"].(string)

	r.json("POST", "/api/threads/"+th+"/runs", "bob", `{"message":"plan","role":"planner"}`, 400)
}

func TestDeletingAProjectDeletesItsRoles(t *testing.T) {
	r := newRoleRig(t)
	r.view("POST", r.base(), "bob", docsBody, 201)

	r.json("DELETE", "/api/projects/"+r.project, "alice", "", 204)

	if l, _ := r.rs.List(context.Background(), roles.ScopeProject, r.project); len(l) != 0 {
		t.Errorf("roles left: %v", l)
	}
}

func TestRegistryResolutionOrderAndDisabledRoles(t *testing.T) {
	// Arrange
	ctx := context.Background()
	rs := rolestore.NewMem(nil)
	reg := NewRoleRegistry(nil, rs)
	mk := func(scope roles.Scope, id, name, prompt string, enabled bool) {
		if _, err := rs.Create(ctx, roles.Role{Scope: scope, ScopeID: id, Name: name, Prompt: prompt, Enabled: enabled}, "x"); err != nil {
			t.Fatal(err)
		}
	}
	mk(roles.ScopeProject, "p1", "docs", "project", true)
	mk(roles.ScopeUser, "bob", "docs", "personal", true)
	mk(roles.ScopeProject, "p1", "coder", "project coder", false)

	// Act / Assert
	if r, err := reg.Resolve(ctx, roles.Query{Login: "bob", ProjectID: "p1"}, "docs"); err != nil || r.Prompt != "personal" {
		t.Errorf("personal over project: %v %v", r, err)
	}
	if r, err := reg.Resolve(ctx, roles.Query{Login: "ann", ProjectID: "p1"}, "docs"); err != nil || r.Prompt != "project" {
		t.Errorf("project: %v %v", r, err)
	}
	if _, err := reg.Resolve(ctx, roles.Query{Login: "ann", ProjectID: "p1"}, "coder"); err == nil {
		t.Error("a disabled override must hide the builtin")
	}
	if r, err := reg.Resolve(ctx, roles.Query{Login: "ann", ProjectID: "p2"}, "coder"); err != nil || !r.Builtin {
		t.Errorf("another project falls back to the builtin: %v %v", r, err)
	}
	if r, err := reg.Get(ctx, roles.Ref{Scope: roles.ScopeProject, ScopeID: "p1", Name: "coder"}); err != nil || r.Enabled {
		t.Errorf("Get returns a disabled role for a run that recorded it: %v %v", r, err)
	}
	if r, err := reg.Get(ctx, roles.Ref{Scope: roles.ScopeBuiltin, Name: "tester"}); err != nil || !r.Builtin {
		t.Errorf("builtin ref: %v %v", r, err)
	}
	avail, _ := reg.Available(ctx, roles.Query{Login: "ann", ProjectID: "p1"})
	for _, a := range avail {
		if a.Name == "coder" {
			t.Error("a disabled role is offered")
		}
	}
}
