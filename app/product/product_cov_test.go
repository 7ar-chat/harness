package product

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/hosts/client"
	"github.com/7ar-chat/harness/hosts/httpapi"
)

// tmServer is a product server wired by hand over the harness of r, to call the policy hooks directly.
func tmServer(r *rig, team store.TeamStore) *server {
	return &server{Server: &httpapi.Server{H: r.h}, team: team, hub: newHub()}
}

func TestTmOIDCBecomesTheAuthenticatorUnlessOneIsGiven(t *testing.T) {
	// Arrange
	idp := newFakeIdP(t, map[string]any{"sub": "u-1", "preferred_username": "alice", "email": "alice@x.io", "email_verified": true})
	cfg := &OIDCConfig{Issuer: idp.srv.URL, ClientID: "cid", ClientSecret: "s", RedirectURL: "http://app.test/auth/callback", SessionKey: []byte("0123456789abcdef0123456789abcdef")}

	// Act: no other authenticator
	r := newRig(t, rigOpts{team: true, rawAuth: true, prod: Options{OIDC: cfg}})
	anon := r.do("GET", "/api/threads", "", "")
	spoof := r.do("GET", "/api/threads", "alice", "") // the X-User header means nothing here
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("GET", r.srv.URL+"/auth/login?next=/x", nil)
	login, err := noFollow.Do(req)

	// Assert: requests need the session cookie, and the login route is mounted
	if anon.StatusCode != 401 || spoof.StatusCode != 401 {
		t.Fatalf("anonymous = %d, spoofed header = %d; want 401", anon.StatusCode, spoof.StatusCode)
	}
	if err != nil || login.StatusCode != http.StatusFound || !strings.HasPrefix(login.Header.Get("Location"), idp.srv.URL+"/authorize") {
		t.Fatalf("login = %v, %v", login, err)
	}

	// An authenticator given by the host wins over the OIDC session.
	custom := newRig(t, rigOpts{team: true, prod: Options{OIDC: cfg}}) // newRig installs the X-User authenticator
	if got := custom.do("GET", "/api/threads", "alice", "").StatusCode; got != 200 {
		t.Fatalf("host authenticator ignored: %d", got)
	}
}

func TestTmSetupFailsWhenTheProviderIsUnreachableOrTeamModeIsOff(t *testing.T) {
	// Arrange: a harness to hang the server on
	r := newRig(t, rigOpts{})
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()

	for name, o := range map[string]Options{
		"unreachable issuer": {Team: teamstore.NewMem(), OIDC: &OIDCConfig{Issuer: dead.URL, ClientID: "c"}},
		"OIDC without team":  {OIDC: &OIDCConfig{Issuer: dead.URL, ClientID: "c"}},
	} {
		// Act
		h, err := New(r.h, httpapi.Options{}, o)

		// Assert
		if err == nil || h != nil {
			t.Errorf("%s: New = %v, %v; want an error", name, h, err)
		}
	}
}

func TestTmAuthorIsTheActorOnlyInTeamMode(t *testing.T) {
	team := &server{team: teamstore.NewMem()}
	solo := &server{}

	if got := team.Author(Caller{Login: "owner", Actor: "bob"}); got.Login != "bob" || got.Kind != store.AuthorUser {
		t.Fatalf("team author = %+v: authorship follows the actor, not the thread owner", got)
	}
	if got := team.Author(Caller{Login: "owner"}); got != (harness.Author{}) {
		t.Fatalf("no actor: %+v", got)
	}
	if got := solo.Author(Caller{Login: "alice", Actor: "alice"}); got != (harness.Author{}) {
		t.Fatalf("solo author = %+v", got)
	}
}

