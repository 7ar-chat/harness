package product

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/7ar-chat/harness/core/store"
)

// cnIdP is an OIDC provider whose id token, userinfo and token endpoint the test steers.
type cnIdP struct {
	srv *httptest.Server
	key *rsa.PrivateKey

	mu         sync.Mutex
	claims     map[string]any // id token claims
	userinfo   map[string]any // nil: the provider has no userinfo endpoint answer (500)
	nonce      string
	badNonce   bool
	noIDToken  bool
	tokenError bool
}

func newCnIdP(t *testing.T, claims, userinfo map[string]any) *cnIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &cnIdP{key: key, claims: claims, userinfo: userinfo}
	mux := http.NewServeMux()
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"issuer": p.srv.URL, "authorization_endpoint": p.srv.URL + "/authorize", "token_endpoint": p.srv.URL + "/token",
			"jwks_uri": p.srv.URL + "/jwks", "userinfo_endpoint": p.srv.URL + "/userinfo", "id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		p.mu.Lock()
		p.nonce = q.Get("nonce")
		p.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=the-code&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.tokenError {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		out := map[string]any{"access_token": "at", "token_type": "Bearer"}
		if !p.noIDToken {
			sig, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
			cl := map[string]any{"iss": p.srv.URL, "aud": "cid", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": p.nonce}
			if p.badNonce {
				cl["nonce"] = "not-the-nonce"
			}
			for k, v := range p.claims {
				cl[k] = v
			}
			out["id_token"], _ = jwt.Signed(sig).Claims(cl).Serialize()
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		info := p.userinfo
		p.mu.Unlock()
		if info == nil {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(info)
	})
	return p
}

func (p *cnIdP) set(f func(p *cnIdP)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f(p)
}

// cnOIDCApp serves the login routes of an oidcAuth built from cfg on a test server.
type cnOIDCApp struct {
	a      *oidcAuth
	srv    *httptest.Server
	client *http.Client
	team   store.TeamStore
}

func newCnOIDCApp(t *testing.T, idp *cnIdP, mutate func(*OIDCConfig), team store.TeamStore) *cnOIDCApp {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cfg := OIDCConfig{Issuer: idp.srv.URL, ClientID: "cid", ClientSecret: "s", RedirectURL: srv.URL + "/auth/callback", SessionKey: []byte("0123456789abcdef0123456789abcdef")}
	if mutate != nil {
		mutate(&cfg)
	}
	a, err := newOIDC(context.Background(), cfg, team)
	if err != nil {
		t.Fatal(err)
	}
	a.mount(mux)
	jar, _ := cookiejar.New(nil)
	return &cnOIDCApp{a: a, srv: srv, team: team, client: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (o *cnOIDCApp) get(t *testing.T, target string) *http.Response {
	t.Helper()
	resp, err := o.client.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// authorize plays the browser through /auth/login and the provider's authorize step, and returns the callback URL
// the provider redirects to (the flow cookie is in the jar).
func (o *cnOIDCApp) authorize(t *testing.T, next string) string {
	t.Helper()
	login := o.get(t, o.srv.URL+"/auth/login?next="+url.QueryEscape(next))
	if login.StatusCode != http.StatusFound {
		t.Fatalf("login = %d", login.StatusCode)
	}
	auth := o.get(t, login.Header.Get("Location"))
	if auth.StatusCode != http.StatusFound {
		t.Fatalf("authorize = %d", auth.StatusCode)
	}
	return auth.Header.Get("Location")
}

func (o *cnOIDCApp) cookie(name string) *http.Cookie {
	u, _ := url.Parse(o.srv.URL)
	for _, c := range o.client.Jar.Cookies(u) {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestOIDCCallbackRejectsBrokenFlows(t *testing.T) {
	good := map[string]any{"sub": "u1", "preferred_username": "alice", "email": "alice@x.io", "email_verified": true}
	for name, tc := range map[string]struct {
		prep   func(idp *cnIdP)
		cb     func(cbURL string) string // rewrites the callback URL the provider redirected to
		noFlow bool                      // callback without the flow cookie
		status int
		msg    string
	}{
		"no flow cookie":            {noFlow: true, status: 400, msg: "expired"},
		"provider reports an error": {cb: func(u string) string { return u + "&error=access_denied" }, status: 401, msg: "refused"},
		"state does not match":      {cb: func(u string) string { return strings.Replace(u, "state=", "state=x", 1) }, status: 400, msg: "Invalid state"},
		"no state":                  {cb: func(u string) string { return strings.Split(u, "&state=")[0] }, status: 400, msg: "Invalid state"},
		"code exchange refused":     {prep: func(idp *cnIdP) { idp.set(func(p *cnIdP) { p.tokenError = true }) }, status: 401, msg: "could not be completed"},
		"no id token":               {prep: func(idp *cnIdP) { idp.set(func(p *cnIdP) { p.noIDToken = true }) }, status: 401, msg: "identity token is invalid"},
		"replayed nonce":            {prep: func(idp *cnIdP) { idp.set(func(p *cnIdP) { p.badNonce = true }) }, status: 401, msg: "identity token is invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			idp := newCnIdP(t, good, nil)
			team := newCnFaultTeam()
			app := newCnOIDCApp(t, idp, nil, team)
			if tc.prep != nil {
				tc.prep(idp)
			}
			cb := app.srv.URL + "/auth/callback?code=the-code&state=whatever"
			if !tc.noFlow {
				cb = app.authorize(t, "/")
			}
			if tc.cb != nil {
				cb = tc.cb(cb)
			}

			// Act
			resp := app.get(t, cb)

			// Assert: refused with a message, no session, no user created, and the flow cookie is burnt.
			body := rawBody(resp.Body)
			if resp.StatusCode != tc.status || !strings.Contains(body, tc.msg) || !strings.Contains(body, "login_failed") {
				t.Fatalf("callback = %d %s", resp.StatusCode, body)
			}
			if app.cookie(sessionCookie) != nil {
				t.Fatal("a session was issued for a failed login")
			}
			if app.cookie(flowCookie) != nil {
				t.Fatal("the flow cookie survived a failed login: it could be replayed")
			}
			if _, err := team.GetUser(context.Background(), "alice"); err == nil {
				t.Fatal("a user was created by a failed login")
			}
		})
	}
}

func TestOIDCCallbackRejectsAnExpiredFlow(t *testing.T) {
	// Arrange: a validly signed flow cookie that is past its expiry.
	idp := newCnIdP(t, map[string]any{"sub": "u1"}, nil)
	app := newCnOIDCApp(t, idp, nil, newCnFaultTeam())
	expired := app.a.seal(flowState{State: "s", Nonce: "n", Verifier: "v", Next: "/", Exp: time.Now().Add(-time.Second).Unix()})
	req, _ := http.NewRequest("GET", app.srv.URL+"/auth/callback?code=c&state=s", nil)
	req.AddCookie(&http.Cookie{Name: flowCookie, Value: expired})

	// Act
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Assert
	if body := rawBody(resp.Body); resp.StatusCode != 400 || !strings.Contains(body, "expired") {
		t.Fatalf("expired flow = %d %s", resp.StatusCode, body)
	}
}

func TestOIDCCallbackLoginOutcomes(t *testing.T) {
	for name, tc := range map[string]struct {
		claims   map[string]any
		userinfo map[string]any
		cfg      func(*OIDCConfig)
		upsert   error
		status   int
		login    string
		email    string
		display  string
	}{
		"profile claims come from userinfo when the id token lacks them": {
			claims:   map[string]any{"sub": "u1"},
			userinfo: map[string]any{"preferred_username": "Alice", "email": "Alice@X.io", "email_verified": true, "given_name": "Alice", "family_name": "Liddell", "picture": "https://idp/p.png"},
			status:   302, login: "alice", email: "alice@x.io", display: "Alice Liddell"},
		"userinfo never overrides the signed id token": {
			claims:   map[string]any{"sub": "u1", "preferred_username": "alice", "name": "Signed Name"},
			userinfo: map[string]any{"preferred_username": "mallory", "name": "Spoofed", "email": "alice@x.io"},
			status:   302, login: "alice", email: "alice@x.io", display: "Signed Name"},
		"userinfo failing is tolerated": {
			claims:   map[string]any{"sub": "u1", "preferred_username": "alice"},
			userinfo: nil,
			status:   302, login: "alice", display: "alice"},
		"id token with an email skips userinfo": {
			claims:   map[string]any{"sub": "u1", "email": "a@x.io", "email_verified": true},
			userinfo: map[string]any{"preferred_username": "from-userinfo"},
			status:   302, login: "a@x.io", email: "a@x.io", display: "a@x.io"},
		"configured login claim": {
			claims: map[string]any{"sub": "u1", "preferred_username": "alice", "employee": "E-42"},
			cfg:    func(c *OIDCConfig) { c.LoginClaim = "employee" },
			status: 302, login: "e-42", display: "e-42"},
		"login claim missing falls back to the subject": {
			claims: map[string]any{"sub": "Subject-9", "preferred_username": "alice"},
			cfg:    func(c *OIDCConfig) { c.LoginClaim = "employee" },
			status: 302, login: "subject-9", display: "subject-9"},
		"domain not allowed": {
			claims: map[string]any{"sub": "u1", "email": "eve@evil.io", "email_verified": true, "preferred_username": "eve"},
			cfg:    func(c *OIDCConfig) { c.AllowedDomains = []string{"x.io"} },
			status: 403},
		"login with a space is unusable": {
			claims: map[string]any{"sub": "u1", "preferred_username": "two words"},
			status: 403},
		"store failure": {
			claims: map[string]any{"sub": "u1", "preferred_username": "alice"},
			upsert: errCnBoom, status: 500},
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			idp := newCnIdP(t, tc.claims, tc.userinfo)
			team := newCnFaultTeam()
			team.failOn("UpsertUser", tc.upsert)
			app := newCnOIDCApp(t, idp, tc.cfg, team)

			// Act
			resp := app.get(t, app.authorize(t, "/#/projects"))

			// Assert
			if resp.StatusCode != tc.status {
				t.Fatalf("callback = %d %s", resp.StatusCode, rawBody(resp.Body))
			}
			if tc.status != 302 {
				if app.cookie(sessionCookie) != nil {
					t.Fatal("a session was issued")
				}
				return
			}
			if resp.Header.Get("Location") != "/#/projects" {
				t.Fatalf("redirected to %q", resp.Header.Get("Location"))
			}
			u, err := team.GetUser(context.Background(), tc.login)
			if err != nil || u.Email != tc.email || u.Label() != tc.display {
				t.Fatalf("user = %+v, %v", u, err)
			}
			req, _ := http.NewRequest("GET", "/", nil)
			req.AddCookie(app.cookie(sessionCookie))
			if got, err := app.a.authenticator()(req); err != nil || got != tc.login {
				t.Fatalf("session logs in %q, %v; want %q", got, err, tc.login)
			}
		})
	}
}

func TestOIDCLogoutEndsTheSession(t *testing.T) {
	// Arrange
	idp := newCnIdP(t, map[string]any{"sub": "u1", "preferred_username": "alice"}, nil)
	app := newCnOIDCApp(t, idp, nil, newCnFaultTeam())
	app.get(t, app.authorize(t, "/"))
	if app.cookie(sessionCookie) == nil {
		t.Fatal("no session after login")
	}

	// Act
	resp, err := app.client.Post(app.srv.URL+"/auth/logout", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Assert: the cookie is cleared (the jar drops a cookie with a negative Max-Age).
	if resp.StatusCode != http.StatusNoContent || app.cookie(sessionCookie) != nil {
		t.Fatalf("logout = %d, session cookie still %v", resp.StatusCode, app.cookie(sessionCookie))
	}
	var cleared bool
	for _, c := range resp.Cookies() {
		cleared = cleared || (c.Name == sessionCookie && c.MaxAge < 0 && c.Value == "")
	}
	if !cleared {
		t.Fatalf("logout did not expire the session cookie: %v", resp.Cookies())
	}
	// Logout is POST only (a GET link must not log people out).
	if got, _ := app.client.Get(app.srv.URL + "/auth/logout"); got.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /auth/logout = %d", got.StatusCode)
	}
}

func TestNewOIDCValidatesAndDefaults(t *testing.T) {
	// Arrange
	idp := newCnIdP(t, map[string]any{"sub": "u1"}, nil)
	ctx := context.Background()

	// Act / Assert: it needs a team store and a reachable issuer.
	if _, err := newOIDC(ctx, OIDCConfig{Issuer: idp.srv.URL}, nil); err == nil {
		t.Fatal("OIDC without a team store")
	}
	if _, err := newOIDC(ctx, OIDCConfig{Issuer: "http://127.0.0.1:1"}, newCnFaultTeam()); err == nil || !strings.Contains(err.Error(), "discovery") {
		t.Fatalf("unreachable issuer: %v", err)
	}
	// Without a usable session key a random one is made (sessions then die with the process); the TTL defaults.
	a, err := newOIDC(ctx, OIDCConfig{Issuer: idp.srv.URL, ClientID: "cid", RedirectURL: "https://app.test/auth/callback", SessionKey: []byte("short")}, newCnFaultTeam())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := newOIDC(ctx, OIDCConfig{Issuer: idp.srv.URL, ClientID: "cid", RedirectURL: "https://app.test/auth/callback", SessionKey: []byte("short")}, newCnFaultTeam())
	if len(a.key) != 32 || string(a.key) == "short" || string(a.key) == string(b.key) {
		t.Fatalf("session keys %q %q", a.key, b.key)
	}
	if a.ttl != 12*time.Hour || !a.secure {
		t.Fatalf("ttl %v secure %v", a.ttl, a.secure)
	}
	if got := strings.Join(a.oauth.Scopes, ","); got != "openid,profile,email" {
		t.Fatalf("scopes %q", got)
	}
	// A cookie signed by one server's random key is worthless to the other.
	req, _ := http.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: a.seal(map[string]any{"l": "alice", "e": time.Now().Add(time.Hour).Unix()})})
	if _, err := b.authenticator()(req); err == nil {
		t.Fatal("a session signed with another key was accepted")
	}
	// Custom scopes are kept after openid; a plain-http redirect gives non-Secure cookies.
	c, _ := newOIDC(ctx, OIDCConfig{Issuer: idp.srv.URL, Scopes: []string{"groups"}, RedirectURL: "http://app.test/cb", SessionTTL: time.Minute}, newCnFaultTeam())
	if strings.Join(c.oauth.Scopes, ",") != "openid,groups" || c.secure || c.ttl != time.Minute {
		t.Fatalf("scopes %v secure %v ttl %v", c.oauth.Scopes, c.secure, c.ttl)
	}
}

func TestOIDCOpenRejectsMalformedTokens(t *testing.T) {
	// Arrange
	a := &oidcAuth{key: []byte("0123456789abcdef0123456789abcdef")}
	good := a.seal(map[string]any{"l": "alice"})
	body, sig, _ := strings.Cut(good, ".")
	var v map[string]any

	// Act / Assert
	if err := a.open(good, &v); err != nil || v["l"] != "alice" {
		t.Fatalf("good token: %v %v", v, err)
	}
	for name, tok := range map[string]string{
		"no separator":          "abc",
		"signature not base64":  body + ".!!!",
		"signature wrong":       body + "." + sig[:len(sig)-2] + "AA",
		"signed but not base64": signedBody(a, "%%%"),
		"signed but not JSON":   signedBody(a, "bm90IGpzb24"),
	} {
		if err := a.open(tok, &v); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// signedBody signs body exactly as oidcAuth.seal does, without encoding it first.
func signedBody(a *oidcAuth, body string) string {
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
