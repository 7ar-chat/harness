package product

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/store"
)

// tmFaultTeam is a TeamStore whose named operations can be made to fail, to prove a storage failure
// answers 500 "internal" without leaking the cause.
type tmFaultTeam struct {
	store.TeamStore
	mu  sync.Mutex
	err map[string]error
}

const tmSecret = "tm-secret-db-failure"

func newTmFaultTeam(inner store.TeamStore) *tmFaultTeam {
	return &tmFaultTeam{TeamStore: inner, err: map[string]error{}}
}

func (f *tmFaultTeam) arm(op string) {
	f.mu.Lock()
	f.err[op] = errors.New(tmSecret)
	f.mu.Unlock()
}

func (f *tmFaultTeam) disarm(op string) {
	f.mu.Lock()
	delete(f.err, op)
	f.mu.Unlock()
}

func (f *tmFaultTeam) hit(op string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err[op]
}

func (f *tmFaultTeam) GetUser(ctx context.Context, login string) (*store.User, error) {
	if err := f.hit("GetUser"); err != nil {
		return nil, err
	}
	return f.TeamStore.GetUser(ctx, login)
}

func (f *tmFaultTeam) CreateProject(ctx context.Context, p store.Project, owner string) (*store.Project, error) {
	if err := f.hit("CreateProject"); err != nil {
		return nil, err
	}
	return f.TeamStore.CreateProject(ctx, p, owner)
}

func (f *tmFaultTeam) GetProject(ctx context.Context, id string) (*store.Project, error) {
	if err := f.hit("GetProject"); err != nil {
		return nil, err
	}
	return f.TeamStore.GetProject(ctx, id)
}

func (f *tmFaultTeam) UpdateProject(ctx context.Context, id string, p store.ProjectPatch) (*store.Project, error) {
	if err := f.hit("UpdateProject"); err != nil {
		return nil, err
	}
	return f.TeamStore.UpdateProject(ctx, id, p)
}

func (f *tmFaultTeam) DeleteProject(ctx context.Context, id string) error {
	if err := f.hit("DeleteProject"); err != nil {
		return err
	}
	return f.TeamStore.DeleteProject(ctx, id)
}

func (f *tmFaultTeam) ListProjects(ctx context.Context, login string) ([]store.ProjectRole, error) {
	if err := f.hit("ListProjects"); err != nil {
		return nil, err
	}
	return f.TeamStore.ListProjects(ctx, login)
}

func (f *tmFaultTeam) MemberRole(ctx context.Context, pid, login string) (string, error) {
	if err := f.hit("MemberRole"); err != nil {
		return "", err
	}
	return f.TeamStore.MemberRole(ctx, pid, login)
}

func (f *tmFaultTeam) ListMembers(ctx context.Context, pid string) ([]store.Member, error) {
	if err := f.hit("ListMembers"); err != nil {
		return nil, err
	}
	return f.TeamStore.ListMembers(ctx, pid)
}

func (f *tmFaultTeam) SetMember(ctx context.Context, pid, login, role string) error {
	if err := f.hit("SetMember"); err != nil {
		return err
	}
	return f.TeamStore.SetMember(ctx, pid, login, role)
}

func (f *tmFaultTeam) RemoveMember(ctx context.Context, pid, login string) error {
	if err := f.hit("RemoveMember"); err != nil {
		return err
	}
	return f.TeamStore.RemoveMember(ctx, pid, login)
}

func (f *tmFaultTeam) CreateInvite(ctx context.Context, in store.Invite) (*store.Invite, error) {
	if err := f.hit("CreateInvite"); err != nil {
		return nil, err
	}
	return f.TeamStore.CreateInvite(ctx, in)
}

func (f *tmFaultTeam) ListInvites(ctx context.Context, pid string) ([]store.Invite, error) {
	if err := f.hit("ListInvites"); err != nil {
		return nil, err
	}
	return f.TeamStore.ListInvites(ctx, pid)
}

func (f *tmFaultTeam) DeleteInvite(ctx context.Context, pid, email string) error {
	if err := f.hit("DeleteInvite"); err != nil {
		return err
	}
	return f.TeamStore.DeleteInvite(ctx, pid, email)
}

