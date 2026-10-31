package product

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/core/store"
)

var cnKey = []byte("0123456789abcdef0123456789abcdef")

// cnFaultTeam is a TeamStore whose named operations can be made to fail.
type cnFaultTeam struct {
	store.TeamStore
	mu   sync.Mutex
	errs map[string]error
}

func newCnFaultTeam() *cnFaultTeam {
	return &cnFaultTeam{TeamStore: teamstore.NewMem(), errs: map[string]error{}}
}

func (f *cnFaultTeam) failOn(op string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.errs, op)
		return
	}
	f.errs[op] = err
}

func (f *cnFaultTeam) err(op string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.errs[op]
}

var errCnBoom = errors.New("store down")

func (f *cnFaultTeam) ListConnectors(ctx context.Context, login string) ([]store.Connector, error) {
	if err := f.err("ListConnectors"); err != nil {
		return nil, err
	}
	return f.TeamStore.ListConnectors(ctx, login)
}

func (f *cnFaultTeam) SetConnector(ctx context.Context, c store.Connector) error {
	if err := f.err("SetConnector"); err != nil {
		return err
	}
	return f.TeamStore.SetConnector(ctx, c)
}

func (f *cnFaultTeam) DeleteConnector(ctx context.Context, login, kind string) error {
	if err := f.err("DeleteConnector"); err != nil {
		return err
	}
	return f.TeamStore.DeleteConnector(ctx, login, kind)
}

func (f *cnFaultTeam) CreateProject(ctx context.Context, p store.Project, owner string) (*store.Project, error) {
	if err := f.err("CreateProject"); err != nil {
		return nil, err
	}
	return f.TeamStore.CreateProject(ctx, p, owner)
}

func (f *cnFaultTeam) UpdateProject(ctx context.Context, id string, patch store.ProjectPatch) (*store.Project, error) {
	if err := f.err("UpdateProject"); err != nil {
		return nil, err
	}
	return f.TeamStore.UpdateProject(ctx, id, patch)
}

func (f *cnFaultTeam) ListProjects(ctx context.Context, login string) ([]store.ProjectRole, error) {
	if err := f.err("ListProjects"); err != nil {
		return nil, err
	}
	return f.TeamStore.ListProjects(ctx, login)
}

func (f *cnFaultTeam) SetThreadMeta(ctx context.Context, m store.ThreadMeta) error {
	if err := f.err("SetThreadMeta"); err != nil {
		return err
	}
	return f.TeamStore.SetThreadMeta(ctx, m)
}

func (f *cnFaultTeam) SeenThreads(ctx context.Context, login string) (map[string]time.Time, error) {
	if err := f.err("SeenThreads"); err != nil {
		return nil, err
	}
	return f.TeamStore.SeenThreads(ctx, login)
}

func (f *cnFaultTeam) SetThreadSeen(ctx context.Context, login, threadID string, at time.Time) error {
	if err := f.err("SetThreadSeen"); err != nil {
		return err
	}
	return f.TeamStore.SetThreadSeen(ctx, login, threadID, at)
}

func (f *cnFaultTeam) UpsertUser(ctx context.Context, u store.User) (*store.User, error) {
	if err := f.err("UpsertUser"); err != nil {
		return nil, err
	}
	return f.TeamStore.UpsertUser(ctx, u)
}

func (f *cnFaultTeam) SetDisplayName(ctx context.Context, login, name string) error {
	if err := f.err("SetDisplayName"); err != nil {
		return err
	}
	return f.TeamStore.SetDisplayName(ctx, login, name)
}

func (f *cnFaultTeam) GetUser(ctx context.Context, login string) (*store.User, error) {
	if err := f.err("GetUser"); err != nil {
		return nil, err
	}
	return f.TeamStore.GetUser(ctx, login)
}

func (f *cnFaultTeam) GetAvatar(ctx context.Context, login string) (string, []byte, error) {
	if err := f.err("GetAvatar"); err != nil {
		return "", nil, err
	}
	return f.TeamStore.GetAvatar(ctx, login)
}

func cnUsers(t *testing.T, tm store.TeamStore, logins ...string) {
	t.Helper()
	for _, l := range logins {
		if _, err := tm.UpsertUser(context.Background(), store.User{Login: l, Name: strings.ToUpper(l[:1]) + l[1:]}); err != nil {
			t.Fatal(err)
		}
	}
}