func TestTmAfterRunActionRecordsCancelAndSteerOfExistingRunsOnly(t *testing.T) {
	// Arrange
	r := newTmFaultRig(t)
	th := r.projectThread("alice", "t")
	run, err := r.h.Send(context.Background(), "alice", th, "hello", harness.RunOptions{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	s := tmServer(r.rig, r.tm)
	sub, done := s.hub.subscribe(nil)
	defer done()
	req := func(id string) *http.Request {
		q := httptest.NewRequest("POST", "/api/runs/x/cancel", nil)
		q.SetPathValue("id", id)
		return q
	}
	c := Caller{Login: "alice", Actor: "bob", ProjectID: r.project}

	// Act
	s.AfterRunAction(req("missing-run"), c, "run.cancel")
	select {
	case ev := <-sub.ch:
		t.Fatalf("an unknown run produced %+v", ev)
	default:
	}
	s.AfterRunAction(req(run.ID), c, "run.cancel")

	// Assert
	ev := <-sub.ch
	if ev.Type != "message.sent" || ev.RunID != run.ID || ev.ThreadID != th || ev.Actor != "bob" || ev.ProjectID != r.project {
		t.Fatalf("event = %+v", ev)
	}
	log, _ := r.tm.ListAudit(context.Background(), r.project, 10)
	if len(log) == 0 || log[0].Action != "run.cancel" || log[0].Actor != "bob" || log[0].ThreadID != th {
		t.Fatalf("audit = %+v", log)
	}
	(&server{}).AfterRunAction(req(run.ID), c, "run.cancel") // solo mode: nothing to record, and no panic on a nil store
}

func TestTmAfterForkLinksTheForkToTheActorsProject(t *testing.T) {
	// Arrange
	r := newTmFaultRig(t)
	ctx := context.Background()
	src := r.projectThread("alice", "src")
	fork, err := r.h.Store.CreateThread(ctx, "alice", "fork")
	if err != nil {
		t.Fatal(err)
	}
	s := tmServer(r.rig, r.fault)
	req := httptest.NewRequest("POST", "/api/threads/x/fork", nil)
	req.SetPathValue("id", src)
	th := &client.Thread{ID: fork.ID, Title: "fork"}

	// Act
	failed := s.AfterFork(httptest.NewRecorder(), req, Caller{Login: "alice", Actor: "bob", ProjectID: r.project}, th)

	// Assert
	meta, err := r.tm.GetThreadMeta(ctx, fork.ID)
	if failed || err != nil || meta.ProjectID != r.project || meta.Owner != "alice" || meta.CreatedBy != "bob" || meta.Kind != store.ThreadChat {
		t.Fatalf("failed=%v meta=%+v err=%v", failed, meta, err)
	}
	log, _ := r.tm.ListAudit(ctx, r.project, 10)
	if len(log) == 0 || log[0].Action != "thread.fork" || log[0].Detail["from"] != src {
		t.Fatalf("audit = %+v", log)
	}

	// A fork that cannot be linked is removed again and answers the error itself.
	orphan, _ := r.h.Store.CreateThread(ctx, "alice", "orphan")
	r.fault.arm("SetThreadMeta")
	w := httptest.NewRecorder()
	failed = s.AfterFork(w, req, Caller{Login: "alice", Actor: "bob", ProjectID: r.project}, &client.Thread{ID: orphan.ID})
	r.fault.disarm("SetThreadMeta")
	if !failed || w.Code != 500 || strings.Contains(w.Body.String(), tmSecret) {
		t.Fatalf("failed=%v status=%d body=%s", failed, w.Code, w.Body)
	}
	if _, err := r.h.Store.GetThread(ctx, "alice", orphan.ID); err == nil {
		t.Fatal("the unlinked fork was left in the store")
	}

	// Without team mode the plain API answers.
	if (&server{}).AfterFork(httptest.NewRecorder(), req, Caller{}, th) {
		t.Fatal("solo AfterFork claimed to have answered")
	}
}

func TestTmBeforeResumeNamesThePendingToolAndForcesTheSupervisorInteractive(t *testing.T) {
	// Arrange
	r := newTmFaultRig(t)
	ctx := context.Background()
	th := r.projectThread("alice", "t")
	s := tmServer(r.rig, r.tm)
	req := httptest.NewRequest("POST", "/api/threads/x/resume", nil)
	c := Caller{Login: "alice", Actor: "alice"}

	// Act / Assert: nothing pending → no tool name, body untouched
	body := client.ResumeRequest{}
	if got := s.BeforeResume(req, c, th, &body); got != "" || body.Interactive {
		t.Fatalf("tool = %q interactive = %v", got, body.Interactive)
	}
	if got := s.BeforeResume(req, c, "unknown-thread", &body); got != "" {
		t.Fatalf("unknown thread tool = %q", got)
	}
	// Solo mode never looks anything up.
	if got := (&server{}).BeforeResume(req, c, th, &body); got != "" {
		t.Fatalf("solo tool = %q", got)
	}
	// The supervisor thread's approvals are interactive whatever the client asked.
	s.sup = NewSupervisor(r.tm)
	sup, err := s.supervisorThread(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	body = client.ResumeRequest{}
	s.BeforeResume(req, c, sup, &body)
	if !body.Interactive {
		t.Fatal("supervisor approvals must be interactive")
	}
	body = client.ResumeRequest{}
	s.BeforeResume(req, c, th, &body)
	if body.Interactive {
		t.Fatal("an ordinary thread was forced interactive")
	}
}

func TestTmBeforeRunForcesSandboxOffOnlyForTheSupervisorThread(t *testing.T) {
	r := newTmFaultRig(t)
	s := tmServer(r.rig, r.tm)
	s.sup = NewSupervisor(r.tm)
	sup, err := s.supervisorThread(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	th := r.projectThread("alice", "t")
	req := httptest.NewRequest("POST", "/", nil)

	var forced, ordinary client.SendOptions
	s.BeforeRun(req, sup, &forced)
	s.BeforeRun(req, th, &ordinary)

	if forced.Sandbox != "off" || !forced.Interactive {
		t.Fatalf("supervisor options = %+v", forced)
	}
	if ordinary.Sandbox != "" || ordinary.Interactive {
		t.Fatalf("ordinary options = %+v", ordinary)
	}
}

func TestTmWireTypesCoverEveryViewTheRoutesSend(t *testing.T) {
	wire := WireTypes()
	for _, name := range []string{"User", "ProjectWithRole", "Member", "Thread", "ThreadSummary", "Overview", "OverviewProject", "Connector",
		"SupervisorSettings", "FeedEvent", "MemoryEntry", "MemoryList", "MemoryStats", "MemoryEmbeddings"} {
		if wire[name] == nil {
			t.Errorf("WireTypes lacks %s", name)
		}
	}
	for name, v := range wire {
		if v == nil {
			t.Errorf("%s is nil: the TypeScript generator reflects over it", name)
		}
	}
}
