package product

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/capstore"
	"github.com/7ar-chat/harness/adapters/store/executorstore"
	"github.com/7ar-chat/harness/adapters/store/rolestore"
	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/capabilities"
	"github.com/7ar-chat/harness/core/executors"
	"github.com/7ar-chat/harness/core/store"
)

type execRig struct {
	*rig
	api     *ExecutorAPI
	caps    *capstore.Mem
	defs    *executorstore.Mem
	project string
}

func newExecRig(t *testing.T) *execRig {
	t.Helper()
	tm := teamstore.NewMem()
	for _, l := range []string{"alice", "bob", "vic", "carol"} {
		if _, err := tm.UpsertUser(context.Background(), store.User{Login: l, Name: l, Email: l + "@x.io"}); err != nil {
			t.Fatal(err)
		}
	}
	caps, defs, rs := capstore.NewMem(nil), executorstore.NewMem(nil), rolestore.NewMem(nil)
	api, err := NewExecutorAPI(tm, caps, defs, []byte("a-secret-key-for-the-test-0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	model := llmtest.New(llmtest.Turn{Chunks: llmtest.Text("ok")}, llmtest.Turn{Chunks: llmtest.Text("Title")})
	er := &execRig{api: api, caps: caps, defs: defs}
	er.rig = newRig(t, rigOpts{team: true, model: model,
		prod: Options{Team: tm, Capabilities: caps, Executors: api, Roles: rs, SecretKey: []byte("a-secret-key-for-the-test-0123456789")},
		mod:  func(o *harness.Options) { o.Roles = NewRoleRegistry(tm, rs) }})
	er.project = er.json("POST", "/api/projects", "alice", `{"name":"api"}`, 201)["id"].(string)
	er.json("PUT", "/api/projects/"+er.project+"/members/bob", "alice", `{"role":"member"}`, 204)
	er.json("PUT", "/api/projects/"+er.project+"/members/vic", "alice", `{"role":"viewer"}`, 204)
	return er
}

func (r *execRig) call(method, path, user, body string) (int, string) {
	r.t.Helper()
	resp := r.do(method, path, user, body)
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (r *execRig) list(path, user string) map[string]ExecutorView {
	r.t.Helper()
	code, raw := r.call("GET", path, user, "")
	if code != 200 {
		r.t.Fatalf("GET %s: %d %s", path, code, raw)
	}
	var vs []ExecutorView
	if err := json.Unmarshal([]byte(raw), &vs); err != nil {
		r.t.Fatal(err)
	}
	out := map[string]ExecutorView{}
	for _, v := range vs {
		out[v.Name] = v
	}
	return out
}

const myCLI = `{"name":"mycli","title":"My CLI","command":"mycli --yolo {task}","binary":"mycli","credentialEnv":["MYCLI_KEY"],"modeFlags":{"read-only":"--ro"}}`

func TestExecutorsListBuiltinsAndProjectDefinitions(t *testing.T) {
	r := newExecRig(t)
	if code, raw := r.call("POST", "/api/projects/"+r.project+"/executors", "alice", myCLI); code != 201 {
		t.Fatalf("create: %d %s", code, raw)
	}
	all := r.list("/api/executors?project="+r.project, "bob")
	for _, n := range []string{"claude-code", "codex", "opencode", "mycli"} {
		if _, ok := all[n]; !ok {
			t.Errorf("missing %s in %v", n, all)
		}
	}
	if all["claude-code"].Editable || all["mycli"].Editable {
		t.Error("a member cannot edit")
	}
	if !r.list("/api/projects/"+r.project+"/executors", "alice")["mycli"].Editable {
		t.Error("the owner can edit")
	}
	if got := r.list("/api/executors", "bob"); len(got) != 3 {
		t.Errorf("without a project only the builtins: %v", got)
	}
	if code, _ := r.call("GET", "/api/executors?project="+r.project, "carol", ""); code != 403 && code != 404 {
		t.Errorf("an outsider got %d", code)
	}
}

func TestExecutorsDefinitionsAreOwnerOnlyAndValidated(t *testing.T) {
	r := newExecRig(t)
	base := "/api/projects/" + r.project + "/executors"
	for _, user := range []string{"bob", "vic", "carol"} {
		if code, _ := r.call("POST", base, user, myCLI); code < 400 {
			t.Errorf("%s created an executor: %d", user, code)
		}
	}
	for name, body := range map[string]string{
		"builtin name": `{"name":"codex","command":"x {task}","binary":"x"}`,
		"no task":      `{"name":"x1","command":"x","binary":"x"}`,
		"bad env":      `{"name":"x2","command":"x {task}","binary":"x","credentialEnv":["PATH"]}`,
		"bad binary":   `{"name":"x3","command":"x {task}","binary":"x; rm -rf /"}`,
	} {
		if code, raw := r.call("POST", base, "alice", body); code != 400 {
			t.Errorf("%s: %d %s", name, code, raw)
		}
	}
	if code, _ := r.call("POST", base, "alice", myCLI); code != 201 {
		t.Fatal(code)
	}
	if code, _ := r.call("POST", base, "alice", myCLI); code != 409 {
		t.Errorf("duplicate: %d", code)
	}
	if code, raw := r.call("PUT", base+"/mycli", "alice", strings.Replace(myCLI, "My CLI", "Renamed", 1)); code != 200 || !strings.Contains(raw, "Renamed") {
		t.Errorf("update: %d %s", code, raw)
	}
	if code, _ := r.call("DELETE", base+"/mycli", "bob", ""); code < 400 {
		t.Errorf("a member deleted: %d", code)
	}
	if code, _ := r.call("DELETE", base+"/mycli", "alice", ""); code != 204 {
		t.Errorf("delete: %d", code)
	}
	if code, _ := r.call("DELETE", base+"/mycli", "alice", ""); code != 404 {
		t.Errorf("delete twice: %d", code)
	}
}

func TestExecutorCredentialsAreSealedWriteOnlyAndPerUser(t *testing.T) {
	r := newExecRig(t)
	const key = "sk-ant-this-is-a-very-secret-key-1234"
	code, raw := r.call("PUT", "/api/me/executors/claude-code/credentials", "bob", `{"env":{"ANTHROPIC_API_KEY":"`+key+`","ANTHROPIC_BASE_URL":"https://proxy.example.com/v1"}}`)
	if code != 200 || strings.Contains(raw, key) || !strings.Contains(raw, `"configured":true`) {
		t.Fatalf("put: %d %s", code, raw)
	}
	// No route returns the values.
	for _, path := range []string{"/api/executors", "/api/executors?project=" + r.project, "/api/projects/" + r.project + "/executors"} {
		if code, raw := r.call("GET", path, "bob", ""); code != 200 || strings.Contains(raw, key) || strings.Contains(raw, "proxy.example.com") {
			t.Errorf("GET %s leaks: %d %s", path, code, raw)
		}
	}
	if !r.list("/api/executors", "bob")["claude-code"].Configured || r.list("/api/executors", "alice")["claude-code"].Configured {
		t.Error("configured must be per user")
	}
	// Sealed at rest.
	installs, _ := r.caps.ListInstalls(context.Background(), capabilities.ScopeUser, "bob")
	if len(installs) != 1 || installs[0].Name != "exec_claude-code" || strings.Contains(string(installs[0].Secrets), key) || len(installs[0].Secrets) == 0 {
		t.Fatalf("installs %+v", installs)
	}
	// The harness gets the values for the owner only.
	got, err := r.api.Credentials(context.Background(), "bob", "claude-code")
	if err != nil || got["ANTHROPIC_API_KEY"] != key || got["ANTHROPIC_BASE_URL"] != "https://proxy.example.com/v1" {
		t.Fatalf("credentials %v %v", got, err)
	}
	if got, _ := r.api.Credentials(context.Background(), "alice", "claude-code"); len(got) != 0 {
		t.Fatalf("alice sees %v", got)
	}
	// The marketplace does not list the install as an MCP server.
	if code, raw := r.call("GET", "/api/me/capabilities", "bob", ""); code != 200 || strings.Contains(raw, "exec_") {
		t.Errorf("marketplace shows it: %d %s", code, raw)
	}
	// A partial update keeps what is there; an empty value removes a key; unknown keys are refused.
	if code, _ := r.call("PUT", "/api/me/executors/claude-code/credentials", "bob", `{"env":{"ANTHROPIC_BASE_URL":""}}`); code != 200 {
		t.Fatal(code)
	}
	got, _ = r.api.Credentials(context.Background(), "bob", "claude-code")
	if got["ANTHROPIC_API_KEY"] != key || got["ANTHROPIC_BASE_URL"] != "" {
		t.Fatalf("after partial update %v", got)
	}
	if code, _ := r.call("PUT", "/api/me/executors/claude-code/credentials", "bob", `{"env":{"PATH":"/tmp"}}`); code != 400 {
		t.Errorf("unknown variable: %d", code)
	}
	if code, _ := r.call("PUT", "/api/me/executors/claude-code/credentials", "bob", `{"env":{"ANTHROPIC_BASE_URL":"javascript:alert(1)"}}`); code != 400 {
		t.Errorf("bad url: %d", code)
	}
	if code, _ := r.call("PUT", "/api/me/executors/nope/credentials", "bob", `{"env":{}}`); code != 404 {
		t.Errorf("unknown executor: %d", code)
	}
	if code, _ := r.call("DELETE", "/api/me/executors/claude-code/credentials", "bob", ""); code != 204 {
		t.Fatal(code)
	}
	if got, _ := r.api.Credentials(context.Background(), "bob", "claude-code"); len(got) != 0 {
		t.Fatalf("after delete %v", got)
	}
	if r.list("/api/executors", "bob")["claude-code"].Configured {
		t.Error("still configured")
	}
}

func TestCustomExecutorCredentialsNeedMembership(t *testing.T) {
	r := newExecRig(t)
	r.call("POST", "/api/projects/"+r.project+"/executors", "alice", myCLI)
	path := "/api/me/executors/mycli/credentials"
	if code, _ := r.call("PUT", path, "bob", `{"env":{"MYCLI_KEY":"abcdefghij"}}`); code != 404 {
		t.Errorf("without ?project the custom executor is unknown: %d", code)
	}
	if code, _ := r.call("PUT", path+"?project="+r.project, "carol", `{"env":{"MYCLI_KEY":"abcdefghij"}}`); code < 400 {
		t.Errorf("an outsider sealed a key: %d", code)
	}
	if code, _ := r.call("PUT", path+"?project="+r.project, "bob", `{"env":{"MYCLI_KEY":"abcdefghij"}}`); code != 200 {
		t.Errorf("member: %d", code)
	}
}

func TestResolverFindsProjectExecutorsThroughTheThread(t *testing.T) {
	r := newExecRig(t)
	r.call("POST", "/api/projects/"+r.project+"/executors", "alice", myCLI)
	q := executors.Query{Login: "bob", ProjectID: r.project}
	if e, err := r.api.Resolve(context.Background(), q, "mycli"); err != nil || e.Command == "" {
		t.Fatalf("%v %v", e, err)
	}
	if _, err := r.api.Resolve(context.Background(), executors.Query{Login: "bob"}, "mycli"); err == nil {
		t.Fatal("a custom executor resolved with no project")
	}
	if e, err := r.api.Resolve(context.Background(), executors.Query{Login: "bob"}, "codex"); err != nil || e.Kind != executors.KindBuiltin {
		t.Fatalf("%v %v", e, err)
	}
	r.call("PUT", "/api/projects/"+r.project+"/executors/mycli", "alice", strings.Replace(myCLI, `"binary"`, `"enabled":false,"binary"`, 1))
	if _, err := r.api.Resolve(context.Background(), q, "mycli"); err == nil {
		t.Fatal("a disabled executor resolved")
	}
	av, _ := r.api.Available(context.Background(), q)
	for _, e := range av {
		if e.Name == "mycli" {
			t.Fatal("a disabled executor is offered")
		}
	}
}

func TestRolesCarryAnExecutorThatMustExist(t *testing.T) {
	r := newExecRig(t)
	base := "/api/projects/" + r.project + "/roles"
	if code, raw := r.call("POST", base, "bob", `{"name":"cc","prompt":"p","executor":"claude-code"}`); code != 201 || !strings.Contains(raw, `"executor":"claude-code"`) {
		t.Fatalf("builtin executor: %d %s", code, raw)
	}
	if code, _ := r.call("POST", base, "bob", `{"name":"ghost","prompt":"p","executor":"nope"}`); code != 400 {
		t.Errorf("unknown executor: %d", code)
	}
	r.call("POST", "/api/projects/"+r.project+"/executors", "alice", myCLI)
	if code, _ := r.call("PUT", base+"/cc", "bob", `{"prompt":"p","executor":"mycli"}`); code != 200 {
		t.Errorf("project executor: %d", code)
	}
	if code, _ := r.call("PUT", base+"/cc", "bob", `{"prompt":"p"}`); code != 200 {
		t.Errorf("clearing: %d", code)
	}
	if _, raw := r.call("GET", base+"/cc", "bob", ""); strings.Contains(raw, `"executor"`) {
		t.Errorf("executor not cleared: %s", raw)
	}
}
