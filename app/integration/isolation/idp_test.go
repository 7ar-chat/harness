package isolation_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// fakeIdP is the smallest OIDC provider the login flow needs: discovery, JWKS, an authorize endpoint that approves
// at once as whoever the current claims describe (PKCE required) and a token endpoint that signs an RS256 id_token.
type fakeIdP struct {
	srv    *httptest.Server
	key    *rsa.PrivateKey
	mu     sync.Mutex
	claims map[string]any
	nonce  string
}

const idpCode = "the-code"

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeIdP{key: key}
	mux := http.NewServeMux()
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": p.srv.URL, "authorization_endpoint": p.srv.URL + "/authorize", "token_endpoint": p.srv.URL + "/token",
			"jwks_uri": p.srv.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
			http.Error(w, "PKCE required", http.StatusBadRequest)
			return
		}
		p.mu.Lock()
		p.nonce = q.Get("nonce")
		p.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+idpCode+"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("code") != idpCode || r.Form.Get("code_verifier") == "" {
			http.Error(w, "bad code", http.StatusBadRequest)
			return
		}
		sig, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
		p.mu.Lock()
		cl := map[string]any{"iss": p.srv.URL, "aud": "cid", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": p.nonce}
		for k, v := range p.claims {
			cl[k] = v
		}
		p.mu.Unlock()
		raw, _ := jwt.Signed(sig).Claims(cl).Serialize()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": raw})
	})
	return p
}

// loginAs makes the next login the identity described by claims.
func (p *fakeIdP) loginAs(claims map[string]any) {
	p.mu.Lock()
	p.claims = claims
	p.mu.Unlock()
}

func user(login, email string) map[string]any {
	return map[string]any{"sub": "sub-" + login, "preferred_username": login, "email": email, "email_verified": true, "name": login}
}