// cnNoFollow is a client that hands redirects back instead of following them.
var cnNoFollow = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// cnGet performs a GET as user without following redirects.
func cnGet(t *testing.T, r *rig, path, user string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", r.srv.URL+path, nil)
	if user != "" {
		req.Header.Set("X-User", user)
	}
	resp, err := cnNoFollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// cnForgeState is an OAuth state the server would accept (same key), for the given login and expiry.
func cnForgeState(t *testing.T, login string, exp time.Time) string {
	t.Helper()
	box, err := newSecretBox(cnKey)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(oauthState{login, exp.Unix()})
	return url.QueryEscape(encodeState(box.seal(b)))
}

// cnGH is a GitHub (API and OAuth) whose behaviour the test steers.
type cnGH struct {
	srv *httptest.Server

	mu            sync.Mutex
	tokenForms    []url.Values
	installAuth   []string // Authorization of /user/installations calls
	refreshFails  bool
	installStatus int // /user/installations status (0 = 200)
	repoStatus    map[int]int
	repos         map[int][]map[string]any
	installations []int
	appInstall    int // /repos/acme/api/installation id (0 = 404)
	appStatus     int
	fileStatus    int
	fileBody      string
	fileRefs      []string
	tokenStatus   int
	tokenBodies   []map[string]any
}

func newCnGH(t *testing.T) *cnGH {
	g := &cnGH{repoStatus: map[int]int{}, repos: map[int][]map[string]any{}, installations: []int{11}, appInstall: 77}
	g.repos[11] = []map[string]any{
		{"full_name": "acme/web", "private": false, "description": "Website", "default_branch": "trunk"},
		{"full_name": "acme/api", "private": true, "description": "The API", "default_branch": "main"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		g.mu.Lock()
		g.tokenForms = append(g.tokenForms, r.PostForm)
		fails := g.refreshFails
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Form.Get("grant_type") == "refresh_token" {
			if fails {
				json.NewEncoder(w).Encode(map[string]any{"error": "bad_refresh_token"})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-new", "refresh_token": "ref-new", "expires_in": 28800})
			return
		}
		switch code := r.Form.Get("code"); code {
		case "bad":
			json.NewEncoder(w).Encode(map[string]any{"error": "bad_verification_code"})
		default:
			json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-" + code})
		}
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") {
		case "tok-alice", "tok-new", "tok-old":
			json.NewEncoder(w).Encode(map[string]any{"login": "AliceGH"})
		default:
			http.Error(w, "no", http.StatusUnauthorized)
		}
	})
	mux.HandleFunc("/user/installations", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.installAuth = append(g.installAuth, r.Header.Get("Authorization"))
		status, ids := g.installStatus, append([]int(nil), g.installations...)
		g.mu.Unlock()
		if status != 0 {
			http.Error(w, "boom", status)
			return
		}
		var ins []map[string]any
		for _, id := range ids {
			ins = append(ins, map[string]any{"id": id})
		}
		json.NewEncoder(w).Encode(map[string]any{"installations": ins})
	})
	mux.HandleFunc("/user/installations/{id}/repositories", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.Atoi(r.PathValue("id"))
		g.mu.Lock()
		status, repos := g.repoStatus[id], g.repos[id]
		g.mu.Unlock()
		if status != 0 {
			http.Error(w, "boom", status)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"repositories": repos})
	})
	mux.HandleFunc("/repos/acme/api/installation", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		id, status := g.appInstall, g.appStatus
		g.mu.Unlock()
		switch {
		case status != 0:
			http.Error(w, "boom", status)
		case id == 0:
			http.NotFound(w, r)
		default:
			json.NewEncoder(w).Encode(map[string]any{"id": id})
		}
	})
	mux.HandleFunc("/app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		g.mu.Lock()
		g.tokenBodies = append(g.tokenBodies, body)
		status := g.tokenStatus
		g.mu.Unlock()
		if status != 0 {
			http.Error(w, "boom", status)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"token": "ghs_" + r.PathValue("id"), "expires_at": time.Now().Add(time.Hour)})
	})
	mux.HandleFunc("/repos/acme/api/contents/{path...}", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.fileRefs = append(g.fileRefs, r.URL.RawQuery)
		status, body := g.fileStatus, g.fileBody
		g.mu.Unlock()
		if status != 0 {
			http.Error(w, "x", status)
			return
		}
		w.Write([]byte(body))
	})
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

func (g *cnGH) set(f func(g *cnGH)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f(g)
}