func (f *tmFaultTeam) SetThreadMeta(ctx context.Context, m store.ThreadMeta) error {
	if err := f.hit("SetThreadMeta"); err != nil {
		return err
	}
	return f.TeamStore.SetThreadMeta(ctx, m)
}

func (f *tmFaultTeam) GetThreadMeta(ctx context.Context, id string) (*store.ThreadMeta, error) {
	if err := f.hit("GetThreadMeta"); err != nil {
		return nil, err
	}
	return f.TeamStore.GetThreadMeta(ctx, id)
}

func (f *tmFaultTeam) ListProjectThreads(ctx context.Context, pid string) ([]store.ThreadMeta, error) {
	if err := f.hit("ListProjectThreads"); err != nil {
		return nil, err
	}
	return f.TeamStore.ListProjectThreads(ctx, pid)
}

func (f *tmFaultTeam) ListAudit(ctx context.Context, pid string, limit int) ([]store.AuditEntry, error) {
	if err := f.hit("ListAudit"); err != nil {
		return nil, err
	}
	return f.TeamStore.ListAudit(ctx, pid, limit)
}

func (f *tmFaultTeam) Audit(ctx context.Context, e store.AuditEntry) error {
	if err := f.hit("Audit"); err != nil {
		return err
	}
	return f.TeamStore.Audit(ctx, e)
}

func (f *tmFaultTeam) UserPrefs(ctx context.Context, login string) (map[string]any, error) {
	if err := f.hit("UserPrefs"); err != nil {
		return nil, err
	}
	return f.TeamStore.UserPrefs(ctx, login)
}

func (f *tmFaultTeam) SetUserPrefs(ctx context.Context, login string, p map[string]any) error {
	if err := f.hit("SetUserPrefs"); err != nil {
		return err
	}
	return f.TeamStore.SetUserPrefs(ctx, login, p)
}

// tmFaultRig is a team rig (alice owns "api", bob is a member, carol a viewer) over a faulty team store.
type tmFaultRig struct {
	*rig
	tm      *teamstore.Mem
	fault   *tmFaultTeam
	project string
}

func newTmFaultRig(t *testing.T) *tmFaultRig { return newTmFaultRigModel(t, nil) }

func newTmFaultRigModel(t *testing.T, model *llmtest.Model) *tmFaultRig {
	t.Helper()
	tm := teamstore.NewMem()
	for _, l := range []string{"alice", "bob", "carol"} {
		if _, err := tm.UpsertUser(context.Background(), store.User{Login: l, Name: l, Email: l + "@x.io"}); err != nil {
			t.Fatal(err)
		}
	}
	f := newTmFaultTeam(tm)
	r := newRig(t, rigOpts{team: true, model: model, prod: Options{Team: f}})
	p := r.json("POST", "/api/projects", "alice", `{"name":"api"}`, 201)["id"].(string)
	r.json("PUT", "/api/projects/"+p+"/members/bob", "alice", `{"role":"member"}`, 204)
	r.json("PUT", "/api/projects/"+p+"/members/carol", "alice", `{"role":"viewer"}`, 204)
	return &tmFaultRig{rig: r, tm: tm, fault: f, project: p}
}

