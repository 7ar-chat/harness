package product

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/core/store"
)

// fakeMailer records the emails the product sends.
type fakeMailer struct {
	mu   sync.Mutex
	sent chan mailed
}

type mailed struct {
	To            []string
	Subject, Body string
}

func newFakeMailer() *fakeMailer { return &fakeMailer{sent: make(chan mailed, 10)} }

func (m *fakeMailer) Send(_ context.Context, to []string, subject, body string) error {
	m.sent <- mailed{to, subject, body}
	return nil
}

func (m *fakeMailer) next(t *testing.T) mailed {
	t.Helper()
	select {
	case e := <-m.sent:
		return e
	case <-time.After(3 * time.Second):
		t.Fatal("no email was sent")
		return mailed{}
	}
}

func (m *fakeMailer) none(t *testing.T) {
	t.Helper()
	select {
	case e := <-m.sent:
		t.Fatalf("unexpected email: %+v", e)
	case <-time.After(150 * time.Millisecond):
	}
}

func seedUsers(t *testing.T, tm *teamstore.Mem, users ...store.User) {
	t.Helper()
	for _, u := range users {
		if _, err := tm.UpsertUser(context.Background(), u); err != nil {
			t.Fatal(err)
		}
	}
}

func signupRig(t *testing.T, tm *teamstore.Mem, mail Mailer) *rig {
	t.Helper()
	return newRig(t, rigOpts{team: true, prod: Options{Team: tm, Admins: []string{"alice"}, Mail: mail, PublicURL: "https://harness.test"}})
}