func (g *cnGH) forms() []url.Values {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]url.Values(nil), g.tokenForms...)
}

func (g *cnGH) auths() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.installAuth...)
}

// cnGitHubRig is a team rig with the GitHub App pointed at a cnGH. mutate tweaks the app before the server starts.
func cnGitHubRig(t *testing.T, mutate func(*GitHubApp)) (*rig, *cnGH, *cnFaultTeam) {
	t.Helper()
	gh := newCnGH(t)
	app, _ := testApp(t, gh.srv.URL)
	app.WebBase, app.ClientID, app.ClientSecret, app.Slug = gh.srv.URL, "cid", "csecret", "my-app"
	if mutate != nil {
		mutate(app)
	}
	tm := newCnFaultTeam()
	cnUsers(t, tm, "alice", "bob")
	r := newRig(t, rigOpts{team: true, prod: Options{Team: tm, GitHub: app, SecretKey: cnKey, PublicURL: "https://harness.test/"}})
	return r, gh, tm
}

// cnSeedGitHub stores a GitHub connector for login holding the given tokens, sealed like the server does.
func cnSeedGitHub(t *testing.T, tm store.TeamStore, login string, tok githubTokens) {
	t.Helper()
	box, _ := newSecretBox(cnKey)
	data, _ := json.Marshal(tok)
	if err := tm.SetConnector(context.Background(), store.Connector{Login: login, Kind: store.ConnectorGitHub, Account: strings.ToLower(login) + "gh", Label: "@" + login, Data: box.seal(data)}); err != nil {
		t.Fatal(err)
	}
}

// cnStoredTokens decrypts the GitHub tokens kept for login.
func cnStoredTokens(t *testing.T, tm store.TeamStore, login string) githubTokens {
	t.Helper()
	cn, err := tm.GetConnector(context.Background(), login, store.ConnectorGitHub)
	if err != nil {
		t.Fatal(err)
	}
	box, _ := newSecretBox(cnKey)
	plain, err := box.open(cn.Data)
	if err != nil {
		t.Fatal(err)
	}
	var tok githubTokens
	if err := json.Unmarshal(plain, &tok); err != nil {
		t.Fatal(err)
	}
	return tok
}

func cnErrCode(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func TestConnectorsExplainWhatIsMissing(t *testing.T) {
	// Arrange: a server with a GitHub App but no secret key, a server with a key but no GitHub / Telegram.
	gh := newCnGH(t)
	app, _ := testApp(t, gh.srv.URL)
	app.ClientID, app.ClientSecret = "cid", "csecret"
	tm := newCnFaultTeam()
	cnUsers(t, tm, "alice")
	noKey := newRig(t, rigOpts{team: true, prod: Options{Team: tm, GitHub: app}})
	tm2 := newCnFaultTeam()
	cnUsers(t, tm2, "alice")
	noApp := newRig(t, rigOpts{team: true, prod: Options{Team: tm2, SecretKey: cnKey}})
	tm3 := newCnFaultTeam()
	cnUsers(t, tm3, "alice")
	noOAuth := newRig(t, rigOpts{team: true, prod: Options{Team: tm3, SecretKey: cnKey, GitHub: &GitHubApp{Slug: "x"}}})
	view := func(r *rig) map[string]map[string]any {
		out := map[string]map[string]any{}
		resp := r.do("GET", "/api/connectors", "alice", "")
		var list []map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
			t.Fatal(err)
		}
		for _, c := range list {
			out[c["kind"].(string)] = c
		}
		return out
	}

	// Act
	k, a, o := view(noKey), view(noApp), view(noOAuth)

	// Assert: each says why it is unavailable and offers no connect link.
	for name, c := range map[string]map[string]any{"github without key": k["github"], "telegram without key": k["telegram"]} {
		if c["configured"] != false || !strings.Contains(c["message"].(string), "HARNESS_SECRET_KEY") || c["connectUrl"] != nil {
			t.Errorf("%s = %v", name, c)
		}
	}
	for name, c := range map[string]map[string]any{"no app": a["github"], "app without OAuth": o["github"]} {
		if c["configured"] != false || !strings.Contains(c["message"].(string), "GITHUB_APP_CLIENT_ID") || c["connectUrl"] != nil {
			t.Errorf("%s = %v", name, c)
		}
	}
	if m := a["telegram"]["message"].(string); a["telegram"]["configured"] != false || !strings.Contains(m, "TELEGRAM_BOT_TOKEN") {
		t.Errorf("telegram without bot = %v", a["telegram"])
	}
	// Every route that needs the connector answers 501 instead of failing later.
	for name, r := range map[string]*rig{"no key": noKey, "no app": noApp, "no oauth": noOAuth} {
		for _, rt := range [][2]string{{"GET", "/api/connectors/github/connect"}, {"GET", "/api/connectors/github/repos"}, {"POST", "/api/projects/import"},
			{"POST", "/api/connectors/telegram/link"}, {"POST", "/api/connectors/telegram/test"}} {
			if got := r.do(rt[0], rt[1], "alice", `{"repo":"acme/api"}`).StatusCode; got != 501 {
				t.Errorf("%s: %s %s = %d, want 501", name, rt[0], rt[1], got)
			}
		}
	}
}