func TestTmStorageFailuresAnswerInternalWithoutLeakingTheCause(t *testing.T) {
	// Arrange
	r := newTmFaultRig(t)
	th := r.json("POST", "/api/threads", "alice", `{"projectId":"`+r.project+`"}`, 201)["id"].(string)
	p := "/api/projects/" + r.project
	cases := []struct {
		op, method, path, body string
	}{
		{"ListProjects", "GET", "/api/projects", ""},
		{"ListProjects", "GET", "/api/me", ""},
		{"ListProjects", "GET", "/api/overview", ""},
		{"GetUser", "GET", "/api/me", ""},
		{"GetUser", "PUT", p + "/members/bob", `{"role":"member"}`},
		{"CreateProject", "POST", "/api/projects", `{"name":"x"}`},
		{"MemberRole", "GET", p, ""},
		{"MemberRole", "POST", "/api/threads", `{"projectId":"` + r.project + `"}`},
		{"MemberRole", "GET", "/api/threads?project=" + r.project, ""},
		{"UpdateProject", "PATCH", p, `{"name":"y"}`},
		{"DeleteProject", "DELETE", p, ""},
		{"ListMembers", "GET", p + "/members", ""},
		{"SetMember", "PUT", p + "/members/bob", `{"role":"viewer"}`},
		{"RemoveMember", "DELETE", p + "/members/bob", ""},
		{"ListInvites", "GET", p + "/invites", ""},
		{"CreateInvite", "POST", p + "/invites", `{"email":"new@x.io"}`},
		{"DeleteInvite", "DELETE", p + "/invites?email=new@x.io", ""},
		{"ListAudit", "GET", p + "/audit", ""},
		{"ListProjectThreads", "GET", "/api/threads?project=" + r.project, ""},
		{"SetThreadMeta", "POST", "/api/threads", `{}`},
		{"GetThreadMeta", "GET", "/api/threads/" + th, ""},
		{"GetThreadMeta", "POST", "/api/threads/" + th + "/runs", `{"message":"hi"}`},
	}
	for _, c := range cases {
		t.Run(c.op+" "+c.method+" "+c.path, func(t *testing.T) {
			// Act
			r.fault.arm(c.op)
			defer r.fault.disarm(c.op)
			resp := r.do(c.method, c.path, "alice", c.body)
			body := rawBody(resp.Body)

			// Assert
			if resp.StatusCode != 500 || strings.Contains(body, tmSecret) || !strings.Contains(body, `"internal"`) {
				t.Fatalf("status %d body %s: want a generic 500", resp.StatusCode, body)
			}
		})
	}
}

func TestTmAProjectWhoseRecordVanishesIsNotFound(t *testing.T) {
	// Arrange: the membership is readable but the project row cannot be loaded.
	r := newTmFaultRig(t)
	r.fault.arm("GetProject")

	// Act / Assert: the caller sees "not found", not a 500 and not a half-built project.
	r.json("GET", "/api/projects/"+r.project, "alice", "", 404)
	r.json("GET", "/api/projects/"+r.project+"/members", "alice", "", 404)
}

func TestTmCreateProjectValidation(t *testing.T) {
	r := newTmFaultRig(t)
	long := strings.Repeat("n", 101)
	bad := map[string]string{
		"empty name":      `{"name":""}`,
		"blank name":      `{"name":"   "}`,
		"101 characters":  `{"name":"` + long + `"}`,
		"http repo":       `{"name":"a","repoUrl":"http://github.com/x/y"}`,
		"file repo":       `{"name":"a","repoUrl":"file:///etc/passwd"}`,
		"no host":         `{"name":"a","repoUrl":"https://"}`,
		"not a url":       `{"name":"a","repoUrl":"github.com/x/y"}`,
		"javascript repo": `{"name":"a","repoUrl":"javascript:alert(1)"}`,
		"malformed json":  `{"name":`,
	}
	for name, body := range bad {
		if got := r.do("POST", "/api/projects", "alice", body).StatusCode; got != 400 {
			t.Errorf("%s: status %d, want 400", name, got)
		}
	}
	// Nothing was created by the rejected requests: alice still has only the rig's project.
	if list := r.json("GET", "/api/me", "alice", "", 200)["projects"].([]any); len(list) != 1 {
		t.Fatalf("rejected requests created projects: %v", list)
	}

	// The accepted shapes: 100 characters, trimmed names, https/ssh/git URLs, no URL at all.
	for name, body := range map[string]string{
		"100 characters": `{"name":"` + strings.Repeat("n", 100) + `"}`,
		"ssh":            `{"name":"s","repoUrl":"ssh://git@github.com/x/y.git"}`,
		"git":            `{"name":"g","repoUrl":"git://github.com/x/y.git"}`,
	} {
		if got := r.do("POST", "/api/projects", "alice", body).StatusCode; got != 201 {
			t.Errorf("%s: status %d, want 201", name, got)
		}
	}
	p := r.json("POST", "/api/projects", "alice", `{"name":"  padded  ","description":"d","defaultBranch":"dev"}`, 201)
	if p["name"] != "padded" || p["role"] != "owner" || p["createdBy"] != "alice" || p["defaultBranch"] != "dev" {
		t.Fatalf("created = %v", p)
	}
}