func errCode(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func TestPendingAndRejectedAccountsCanOnlyAskWhereTheyStand(t *testing.T) {
	// Arrange
	tm := teamstore.NewMem()
	seedUsers(t, tm,
		store.User{Login: "alice", Email: "alice@x.io"},
		store.User{Login: "dave", Email: "dave@x.io", Status: store.UserPending},
		store.User{Login: "erin", Email: "erin@x.io", Status: store.UserRejected})
	r := signupRig(t, tm, nil)

	// Act
	me := r.json("GET", "/api/me", "dave", "", 200)
	pendingThreads := r.json("GET", "/api/threads", "dave", "", 403)
	pendingProject := r.json("POST", "/api/projects", "dave", `{"name":"x"}`, 403)
	rejected := r.json("GET", "/api/threads", "erin", "", 403)
	r.json("GET", "/api/threads", "alice", "", 200)

	// Assert
	if u := me["user"].(map[string]any); u["status"] != "pending" || me["admin"] != false {
		t.Errorf("me = %v", me)
	}
	if errCode(pendingThreads) != "pending_approval" || errCode(pendingProject) != "pending_approval" {
		t.Errorf("pending answers: %v %v", pendingThreads, pendingProject)
	}
	if errCode(rejected) != "account_rejected" {
		t.Errorf("rejected answer: %v", rejected)
	}
}

func TestAdminsApproveAndRejectAccounts(t *testing.T) {
	// Arrange
	tm := teamstore.NewMem()
	seedUsers(t, tm,
		store.User{Login: "alice", Email: "alice@x.io"},
		store.User{Login: "bob", Email: "bob@x.io"},
		store.User{Login: "dave", Email: "dave@x.io", Status: store.UserPending})
	r := signupRig(t, tm, nil)

	// Act and assert: only admins use the panel.
	if me := r.json("GET", "/api/me", "alice", "", 200); me["admin"] != true {
		t.Errorf("alice is not shown as admin: %v", me)
	}
	r.json("GET", "/api/admin/users", "bob", "", 403)
	r.json("POST", "/api/admin/users/dave/approve", "bob", "", 403)

	resp := r.do("GET", "/api/admin/users?status=pending", "alice", "")
	var pending []adminUserView
	_ = json.NewDecoder(resp.Body).Decode(&pending)
	if len(pending) != 1 || pending[0].Login != "dave" || pending[0].Status != "pending" {
		t.Fatalf("pending = %+v", pending)
	}

	// Approving lets dave in at once.
	if got := r.json("POST", "/api/admin/users/dave/approve", "alice", "", 200); got["status"] != "approved" {
		t.Errorf("approve answered %v", got)
	}
	r.json("GET", "/api/threads", "dave", "", 200)

	// Rejecting shuts him out again, and the panel counts him as rejected.
	r.json("POST", "/api/admin/users/dave/reject", "alice", "", 200)
	if c := errCode(r.json("GET", "/api/threads", "dave", "", 403)); c != "account_rejected" {
		t.Errorf("after reject: %q", c)
	}
	resp = r.do("GET", "/api/admin/users?status=rejected", "alice", "")
	var rejected []adminUserView
	_ = json.NewDecoder(resp.Body).Decode(&rejected)
	if len(rejected) != 1 || rejected[0].Login != "dave" {
		t.Errorf("rejected = %+v", rejected)
	}

	// An admin cannot be rejected, an unknown account is not found.
	r.json("POST", "/api/admin/users/alice/reject", "alice", "", 400)
	r.json("POST", "/api/admin/users/nobody/approve", "alice", "", 404)
}

// fakeGHLogin is the part of GitHub the sign-in needs: the token endpoint and the account of the token.
type fakeGHLogin struct {
	srv    *httptest.Server
	login  string
	name   string
	avatar string
	emails []map[string]any
}

func newFakeGHLogin(t *testing.T) *fakeGHLogin {
	t.Helper()
	g := &fakeGHLogin{login: "Dave", name: "Dave D", avatar: "https://avatars.test/dave.png",
		emails: []map[string]any{{"email": "Dave@X.io", "primary": true, "verified": true}}}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("code") != "good" || r.Form.Get("code_verifier") == "" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"error":"bad_verification_code"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"gh-token","token_type":"bearer"}`)
	})
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer gh-token" {
				http.Error(w, "no", 401)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("/user", auth(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"login": g.login, "name": g.name, "avatar_url": g.avatar})
	}))
	mux.HandleFunc("/user/emails", auth(func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(g.emails) }))
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

// ghLoginRig serves the product with the GitHub sign-in against the fake GitHub; requests carry the session cookie.
type ghLoginRig struct {
	*rig
	gh *fakeGHLogin
}

func newGHLoginRig(t *testing.T, tm *teamstore.Mem, mail Mailer) *ghLoginRig {
	t.Helper()
	gh := newFakeGHLogin(t)
	cfg := &GitHubLoginConfig{ClientID: "cid", ClientSecret: "csecret", SessionKey: []byte("0123456789abcdef0123456789abcdef"),
		authURL: gh.srv.URL + "/authorize", tokenURL: gh.srv.URL + "/token", apiURL: gh.srv.URL}
	r := newRig(t, rigOpts{team: true, rawAuth: true, prod: Options{Team: tm, Admins: []string{"alice", "root@x.io"}, Mail: mail,
		PublicURL: "https://harness.test", GitHubLogin: cfg}})
	return &ghLoginRig{rig: r, gh: gh}
}

// signIn runs the flow and returns the session cookie ("" when none was given).
func (g *ghLoginRig) signIn(code string) (cookie string, status int) {
	g.t.Helper()
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(g.srv.URL + "/auth/signin/github?next=/%23/profile")
	if err != nil {
		g.t.Fatal(err)
	}
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	q := loc.Query()
	if resp.StatusCode != http.StatusFound || !strings.HasSuffix(loc.Path, "/authorize") || q.Get("client_id") != "cid" ||
		q.Get("code_challenge_method") != "S256" || !strings.Contains(q.Get("scope"), "user:email") ||
		q.Get("redirect_uri") != "https://harness.test/auth/signin/github/callback" {
		g.t.Fatalf("authorize redirect = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	req, _ := http.NewRequest("GET", g.srv.URL+"/auth/signin/github/callback?code="+code+"&state="+url.QueryEscape(q.Get("state")), nil)
	for _, c := range resp.Cookies() {
		req.AddCookie(c)
	}
	back, err := noFollow.Do(req)
	if err != nil {
		g.t.Fatal(err)
	}
	back.Body.Close()
	for _, c := range back.Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			cookie = c.Name + "=" + c.Value
		}
	}
	if back.StatusCode == http.StatusFound && back.Header.Get("Location") != "/#/profile" {
		g.t.Errorf("redirected to %q", back.Header.Get("Location"))
	}
	return cookie, back.StatusCode
}

// as sends a request with the session cookie.
func (g *ghLoginRig) as(cookie, method, path string) (int, map[string]any) {
	g.t.Helper()
	req, _ := http.NewRequest(method, g.srv.URL+path, nil)
	req.Header.Set("Cookie", cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		g.t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &m)
	return resp.StatusCode, m
}

func TestGitHubSignUpWaitsForApprovalAndTellsTheAdmins(t *testing.T) {
	// Arrange: alice is an admin with an email, root@x.io is an admin known by email only.
	tm := teamstore.NewMem()
	seedUsers(t, tm, store.User{Login: "alice", Email: "alice@x.io"})
	mail := newFakeMailer()
	r := newGHLoginRig(t, tm, mail)

	// Act
	cookie, status := r.signIn("good")

	// Assert: a session, but pending.
	if cookie == "" || status != http.StatusFound {
		t.Fatalf("sign-in = %d, cookie %q", status, cookie)
	}
	u, err := tm.GetUser(context.Background(), "github_dave")
	if err != nil || u.Status != store.UserPending || u.Email != "dave@x.io" || u.Name != "Dave D" || u.AvatarURL == "" {
		t.Fatalf("user = %+v, %v", u, err)
	}
	if code, me := r.as(cookie, "GET", "/api/me"); code != 200 || me["user"].(map[string]any)["status"] != "pending" {
		t.Errorf("me = %d %v", code, me)
	}
	if code, body := r.as(cookie, "GET", "/api/threads"); code != 403 || errCode(body) != "pending_approval" {
		t.Errorf("threads = %d %v", code, body)
	}

	// The admins are emailed: the address in the list, and the email of the admin named by login.
	e := mail.next(t)
	if strings.Join(e.To, ",") != "root@x.io,alice@x.io" && strings.Join(e.To, ",") != "alice@x.io,root@x.io" {
		t.Errorf("recipients = %v", e.To)
	}
	for _, want := range []string{"Dave D", "github_dave", "dave@x.io", "https://harness.test/#/admin"} {
		if !strings.Contains(e.Body, want) {
			t.Errorf("body lacks %q:\n%s", want, e.Body)
		}
	}
	if !strings.Contains(e.Subject, "Dave D") {
		t.Errorf("subject = %q", e.Subject)
	}

	// Signing in again neither emails nor changes the status; approval lets the same session through.
	if _, _ = r.signIn("good"); true {
		mail.none(t)
	}
	r.json("POST", "/api/admin/users/github_dave/approve", "", "", 401) // the rig's X-User is off: sessions only
	req, _ := http.NewRequest("POST", r.srv.URL+"/api/admin/users/github_dave/approve", nil)
	req.Header.Set("Cookie", mustSession(t, r, "alice"))
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("approve = %d", resp.StatusCode)
	}
	if code, _ := r.as(cookie, "GET", "/api/threads"); code != 200 {
		t.Errorf("threads after approval = %d", code)
	}
}

// mustSession returns a session cookie for login, signed with the server's key (the GitHub rig's).
func mustSession(t *testing.T, r *ghLoginRig, login string) string {
	t.Helper()
	s := newSessions([]byte("0123456789abcdef0123456789abcdef"), time.Hour, false)
	rec := httptest.NewRecorder()
	s.start(rec, login)
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c.Name + "=" + c.Value
		}
	}
	t.Fatal("no cookie")
	return ""
}

func TestGitHubSignInOfARejectedAccountStaysRejected(t *testing.T) {
	// Arrange
	tm := teamstore.NewMem()
	seedUsers(t, tm, store.User{Login: "github_dave", Email: "dave@x.io", Status: store.UserRejected})
	r := newGHLoginRig(t, tm, newFakeMailer())

	// Act
	cookie, _ := r.signIn("good")

	// Assert
	if code, body := r.as(cookie, "GET", "/api/threads"); code != 403 || errCode(body) != "account_rejected" {
		t.Errorf("threads = %d %v", code, body)
	}
}

func TestGitHubSignInJoinsTheAccountThatOwnsTheVerifiedEmail(t *testing.T) {
	// Arrange: ghazi already uses the product through the identity provider.
	tm := teamstore.NewMem()
	seedUsers(t, tm, store.User{Login: "ghazi", Email: "dave@x.io", Name: "Ghazi B"})
	mail := newFakeMailer()
	r := newGHLoginRig(t, tm, mail)

	// Act
	cookie, _ := r.signIn("good")

	// Assert: same account, approved, nobody emailed, no second account.
	code, me := r.as(cookie, "GET", "/api/me")
	if u := me["user"].(map[string]any); code != 200 || u["login"] != "ghazi" || u["name"] != "Ghazi B" || u["avatarUrl"] != "https://avatars.test/dave.png" {
		t.Errorf("me = %d %v", code, me)
	}
	if code, _ := r.as(cookie, "GET", "/api/threads"); code != 200 {
		t.Errorf("threads = %d", code)
	}
	if _, err := tm.GetUser(context.Background(), "github_dave"); err == nil {
		t.Error("a second account was created")
	}
	mail.none(t)
}

func TestGitHubSignInIgnoresAnUnverifiedEmailAndAdminsAreAlwaysApproved(t *testing.T) {
	// Arrange: the GitHub account claims alice's address without having verified it.
	tm := teamstore.NewMem()
	seedUsers(t, tm, store.User{Login: "alice", Email: "alice@x.io"})
	r := newGHLoginRig(t, tm, newFakeMailer())
	r.gh.emails = []map[string]any{{"email": "alice@x.io", "primary": true, "verified": false}}
	r.gh.login = "mallory"

	// Act
	cookie, _ := r.signIn("good")

	// Assert: a separate, pending account that is not alice.
	u, err := tm.GetUser(context.Background(), "github_mallory")
	if err != nil || u.Status != store.UserPending || u.Email != "" {
		t.Fatalf("user = %+v, %v", u, err)
	}
	if code, me := r.as(cookie, "GET", "/api/me"); code != 200 || me["user"].(map[string]any)["login"] != "github_mallory" || me["admin"] != false {
		t.Errorf("me = %d %v", code, me)
	}

	// An admin who signs up with GitHub is approved on the spot (root@x.io is an admin by email).
	r.gh.login, r.gh.emails = "root", []map[string]any{{"email": "root@x.io", "primary": true, "verified": true}}
	cookie, _ = r.signIn("good")
	if code, me := r.as(cookie, "GET", "/api/me"); code != 200 || me["admin"] != true || me["user"].(map[string]any)["status"] != "approved" {
		t.Errorf("admin me = %d %v", code, me)
	}
}

func TestGitHubSignInFailuresGiveNoSession(t *testing.T) {
	tm := teamstore.NewMem()
	r := newGHLoginRig(t, tm, nil)

	if cookie, status := r.signIn("bad"); cookie != "" || status == http.StatusFound {
		t.Errorf("bad code: %d %q", status, cookie)
	}
	// No flow cookie, wrong state.
	resp, _ := http.Get(r.srv.URL + "/auth/signin/github/callback?code=good&state=x")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("callback without a flow = %d", resp.StatusCode)
	}
	if users, _ := tm.ListUsers(context.Background()); len(users) != 0 {
		t.Errorf("users = %+v", users)
	}
}

func TestProvidersListsTheConfiguredLogins(t *testing.T) {
	// GitHub only.
	r := newGHLoginRig(t, teamstore.NewMem(), nil)
	resp, err := http.Get(r.srv.URL + "/auth/providers")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct {
		Providers []struct{ ID, Name, URL string }
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if len(got.Providers) != 1 || got.Providers[0].ID != "github" || got.Providers[0].URL != "/auth/signin/github" {
		t.Errorf("providers = %+v", got)
	}
	// Logging out works without OIDC.
	out, _ := http.Post(r.srv.URL+"/auth/logout", "", nil)
	out.Body.Close()
	if out.StatusCode != http.StatusNoContent {
		t.Errorf("logout = %d", out.StatusCode)
	}
}

func TestOIDCAndGitHubSignInRunSideBySide(t *testing.T) {
	// Arrange: one server with both logins.
	idp := newFakeIdP(t, map[string]any{"sub": "u-1", "preferred_username": "ghazi", "email": "ghazi@x.io", "email_verified": true})
	tm := teamstore.NewMem()
	gh := newFakeGHLogin(t)
	key := []byte("0123456789abcdef0123456789abcdef")
	r := newRig(t, rigOpts{team: true, rawAuth: true, prod: Options{Team: tm, PublicURL: "https://harness.test",
		OIDC:        &OIDCConfig{Issuer: idp.srv.URL, ClientID: "cid", ClientSecret: "s", RedirectURL: "https://harness.test/auth/callback", SessionKey: key},
		GitHubLogin: &GitHubLoginConfig{ClientID: "cid", ClientSecret: "s", SessionKey: key, authURL: gh.srv.URL + "/authorize", tokenURL: gh.srv.URL + "/token", apiURL: gh.srv.URL}}})
	g := &ghLoginRig{rig: r, gh: gh}

	// Act
	resp, err := http.Get(r.srv.URL + "/auth/providers")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct{ Providers []struct{ ID string } }
	_ = json.NewDecoder(resp.Body).Decode(&got)
	cookie, _ := g.signIn("good")

	// Assert: both are offered, the GitHub session is valid and pending, and OIDC still redirects to its provider.
	if len(got.Providers) != 2 || got.Providers[0].ID != "oidc" || got.Providers[1].ID != "github" {
		t.Errorf("providers = %+v", got)
	}
	if code, me := g.as(cookie, "GET", "/api/me"); code != 200 || me["user"].(map[string]any)["status"] != "pending" {
		t.Errorf("github session: %d %v", code, me)
	}
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	o, _ := noFollow.Get(r.srv.URL + "/auth/login")
	o.Body.Close()
	if o.StatusCode != http.StatusFound || !strings.HasPrefix(o.Header.Get("Location"), idp.srv.URL+"/authorize") {
		t.Errorf("oidc login = %d %s", o.StatusCode, o.Header.Get("Location"))
	}
}