func TestPublicBasePrecedence(t *testing.T) {
	// Arrange
	req := func(host string, tls bool) *http.Request {
		r := httptest.NewRequest("GET", "http://"+host+"/x", nil)
		if tls {
			r = httptest.NewRequest("GET", "https://"+host+"/x", nil)
		}
		return r
	}
	for name, tc := range map[string]struct {
		opt  Options
		r    *http.Request
		want string
	}{
		"configured URL wins, trailing slash trimmed": {Options{PublicURL: "https://app.example.com/", OIDC: &OIDCConfig{RedirectURL: "https://sso.example.com/auth/callback"}}, req("internal:8080", false), "https://app.example.com"},
		"OIDC redirect URL":                           {Options{OIDC: &OIDCConfig{RedirectURL: "https://sso.example.com:8443/auth/callback"}}, req("internal:8080", false), "https://sso.example.com:8443"},
		"OIDC without a usable URL falls back":        {Options{OIDC: &OIDCConfig{RedirectURL: "/auth/callback"}}, req("internal:8080", false), "http://internal:8080"},
		"plain request":                               {Options{}, req("harness.local:9000", false), "http://harness.local:9000"},
		"TLS request":                                 {Options{}, req("harness.local", true), "https://harness.local"},
	} {
		// Act
		got := (&server{opt: tc.opt}).publicBase(tc.r)

		// Assert
		if got != tc.want {
			t.Errorf("%s: publicBase = %q, want %q", name, got, tc.want)
		}
	}
}

func TestGitHubCallbackFailuresLinkNothing(t *testing.T) {
	// Arrange
	r, _, tm := cnGitHubRig(t, nil)
	valid := cnForgeState(t, "alice", time.Now().Add(time.Minute))
	for name, tc := range map[string]struct {
		user, query, want string
	}{
		"not logged in":        {"", "code=alice&state=" + valid, "/auth/login?next=%2F%23%2Fprofile"},
		"no state":             {"alice", "code=alice", "/#/profile?github=invalid_state"},
		"expired state":        {"alice", "code=alice&state=" + cnForgeState(t, "alice", time.Now().Add(-time.Second)), "/#/profile?github=invalid_state"},
		"state of another":     {"alice", "code=alice&state=" + cnForgeState(t, "bob", time.Now().Add(time.Minute)), "/#/profile?github=invalid_state"},
		"token exchange error": {"alice", "code=bad&state=" + valid, "/#/profile?github=failed"},
		"unknown GitHub user":  {"alice", "code=ghost&state=" + valid, "/#/profile?github=failed"},
	} {
		// Act
		resp := cnGet(t, r, "/auth/github/callback?"+tc.query, tc.user)

		// Assert
		if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != tc.want {
			t.Errorf("%s: %d %q, want %q", name, resp.StatusCode, resp.Header.Get("Location"), tc.want)
		}
	}
	if _, err := tm.GetConnector(context.Background(), "alice", store.ConnectorGitHub); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a failed callback linked an account: %v", err)
	}
	// The API-prefixed callback is the same handler, and a store failure is reported, not swallowed.
	tm.failOn("SetConnector", errCnBoom)
	if got := cnGet(t, r, "/api/auth/github/callback?code=alice&state="+valid, "alice").Header.Get("Location"); got != "/#/profile?github=failed" {
		t.Fatalf("store failure: %q", got)
	}
	tm.failOn("SetConnector", nil)
	if got := cnGet(t, r, "/api/auth/github/callback?code=alice&state="+valid, "alice").Header.Get("Location"); got != "/#/profile?github=connected" {
		t.Fatalf("api-prefixed callback: %q", got)
	}
	if cn, err := tm.GetConnector(context.Background(), "alice", store.ConnectorGitHub); err != nil || cn.Account != "alicegh" || cn.Label != "@AliceGH" {
		t.Fatalf("connector = %+v, %v", cn, err)
	}
}

