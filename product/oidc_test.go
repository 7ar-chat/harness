package product

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/GhaziBenDahmane/harness/adapters/store/teamstore"
)

// fakeIdP is the smallest OIDC provider the login flow needs: discovery, JWKS,
// authorize (auto-approves as the configured user) and token.
type fakeIdP struct {
	srv    *httptest.Server
	key    *rsa.PrivateKey
	claims map[string]any
	nonce  string
	code   string
}

func newFakeIdP(t *testing.T, claims map[string]any) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeIdP{key: key, claims: claims, code: "the-code"}
	mux := http.NewServeMux()
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": p.srv.URL, "authorization_endpoint": p.srv.URL + "/authorize", "token_endpoint": p.srv.URL + "/token",
			"jwks_uri": p.srv.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		p.nonce = q.Get("nonce")
		if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
			http.Error(w, "PKCE required", 400)
			return
		}
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+p.code+"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("code") != p.code || r.Form.Get("code_verifier") == "" {
			http.Error(w, "bad code", 400)
			return
		}
		sig, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
		cl := map[string]any{"iss": p.srv.URL, "aud": "cid", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": p.nonce}
		for k, v := range p.claims {
			cl[k] = v
		}
		raw, _ := jwt.Signed(sig).Claims(cl).Serialize()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": raw})
	})
	return p
}

func TestOIDCLoginCreatesSessionAndUser(t *testing.T) {
	idp := newFakeIdP(t, map[string]any{"sub": "u-1", "preferred_username": "Alice", "email": "Alice@X.io", "email_verified": true, "name": "Alice A"})
	team := teamstore.NewMem()
	var app *httptest.Server
	cfg := OIDCConfig{Issuer: idp.srv.URL, ClientID: "cid", ClientSecret: "s", SessionKey: []byte("0123456789abcdef0123456789abcdef")}
	// The redirect URL needs the app's address: start the server first.
	mux := http.NewServeMux()
	app = httptest.NewServer(mux)
	defer app.Close()
	cfg.RedirectURL = app.URL + "/auth/callback"
	a, err := newOIDC(context.Background(), cfg, team)
	if err != nil {
		t.Fatal(err)
	}
	a.mount(mux)
	mux.HandleFunc("/whoami", func(w http.ResponseWriter, r *http.Request) {
		l, err := a.authenticator()(r)
		if err != nil {
			http.Error(w, "no", 401)
			return
		}
		w.Write([]byte(l))
	})

	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	if resp, _ := c.Get(app.URL + "/whoami"); resp.StatusCode != 401 {
		t.Fatalf("anonymous whoami = %d", resp.StatusCode)
	}
	resp, err := c.Get(app.URL + "/auth/login?next=/projects")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Request.URL.Path != "/projects" && resp.StatusCode != 404 { // the app has no /projects route: 404 after the redirect proves the flow
		t.Fatalf("login ended at %s (%d)", resp.Request.URL, resp.StatusCode)
	}
	resp, _ = c.Get(app.URL + "/whoami")
	buf := new(strings.Builder)
	b := make([]byte, 64)
	n, _ := resp.Body.Read(b)
	buf.Write(b[:n])
	if resp.StatusCode != 200 || buf.String() != "alice" {
		t.Fatalf("whoami = %d %q", resp.StatusCode, buf.String())
	}
	u, err := team.GetUser(context.Background(), "alice")
	if err != nil || u.Email != "alice@x.io" || u.Name != "Alice A" {
		t.Fatalf("user = %+v, %v", u, err)
	}
}

func TestOIDCRejectsForgedSessionsAndUnsafeNext(t *testing.T) {
	a := &oidcAuth{key: []byte("0123456789abcdef0123456789abcdef"), ttl: time.Hour}
	good := a.seal(map[string]any{"l": "alice", "e": time.Now().Add(time.Hour).Unix()})
	expired := a.seal(map[string]any{"l": "alice", "e": time.Now().Add(-time.Hour).Unix()})
	other := (&oidcAuth{key: []byte("another-key-another-key-another!")}).seal(map[string]any{"l": "mallory", "e": time.Now().Add(time.Hour).Unix()})
	for name, tok := range map[string]string{"good": good, "expired": expired, "other key": other, "garbage": "x.y", "empty": ""} {
		req, _ := http.NewRequest("GET", "/", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
		login, err := a.authenticator()(req)
		if (name == "good") != (err == nil) || (name == "good" && login != "alice") {
			t.Errorf("%s: login=%q err=%v", name, login, err)
		}
	}
	for next, want := range map[string]string{"/a/b?c=1": "/a/b?c=1", "https://evil.io": "/", "//evil.io": "/", "/\\evil.io": "/", "": "/", "javascript:1": "/"} {
		if got := safeNext(next); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", next, got, want)
		}
	}
}

func TestOIDCDomainAllowlistAndUnverifiedEmail(t *testing.T) {
	a := &oidcAuth{cfg: OIDCConfig{AllowedDomains: []string{"x.io"}}}
	if _, err := a.userFromClaims(map[string]any{"email": "eve@evil.io", "email_verified": true}, "s"); err == nil {
		t.Fatal("foreign domain allowed")
	}
	if u, err := a.userFromClaims(map[string]any{"email": "bob@x.io", "email_verified": true, "preferred_username": "bob"}, "s"); err != nil || u.Login != "bob" {
		t.Fatalf("allowed user = %+v, %v", u, err)
	}
	b := &oidcAuth{}
	u, _ := b.userFromClaims(map[string]any{"email": "boss@x.io", "email_verified": false}, "sub-9")
	if u.Email != "" || u.Login != "sub-9" {
		t.Fatalf("unverified email must not be used to claim invites: %+v", u)
	}
}

func TestDefaultDisplayNameIsFirstAndLastName(t *testing.T) {
	a := &oidcAuth{}
	for name, tc := range map[string]struct {
		claims map[string]any
		want   string
	}{
		"first and last": {map[string]any{"given_name": "Ada", "family_name": "Lovelace", "name": "ada.l", "preferred_username": "ada"}, "Ada Lovelace"},
		"first only":     {map[string]any{"given_name": "Ada", "preferred_username": "ada"}, "Ada"},
		"name claim":     {map[string]any{"name": "Ada L.", "preferred_username": "ada"}, "Ada L."},
		"login":          {map[string]any{"preferred_username": "ada"}, "ada"},
	} {
		u, err := a.userFromClaims(tc.claims, "sub")
		if err != nil || u.Name != tc.want {
			t.Errorf("%s: name = %q, %v; want %q", name, u.Name, err, tc.want)
		}
	}
}