func TestTmProjectListsAreNeverNull(t *testing.T) {
	r := newTmFaultRig(t)
	// Zed was never seen by the identity provider: still a valid caller with an empty (not null) project list.
	if body := rawBody(r.do("GET", "/api/projects", "zed", "").Body); strings.TrimSpace(body) != "[]" {
		t.Fatalf("zed's projects = %q", body)
	}
	me := r.json("GET", "/api/me", "zed", "", 200)
	if u := me["user"].(map[string]any); u["login"] != "zed" || u["name"] != "zed" {
		t.Fatalf("me = %v", me)
	}
	if list, ok := me["projects"].([]any); !ok || len(list) != 0 {
		t.Fatalf("zed's projects = %v", me["projects"])
	}
	// A real user sees only the projects they belong to, with their role in each.
	rows := r.json("GET", "/api/me", "bob", "", 200)["projects"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["role"] != "member" {
		t.Fatalf("bob's projects = %v", rows)
	}
	if body := rawBody(r.do("GET", "/api/projects", "carol", "").Body); !strings.Contains(body, `"role":"viewer"`) {
		t.Fatalf("carol's projects = %s", body)
	}
	if body := rawBody(r.do("GET", "/api/projects/"+r.project+"/invites", "alice", "").Body); strings.TrimSpace(body) != "[]" {
		t.Fatalf("invites = %q, want []", body)
	}
}

func TestTmProjectAuthorizationByRole(t *testing.T) {
	// Arrange: alice owner, bob member, carol viewer, dave outsider (404: the project must not be probeable).
	r := newTmFaultRig(t)
	p := "/api/projects/" + r.project
	type row struct{ owner, member, viewer, outsider int }
	cases := map[string]struct {
		method, path, body string
		want               row
	}{
		"get":            {"GET", p, "", row{200, 200, 200, 404}},
		"list members":   {"GET", p + "/members", "", row{200, 200, 200, 404}},
		"audit":          {"GET", p + "/audit", "", row{200, 200, 403, 404}},
		"list invites":   {"GET", p + "/invites", "", row{200, 403, 403, 404}},
		"create invite":  {"POST", p + "/invites", `{"email":"n@x.io"}`, row{201, 403, 403, 404}},
		"delete invite":  {"DELETE", p + "/invites?email=zzz@x.io", "", row{404, 403, 403, 404}},
		"patch":          {"PATCH", p, `{"description":"d"}`, row{200, 403, 403, 404}},
		"put member":     {"PUT", p + "/members/bob", `{"role":"member"}`, row{204, 403, 403, 404}},
		"remove another": {"DELETE", p + "/members/nobody", "", row{404, 403, 403, 404}},
	}
	users := []struct {
		name string
		pick func(row) int
	}{
		{"alice", func(r row) int { return r.owner }},
		{"bob", func(r row) int { return r.member }},
		{"carol", func(r row) int { return r.viewer }},
		{"dave", func(r row) int { return r.outsider }},
	}
	for name, c := range cases {
		for _, u := range users {
			// Act
			got := r.do(c.method, c.path, u.name, c.body).StatusCode
			// Assert
			if want := u.pick(c.want); got != want {
				t.Errorf("%s as %s: status %d, want %d", name, u.name, got, want)
			}
		}
	}
	// Unknown project ids behave like projects the caller is not in.
	r.json("GET", "/api/projects/nope", "alice", "", 404)
	r.json("DELETE", "/api/projects/nope", "alice", "", 404)
}

func TestTmPatchProjectChangesOnlyTheNamedFields(t *testing.T) {
	r := newTmFaultRig(t)
	p := "/api/projects/" + r.project
	r.json("PATCH", p, "alice", `{"description":"first","repoUrl":"https://github.com/x/api","defaultBranch":"main"}`, 200)

	got := r.json("PATCH", p, "alice", `{"name":"renamed"}`, 200)

	if got["name"] != "renamed" || got["description"] != "first" || got["repoUrl"] != "https://github.com/x/api" || got["defaultBranch"] != "main" || got["role"] != "owner" {
		t.Fatalf("patched = %v", got)
	}
	// An explicit empty string clears a field (it is not "missing").
	if got = r.json("PATCH", p, "alice", `{"description":"","repoUrl":""}`, 200); got["description"] != nil || got["repoUrl"] != nil {
		t.Fatalf("cleared = %v", got)
	}
	// Invalid values are refused and leave the project as it was.
	for name, body := range map[string]string{
		"blank name": `{"name":" "}`, "empty name": `{"name":""}`, "long name": `{"name":"` + strings.Repeat("x", 101) + `"}`,
		"bad url": `{"repoUrl":"ftp://x/y"}`, "bad json": `{`,
	} {
		if status := r.do("PATCH", p, "alice", body).StatusCode; status != 400 {
			t.Errorf("%s: status %d, want 400", name, status)
		}
	}
	if cur := r.json("GET", p, "alice", "", 200); cur["name"] != "renamed" {
		t.Fatalf("a rejected patch changed the project: %v", cur)
	}
	if body := rawBody(r.do("GET", p+"/audit", "alice", "").Body); !strings.Contains(body, "project.update") {
		t.Fatalf("audit lacks the update: %s", body)
	}
}

func TestTmDeleteProjectRemovesItsThreadsAndTellsTheMembers(t *testing.T) {
	// Arrange
	r := newTmFaultRig(t)
	aliceThread := r.json("POST", "/api/threads", "alice", `{"projectId":"`+r.project+`"}`, 201)["id"].(string)
	bobThread := r.json("POST", "/api/threads", "bob", `{"projectId":"`+r.project+`"}`, 201)["id"].(string)
	personal := r.json("POST", "/api/threads", "alice", `{}`, 201)["id"].(string)
	bobFeed, carolFeed := feedOf(r.rig, "bob"), feedOf(r.rig, "carol")

	// Act
	if got := r.do("DELETE", "/api/projects/"+r.project, "alice", "").StatusCode; got != 204 {
		t.Fatalf("delete = %d", got)
	}

	// Assert: the project and its threads are gone for everyone, personal threads are untouched.
	for _, u := range []string{"alice", "bob", "carol"} {
		r.json("GET", "/api/projects/"+r.project, u, "", 404)
	}
	r.json("GET", "/api/threads/"+aliceThread, "alice", "", 404)
	r.json("GET", "/api/threads/"+bobThread, "bob", "", 404)
	r.json("GET", "/api/threads/"+personal, "alice", "", 200)
	if rows := r.json("GET", "/api/me", "bob", "", 200)["projects"].([]any); len(rows) != 0 {
		t.Fatalf("bob still has projects: %v", rows)
	}
	// Each former member is told individually, and only about this project.
	for name, ch := range map[string]<-chan string{"bob": bobFeed, "carol": carolFeed} {
		line := tmWaitLine(t, ch, `"project.changed"`)
		if !strings.Contains(line, r.project) {
			t.Errorf("%s: event %s lacks the project", name, line)
		}
	}
}

func TestTmMembersViewHidesEmailsFromNonOwners(t *testing.T) {
	r := newTmFaultRig(t)
	p := "/api/projects/" + r.project + "/members"
	r.tm.SetDisplayName(context.Background(), "bob", "Bobby B")

	owner := tmList(t, r.rig, p, "alice")
	member := tmList(t, r.rig, p, "bob")

	byLogin := func(l []map[string]any, login string) map[string]any {
		for _, m := range l {
			if m["login"] == login {
				return m
			}
		}
		t.Fatalf("no %s in %v", login, l)
		return nil
	}
	if byLogin(owner, "bob")["email"] != "bob@x.io" || byLogin(owner, "bob")["name"] != "Bobby B" {
		t.Fatalf("owner's view of bob = %v", byLogin(owner, "bob"))
	}
	if _, leaked := byLogin(member, "alice")["email"]; leaked {
		t.Fatalf("a member sees the owner's email: %v", byLogin(member, "alice"))
	}
	if byLogin(member, "alice")["role"] != "owner" || byLogin(member, "carol")["role"] != "viewer" {
		t.Fatalf("roles = %v", member)
	}
	// A member whose user record is gone still lists, named by login.
	r.tm.SetMember(context.Background(), r.project, "ghost", store.RoleViewer)
	if g := byLogin(tmList(t, r.rig, p, "alice"), "ghost"); g["name"] != "ghost" || g["email"] != nil {
		t.Fatalf("ghost = %v", g)
	}
}

func TestTmMemberManagement(t *testing.T) {
	r := newTmFaultRig(t)
	p := "/api/projects/" + r.project

	// role validation and unknown users
	r.json("PUT", p+"/members/bob", "alice", `{"role":"admin"}`, 400)
	r.json("PUT", p+"/members/bob", "alice", `{}`, 400)
	r.json("PUT", p+"/members/bob", "alice", `{`, 400)
	r.json("PUT", p+"/members/stranger", "alice", `{"role":"member"}`, 404) // must have logged in once

	// the last owner can be neither demoted nor removed
	r.json("PUT", p+"/members/alice", "alice", `{"role":"member"}`, 409)
	r.json("DELETE", p+"/members/alice", "alice", "", 409)
	if got := r.json("GET", p, "alice", "", 200)["role"]; got != "owner" {
		t.Fatalf("alice lost her role: %v", got)
	}

	// promoting a second owner lets the first one step down, and the new owner can then act
	r.json("PUT", p+"/members/bob", "alice", `{"role":"owner"}`, 204)
	r.json("PUT", p+"/members/alice", "bob", `{"role":"viewer"}`, 204)
	r.json("PATCH", p, "bob", `{"description":"bob runs it now"}`, 200)
	r.json("PATCH", p, "alice", `{"description":"x"}`, 403)

	// an owner removes anyone; removing somebody who is not a member is 404
	if got := r.do("DELETE", p+"/members/carol", "bob", "").StatusCode; got != 204 {
		t.Fatalf("remove carol = %d", got)
	}
	r.json("GET", p, "carol", "", 404)
	r.json("DELETE", p+"/members/carol", "bob", "", 404)
	// members may leave but not remove others; an outsider cannot even "leave"
	r.json("DELETE", p+"/members/bob", "alice", "", 403)
	r.json("DELETE", p+"/members/alice", "carol", "", 404)
	if got := r.do("DELETE", p+"/members/alice", "alice", "").StatusCode; got != 204 {
		t.Fatalf("alice leaving = %d", got)
	}
	// The audit log records who changed what.
	body := rawBody(r.do("GET", p+"/audit", "bob", "").Body)
	for _, want := range []string{"member.set", "member.remove", `"actor":"alice"`, `"actor":"bob"`} {
		if !strings.Contains(body, want) {
			t.Errorf("audit lacks %s: %s", want, body)
		}
	}
}

func TestTmInvites(t *testing.T) {
	r := newTmFaultRig(t)
	p := "/api/projects/" + r.project
	ctx := context.Background()

	// validation
	r.json("POST", p+"/invites", "alice", `{"email":"nobody"}`, 400)
	r.json("POST", p+"/invites", "alice", `{"email":"a@b.io","role":"boss"}`, 400)
	r.json("POST", p+"/invites", "alice", `{`, 400)

	// an unknown email becomes a pending invite with the default role
	iv := r.json("POST", p+"/invites", "alice", `{"email":"Dave@X.io"}`, 201)
	if iv["email"] != "dave@x.io" || iv["role"] != "member" || iv["invitedBy"] != "alice" {
		t.Fatalf("invite = %v", iv)
	}
	if list := tmList(t, r.rig, p+"/invites", "alice"); len(list) != 1 || list[0]["email"] != "dave@x.io" {
		t.Fatalf("invites = %v", list)
	}
	// the invite is claimed when the invited person first logs in
	if _, err := r.tm.UpsertUser(ctx, store.User{Login: "dave", Email: "dave@x.io"}); err != nil {
		t.Fatal(err)
	}
	if got := r.json("GET", p, "dave", "", 200)["role"]; got != "member" {
		t.Fatalf("dave's role after login = %v", got)
	}
	if list := tmList(t, r.rig, p+"/invites", "alice"); len(list) != 0 {
		t.Fatalf("claimed invite still listed: %v", list)
	}

	// inviting somebody who already has an account adds them at once, with the requested role
	added := r.json("POST", p+"/invites", "alice", `{"email":"erin@x.io","role":"viewer"}`, 201)
	if added["email"] != "erin@x.io" {
		t.Fatalf("unknown erin = %v", added)
	}
	r.tm.UpsertUser(ctx, store.User{Login: "frank", Email: "frank@x.io"})
	if res := r.json("POST", p+"/invites", "alice", `{"email":"frank@x.io","role":"viewer"}`, 200); res["added"] != true {
		t.Fatalf("existing user = %v", res)
	}
	if got := r.json("GET", p, "frank", "", 200)["role"]; got != "viewer" {
		t.Fatalf("frank's role = %v", got)
	}

	// revoking: owner only, and only what exists
	if got := r.do("DELETE", p+"/invites?email=erin@x.io", "alice", "").StatusCode; got != 204 {
		t.Fatalf("revoke = %d", got)
	}
	r.json("DELETE", p+"/invites?email=erin@x.io", "alice", "", 404)
	r.json("DELETE", p+"/invites", "alice", "", 404)
	r.json("DELETE", p+"/invites?email=x@x.io", "bob", "", 403)
}

func TestTmAuditLimitAndOrder(t *testing.T) {
	r := newTmFaultRig(t)
	p := "/api/projects/" + r.project
	for i := range 3 {
		r.json("PATCH", p, "alice", `{"description":"v`+string(rune('0'+i))+`"}`, 200)
	}

	all := tmList(t, r.rig, p+"/audit", "bob")
	two := tmList(t, r.rig, p+"/audit?limit=2", "bob")

	if len(two) != 2 {
		t.Fatalf("limit=2 gave %d rows", len(two))
	}
	if len(all) < 5 || all[0]["action"] != "project.update" {
		t.Fatalf("audit is newest first, got %v", all)
	}
	// Audit entries of other projects never show up.
	other := r.json("POST", "/api/projects", "alice", `{"name":"other"}`, 201)["id"].(string)
	for _, e := range tmList(t, r.rig, "/api/projects/"+other+"/audit", "alice") {
		if e["projectId"] != other {
			t.Fatalf("foreign audit entry %v", e)
		}
	}
}

func TestTmOverviewListsOnlyWhatTheUserCanSee(t *testing.T) {
	r := newTmFaultRig(t)
	shared := r.json("POST", "/api/threads", "alice", `{"projectId":"`+r.project+`","title":"Shared work"}`, 201)["id"].(string)
	mine := r.json("POST", "/api/threads", "alice", `{"title":"Alice private"}`, 201)["id"].(string)
	theirs := r.json("POST", "/api/threads", "dave", `{"title":"Dave private"}`, 201)["id"].(string)

	ov := r.json("GET", "/api/overview", "alice", "", 200)
	bob := r.json("GET", "/api/overview", "bob", "", 200)
	dave := r.json("GET", "/api/overview", "dave", "", 200)

	ids := func(doc map[string]any) (personal, project []string) {
		for _, th := range doc["personal"].([]any) {
			personal = append(personal, th.(map[string]any)["id"].(string))
		}
		for _, p := range doc["projects"].([]any) {
			for _, th := range p.(map[string]any)["threads"].([]any) {
				project = append(project, th.(map[string]any)["id"].(string))
			}
		}
		return
	}
	if pe, pr := ids(ov); len(pe) != 1 || pe[0] != mine || len(pr) != 1 || pr[0] != shared {
		t.Fatalf("alice: personal %v project %v", pe, pr)
	}
	if pe, pr := ids(bob); len(pe) != 0 || len(pr) != 1 || pr[0] != shared {
		t.Fatalf("bob: personal %v project %v", pe, pr)
	}
	if pe, pr := ids(dave); len(pe) != 1 || pe[0] != theirs || len(pr) != 0 {
		t.Fatalf("dave: personal %v project %v", pe, pr)
	}
	proj := ov["projects"].([]any)[0].(map[string]any)
	if proj["name"] != "api" || proj["role"] != "owner" {
		t.Fatalf("project header = %v", proj)
	}
	th := proj["threads"].([]any)[0].(map[string]any)
	if th["status"] != "idle" || th["createdBy"] != "alice" || th["kind"] != "chat" || th["title"] != "Shared work" {
		t.Fatalf("thread row = %v", th)
	}
}