func TestGitHubCallbackWithoutOAuthCredentials(t *testing.T) {
	// Arrange
	r, _, _ := cnGitHubRig(t, func(a *GitHubApp) { a.ClientSecret = "" })

	// Act
	resp := cnGet(t, r, "/auth/github/callback?code=alice&state="+cnForgeState(t, "alice", time.Now().Add(time.Minute)), "alice")

	// Assert
	if got := resp.Header.Get("Location"); got != "/#/profile?github=not_configured" {
		t.Fatalf("Location = %q", got)
	}
}

func TestGitHubTokenRequestParsesExpiryAndErrors(t *testing.T) {
	// Arrange
	var resp string
	var status int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != 0 {
			w.WriteHeader(status)
		}
		w.Write([]byte(resp))
	}))
	g := &GitHubApp{WebBase: srv.URL, ClientID: "cid", ClientSecret: "cs"}

	// Act / Assert: a token with a lifetime carries its expiry and refresh token.
	resp = `{"access_token":"a1","refresh_token":"r1","expires_in":3600}`
	tok, err := g.tokenRequest(context.Background(), url.Values{"code": {"c"}})
	if err != nil || tok.Access != "a1" || tok.Refresh != "r1" || time.Until(tok.Expires) < 59*time.Minute || time.Until(tok.Expires) > time.Hour {
		t.Fatalf("token = %+v, %v", tok, err)
	}
	// A token that never expires has no expiry (zero: it is never refreshed).
	resp = `{"access_token":"a2"}`
	if tok, err = g.tokenRequest(context.Background(), url.Values{}); err != nil || !tok.Expires.IsZero() || tok.Refresh != "" {
		t.Fatalf("non-expiring token = %+v, %v", tok, err)
	}
	// GitHub reports failures as 200 with an error field, or as garbage.
	for _, body := range []string{`{"error":"bad_verification_code"}`, `not json`, `{}`} {
		resp = body
		if _, err := g.tokenRequest(context.Background(), url.Values{}); err == nil {
			t.Errorf("body %q accepted", body)
		}
	}
	resp = `{"error":"bad_verification_code"}`
	if _, err := g.tokenRequest(context.Background(), url.Values{}); err == nil || !strings.Contains(err.Error(), "bad_verification_code") {
		t.Errorf("error = %v", err)
	}
	// An unreachable GitHub is an error, not a token.
	srv.Close()
	if _, err := g.tokenRequest(context.Background(), url.Values{}); err == nil {
		t.Fatal("unreachable GitHub gave a token")
	}
	if _, err := (&GitHubApp{WebBase: "http://bad host"}).tokenRequest(context.Background(), url.Values{}); err == nil {
		t.Fatal("invalid URL accepted")
	}
}

func TestExpiredGitHubTokenIsRefreshedOnUse(t *testing.T) {
	for name, tc := range map[string]struct {
		expires     time.Duration
		wantRefresh bool
	}{
		"expired":                     {-time.Minute, true},
		"expires within the margin":   {30 * time.Second, true},
		"still valid":                 {10 * time.Minute, false},
		"never expires (zero expiry)": {0, false},
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			r, gh, tm := cnGitHubRig(t, nil)
			tok := githubTokens{Access: "tok-old", Refresh: "ref-old"}
			if tc.expires != 0 {
				tok.Expires = time.Now().Add(tc.expires)
			}
			cnSeedGitHub(t, tm, "alice", tok)

			// Act
			repos := r.json("GET", "/api/connectors/github/repos", "alice", "", 200)["repos"].([]any)

			// Assert
			if len(repos) != 2 {
				t.Fatalf("repos = %v", repos)
			}
			forms := gh.forms()
			if !tc.wantRefresh {
				if len(forms) != 0 || gh.auths()[0] != "Bearer tok-old" {
					t.Fatalf("a usable token was refreshed: %v %v", forms, gh.auths())
				}
				return
			}
			if len(forms) != 1 || forms[0].Get("grant_type") != "refresh_token" || forms[0].Get("refresh_token") != "ref-old" ||
				forms[0].Get("client_id") != "cid" || forms[0].Get("client_secret") != "csecret" {
				t.Fatalf("refresh request = %v", forms)
			}
			if got := gh.auths(); got[0] != "Bearer tok-new" {
				t.Fatalf("the API was called with %v, not the refreshed token", got)
			}
			if st := cnStoredTokens(t, tm, "alice"); st.Access != "tok-new" || st.Refresh != "ref-new" || time.Until(st.Expires) < 7*time.Hour {
				t.Fatalf("stored tokens = %+v", st)
			}
		})
	}
}

func TestGitHubRefreshFailuresAskTheUserToReconnect(t *testing.T) {
	expired := githubTokens{Access: "tok-old", Refresh: "ref-old", Expires: time.Now().Add(-time.Hour)}
	for name, tc := range map[string]struct {
		tok  githubTokens
		prep func(gh *cnGH, tm *cnFaultTeam)
		want int
		code string
	}{
		"no refresh token":          {githubTokens{Access: "tok-old", Expires: expired.Expires}, nil, 409, "expired"},
		"GitHub refuses refresh":    {expired, func(gh *cnGH, _ *cnFaultTeam) { gh.set(func(g *cnGH) { g.refreshFails = true }) }, 409, "expired"},
		"cannot save the new token": {expired, func(_ *cnGH, tm *cnFaultTeam) { tm.failOn("SetConnector", errCnBoom) }, 502, "github_error"},
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			r, gh, tm := cnGitHubRig(t, nil)
			cnSeedGitHub(t, tm, "alice", tc.tok)
			if tc.prep != nil {
				tc.prep(gh, tm)
			}

			// Act: both the listing and the import go through the same token logic.
			list := r.json("GET", "/api/connectors/github/repos", "alice", "", tc.want)
			imp := r.json("POST", "/api/projects/import", "alice", `{"repo":"acme/api"}`, tc.want)

			// Assert
			if cnErrCode(list) != tc.code || cnErrCode(imp) != tc.code {
				t.Fatalf("list = %v, import = %v", list, imp)
			}
			if len(gh.auths()) != 0 {
				t.Fatalf("GitHub was called with a dead token: %v", gh.auths())
			}
			if st := cnStoredTokens(t, tm, "alice"); st.Access != "tok-old" {
				t.Fatalf("the stored token changed to %+v", st)
			}
		})
	}
}

func TestGitHubConnectorWithUnreadableTokenIsAGatewayError(t *testing.T) {
	// Arrange: a connector sealed with another key (rotated HARNESS_SECRET_KEY), and one holding garbage.
	r, _, tm := cnGitHubRig(t, nil)
	other, _ := newSecretBox([]byte("another-secret-key-another-secret"))
	garbage, _ := newSecretBox(cnKey)
	for login, data := range map[string][]byte{"alice": other.seal([]byte(`{"access":"x"}`)), "bob": garbage.seal([]byte("not json"))} {
		if err := tm.SetConnector(context.Background(), store.Connector{Login: login, Kind: store.ConnectorGitHub, Account: login, Data: data}); err != nil {
			t.Fatal(err)
		}
	}

	// Act / Assert
	for _, u := range []string{"alice", "bob"} {
		if got := cnErrCode(r.json("GET", "/api/connectors/github/repos", u, "", 502)); got != "github_error" {
			t.Errorf("%s: code %q", u, got)
		}
	}
}

func TestGitHubRepoListingFailsWholeWhenAnyInstallationFails(t *testing.T) {
	for name, prep := range map[string]func(g *cnGH){
		"installations":              func(g *cnGH) { g.installStatus = 500 },
		"repositories of the second": func(g *cnGH) { g.installations = []int{11, 12}; g.repos[12] = nil; g.repoStatus[12] = 502 },
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			r, gh, tm := cnGitHubRig(t, nil)
			cnSeedGitHub(t, tm, "alice", githubTokens{Access: "tok-alice"})
			gh.set(prep)

			// Act / Assert: a partial list would hide repositories without saying so.
			if got := cnErrCode(r.json("GET", "/api/connectors/github/repos", "alice", "", 502)); got != "github_error" {
				t.Errorf("list code %q", got)
			}
			if got := cnErrCode(r.json("POST", "/api/projects/import", "alice", `{"repo":"acme/api"}`, 502)); got != "github_error" {
				t.Errorf("import code %q", got)
			}
		})
	}
}

func TestGitHubReposAcrossInstallationsAreSortedFilteredAndFlagged(t *testing.T) {
	// Arrange
	r, gh, tm := cnGitHubRig(t, nil)
	cnSeedGitHub(t, tm, "alice", githubTokens{Access: "tok-alice"})
	gh.set(func(g *cnGH) {
		g.installations = []int{12, 11}
		g.repos[12] = []map[string]any{{"full_name": "zeta/app", "description": "Mobile client", "default_branch": "main"}}
	})

	// Act
	all := r.json("GET", "/api/connectors/github/repos", "alice", "", 200)
	byDesc := r.json("GET", "/api/connectors/github/repos?q=%20MOBILE%20", "alice", "", 200)
	none := r.json("GET", "/api/connectors/github/repos?q=nothing-like-this", "alice", "", 200)

	// Assert
	names := []string{}
	inst := map[string]float64{}
	for _, v := range all["repos"].([]any) {
		m := v.(map[string]any)
		names = append(names, m["fullName"].(string))
		inst[m["fullName"].(string)] = m["installationId"].(float64)
	}
	if strings.Join(names, ",") != "acme/api,acme/web,zeta/app" || inst["zeta/app"] != 12 || inst["acme/api"] != 11 {
		t.Fatalf("repos = %v %v", names, inst)
	}
	if l := byDesc["repos"].([]any); len(l) != 1 || l[0].(map[string]any)["fullName"] != "zeta/app" {
		t.Fatalf("search by description = %v", byDesc)
	}
	if l, ok := none["repos"].([]any); !ok || len(l) != 0 {
		t.Fatalf("no match must be an empty list, got %v", none["repos"])
	}
	if all["installUrl"] != "https://github.com/apps/my-app/installations/new" {
		t.Fatalf("installUrl = %v", all["installUrl"])
	}
}

func TestGitHubReposOmitInstallLinkWithoutSlug(t *testing.T) {
	// Arrange
	r, _, tm := cnGitHubRig(t, func(a *GitHubApp) { a.Slug = "" })
	cnSeedGitHub(t, tm, "alice", githubTokens{Access: "tok-alice"})

	// Act
	res := r.json("GET", "/api/connectors/github/repos", "alice", "", 200)

	// Assert
	if _, has := res["installUrl"]; has {
		t.Fatalf("a link to a nameless app: %v", res["installUrl"])
	}
}

func TestImportProjectValidatesAndReportsFailures(t *testing.T) {
	// Arrange
	r, _, tm := cnGitHubRig(t, nil)
	cnSeedGitHub(t, tm, "alice", githubTokens{Access: "tok-alice"})

	// Act / Assert
	r.json("POST", "/api/projects/import", "alice", `{"repo":`, 400)
	r.json("POST", "/api/projects/import", "alice", `{}`, 404)
	// The name matches regardless of case and surrounding spaces; the project takes GitHub's spelling.
	p := r.json("POST", "/api/projects/import", "alice", `{"repo":"  ACME/Web "}`, 201)
	if p["repoUrl"] != "https://github.com/acme/web" || p["name"] != "web" || p["description"] != "Website" || p["defaultBranch"] != "trunk" {
		t.Fatalf("imported = %v", p)
	}
	// Importing it again, however spelled, is refused.
	if got := cnErrCode(r.json("POST", "/api/projects/import", "alice", `{"repo":"acme/web"}`, 409)); got != "already_imported" {
		t.Fatalf("code %q", got)
	}
	tm.failOn("CreateProject", errCnBoom)
	r.json("POST", "/api/projects/import", "alice", `{"repo":"acme/api"}`, 500)
	tm.failOn("CreateProject", nil)
	if list, _ := tm.ListProjects(context.Background(), "alice"); len(list) != 1 {
		t.Fatalf("projects after a failed import = %d, want 1", len(list))
	}
}

func TestGitHubDisconnectAndConnectorListErrors(t *testing.T) {
	// Arrange
	r, _, tm := cnGitHubRig(t, nil)

	// Act / Assert: disconnecting when not connected is fine (idempotent), a failing store is not.
	if got := r.do("DELETE", "/api/connectors/github", "alice", "").StatusCode; got != 204 {
		t.Fatalf("disconnect when not connected = %d", got)
	}
	tm.failOn("DeleteConnector", errCnBoom)
	if got := r.do("DELETE", "/api/connectors/github", "alice", "").StatusCode; got != 500 {
		t.Fatalf("disconnect with a failing store = %d", got)
	}
	tm.failOn("ListConnectors", errCnBoom)
	if got := r.do("GET", "/api/connectors", "alice", "").StatusCode; got != 500 {
		t.Fatalf("list with a failing store = %d", got)
	}
}

func TestGitHubConnectStartRedirectsWithABoundExpiringState(t *testing.T) {
	// Arrange
	r, gh, _ := cnGitHubRig(t, nil)

	// Act
	resp := cnGet(t, r, "/api/connectors/github/connect", "alice")

	// Assert: the state names the user and expires, and the callback is built from the public URL (no double slash).
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != 302 || !strings.HasPrefix(resp.Header.Get("Location"), gh.srv.URL+"/login/oauth/authorize?") ||
		loc.Query().Get("redirect_uri") != "https://harness.test/auth/github/callback" {
		t.Fatalf("redirect = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	box, _ := newSecretBox(cnKey)
	raw, err := decodeB64(loc.Query().Get("state"))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := box.open(raw)
	if err != nil {
		t.Fatal(err)
	}
	var st oauthState
	if err := json.Unmarshal(plain, &st); err != nil || st.Login != "alice" || time.Until(time.Unix(st.Exp, 0)) > 10*time.Minute || time.Until(time.Unix(st.Exp, 0)) < 9*time.Minute {
		t.Fatalf("state = %+v, %v", st, err)
	}
}

func TestSecretBoxRefusesWrongKeysAndTamperedData(t *testing.T) {
	// Arrange
	if _, err := newSecretBox(nil); err == nil {
		t.Fatal("an empty key was accepted")
	}
	a, _ := newSecretBox([]byte("key-a"))
	b, _ := newSecretBox([]byte("key-b"))
	sealed := a.seal([]byte("secret token"))

	// Act / Assert: round trip, fresh nonce per seal, and nothing opens with another key or after tampering.
	if plain, err := a.open(sealed); err != nil || string(plain) != "secret token" {
		t.Fatalf("round trip = %q, %v", plain, err)
	}
	if string(sealed) == string(a.seal([]byte("secret token"))) {
		t.Fatal("two seals of the same data are identical: the nonce is reused")
	}
	if _, err := b.open(sealed); err == nil {
		t.Fatal("opened with another key")
	}
	flipped := append([]byte(nil), sealed...)
	flipped[len(flipped)-1] ^= 1
	if _, err := a.open(flipped); err == nil {
		t.Fatal("opened tampered data")
	}
	if _, err := a.open(sealed[:5]); err == nil {
		t.Fatal("opened data shorter than a nonce")
	}
	if _, err := a.open(nil); err == nil {
		t.Fatal("opened nothing")
	}
	if plain, err := a.open(a.seal(nil)); err != nil || len(plain) != 0 {
		t.Fatalf("empty plaintext = %q, %v", plain, err)
	}
}

func TestSeenRoutesValidateClampAndReportStoreErrors(t *testing.T) {
	// Arrange
	r, _, tm := cnGitHubRig(t, nil)
	th := r.json("POST", "/api/threads", "alice", `{}`, 201)["id"].(string)

	// Act / Assert: input validation.
	r.json("POST", "/api/seen", "alice", `{"threadId":`, 400)
	if got := cnErrCode(r.json("POST", "/api/seen", "alice", `{}`, 400)); got != "invalid_request" {
		t.Fatalf("code %q", got)
	}
	// A timestamp in the future is replaced by now: the stored time never moves backwards, so accepting it
	// would hide every later visit.
	r.json("POST", "/api/seen", "alice", `{"threadId":"`+th+`","at":"2099-01-01T00:00:00Z"}`, 204)
	seen, _ := tm.SeenThreads(context.Background(), "alice")
	if at := seen[th]; at.IsZero() || at.After(time.Now().Add(time.Minute)) {
		t.Fatalf("stored seen time = %v", at)
	}
	// No time at all means now.
	r.json("POST", "/api/seen", "alice", `{"threadId":"`+th+`"}`, 204)
	if after, _ := tm.SeenThreads(context.Background(), "alice"); after[th].Before(seen[th]) {
		t.Fatalf("seen time went backwards: %v -> %v", seen[th], after[th])
	}
	// Store failures are reported.
	tm.failOn("SetThreadSeen", errCnBoom)
	r.json("POST", "/api/seen", "alice", `{"threadId":"`+th+`"}`, 500)
	tm.failOn("SeenThreads", errCnBoom)
	r.json("GET", "/api/seen", "alice", "", 500)
}
