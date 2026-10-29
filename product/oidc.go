package product

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/GhaziBenDahmane/harness/core/store"
)

// OIDCConfig configures the OpenID Connect login (authorization code flow with
// PKCE, run by the server: the browser only ever holds an HttpOnly session
// cookie, which is also what the event streams, that cannot set headers, need).
type OIDCConfig struct {
	// Issuer is the provider's issuer URL (discovery is read from it).
	Issuer       string
	ClientID     string
	ClientSecret string
	// RedirectURL is the callback the provider redirects to: https://host/auth/callback.
	RedirectURL string
	// Scopes besides "openid" (default profile, email).
	Scopes []string
	// LoginClaim is the claim used as the user's login: the key of threads,
	// memberships and memory (default "preferred_username", then "email", then
	// "sub"). Pick a claim that never changes for a person.
	LoginClaim string
	// AllowedDomains, when set, restricts logins to these email domains.
	AllowedDomains []string
	// SessionKey signs the session cookies (32 random bytes; random at start
	// when empty, which logs everyone out on restart).
	SessionKey []byte
	// SessionTTL is the session lifetime (default 12 h).
	SessionTTL time.Duration
}

const (
	sessionCookie = "harness_session"
	flowCookie    = "harness_oidc_flow"
)

// oidcAuth is the login flow and the session store.
type oidcAuth struct {
	cfg      OIDCConfig
	provider *oidc.Provider
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
	team     store.TeamStore
	key      []byte
	ttl      time.Duration
	secure   bool
}

// newOIDC discovers the provider and returns the handler set.
func newOIDC(ctx context.Context, cfg OIDCConfig, team store.TeamStore) (*oidcAuth, error) {
	if team == nil {
		return nil, errors.New("httpserver: OIDC needs Options.Team")
	}
	p, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("httpserver: OIDC discovery: %w", err)
	}
	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = []string{"profile", "email"}
	}
	a := &oidcAuth{
		cfg: cfg, provider: p, team: team, key: cfg.SessionKey, ttl: cfg.SessionTTL,
		oauth: oauth2.Config{
			ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, RedirectURL: cfg.RedirectURL,
			Endpoint: p.Endpoint(), Scopes: append([]string{oidc.ScopeOpenID}, scopes...),
		},
		verifier: p.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		secure:   strings.HasPrefix(cfg.RedirectURL, "https://"),
	}
	if len(a.key) < 16 {
		a.key = make([]byte, 32)
		_, _ = rand.Read(a.key)
		slog.Warn("httpserver: no OIDC session key; sessions end when the server restarts")
	}
	if a.ttl <= 0 {
		a.ttl = 12 * time.Hour
	}
	return a, nil
}

func (a *oidcAuth) mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", a.login)
	mux.HandleFunc("GET /auth/callback", a.callback)
	mux.HandleFunc("POST /auth/logout", a.logout)
}

// authenticator maps the session cookie to a login.
func (a *oidcAuth) authenticator() Authenticator {
	return func(r *http.Request) (string, error) {
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			return "", errors.New("no session")
		}
		var s struct {
			Login string `json:"l"`
			Exp   int64  `json:"e"`
		}
		if err := a.open(c.Value, &s); err != nil || s.Login == "" || time.Now().Unix() > s.Exp {
			return "", errors.New("invalid session")
		}
		return s.Login, nil
	}
}

// seal signs v: base64(json).base64(hmac).
func (a *oidcAuth) seal(v any) string {
	b, _ := json.Marshal(v)
	body := base64.RawURLEncoding.EncodeToString(b)
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *oidcAuth) open(token string, v any) error {
	body, sig, ok := strings.Cut(token, ".")
	if !ok {
		return errors.New("malformed")
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(body))
	if !hmac.Equal(got, mac.Sum(nil)) {
		return errors.New("bad signature")
	}
	b, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (a *oidcAuth) setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true,
		Secure: a.secure, SameSite: http.SameSiteLaxMode})
}

type flowState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Next     string `json:"x"`
	Exp      int64  `json:"e"`
}

// safeNext keeps the post-login redirect on this site.
func safeNext(next string) string {
	u, err := url.Parse(next)
	if err != nil || u.Host != "" || u.Scheme != "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	return next
}

func (a *oidcAuth) login(w http.ResponseWriter, r *http.Request) {
	f := flowState{State: randomString(18), Nonce: randomString(18), Verifier: oauth2.GenerateVerifier(),
		Next: safeNext(r.URL.Query().Get("next")), Exp: time.Now().Add(10 * time.Minute).Unix()}
	a.setCookie(w, flowCookie, a.seal(f), 600)
	http.Redirect(w, r, a.oauth.AuthCodeURL(f.State, oidc.Nonce(f.Nonce), oauth2.S256ChallengeOption(f.Verifier)), http.StatusFound)
}

func (a *oidcAuth) callback(w http.ResponseWriter, r *http.Request) {
	fail := func(status int, msg string) {
		a.setCookie(w, flowCookie, "", -1)
		writeError(w, status, "login_failed", msg)
	}
	c, err := r.Cookie(flowCookie)
	var f flowState
	if err != nil || a.open(c.Value, &f) != nil || time.Now().Unix() > f.Exp {
		fail(http.StatusBadRequest, "The login attempt expired; try again.")
		return
	}
	q := r.URL.Query()
	if q.Get("error") != "" {
		fail(http.StatusUnauthorized, "The identity provider refused the login.")
		return
	}
	if !hmac.Equal([]byte(q.Get("state")), []byte(f.State)) {
		fail(http.StatusBadRequest, "Invalid state.")
		return
	}
	tok, err := a.oauth.Exchange(r.Context(), q.Get("code"), oauth2.VerifierOption(f.Verifier))
	if err != nil {
		slog.Warn("httpserver: OIDC code exchange failed", "error", err)
		fail(http.StatusUnauthorized, "The login could not be completed.")
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := a.verifier.Verify(r.Context(), raw)
	if err != nil || !hmac.Equal([]byte(idt.Nonce), []byte(f.Nonce)) {
		fail(http.StatusUnauthorized, "The identity token is invalid.")
		return
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		fail(http.StatusUnauthorized, "The identity token is unreadable.")
		return
	}
	// Some providers leave profile claims out of the ID token.
	if _, ok := claims["email"]; !ok {
		if ui, err := a.provider.UserInfo(r.Context(), oauth2.StaticTokenSource(tok)); err == nil {
			var extra map[string]any
			if ui.Claims(&extra) == nil {
				for k, v := range extra {
					if _, has := claims[k]; !has {
						claims[k] = v
					}
				}
			}
		}
	}
	u, err := a.userFromClaims(claims, idt.Subject)
	if err != nil {
		fail(http.StatusForbidden, err.Error())
		return
	}
	if _, err := a.team.UpsertUser(r.Context(), u); err != nil {
		slog.Error("httpserver: recording the user failed", "error", err)
		fail(http.StatusInternalServerError, "The login could not be completed.")
		return
	}
	a.setCookie(w, flowCookie, "", -1)
	a.setCookie(w, sessionCookie, a.seal(map[string]any{"l": u.Login, "e": time.Now().Add(a.ttl).Unix()}), int(a.ttl.Seconds()))
	http.Redirect(w, r, f.Next, http.StatusFound)
}

// userFromClaims builds the user and applies the domain allowlist.
func (a *oidcAuth) userFromClaims(claims map[string]any, sub string) (store.User, error) {
	email := strings.ToLower(claimStr(claims, "email"))
	if claims["email_verified"] == false {
		// An unverified address must not claim project invites.
		email = ""
	}
	if len(a.cfg.AllowedDomains) > 0 {
		_, domain, _ := strings.Cut(email, "@")
		ok := false
		for _, d := range a.cfg.AllowedDomains {
			if strings.EqualFold(d, domain) {
				ok = true
			}
		}
		if !ok {
			return store.User{}, errors.New("This account is not allowed to use this server.")
		}
	}
	claim := a.cfg.LoginClaim
	login := ""
	if claim != "" {
		login = claimStr(claims, claim)
	} else {
		// The email is only a login when verified: an unverified one could
		// otherwise take over the login of whoever owns that address.
		if login = claimStr(claims, "preferred_username"); login == "" {
			login = email
		}
	}
	if login == "" {
		login = sub
	}
	login = strings.ToLower(login)
	if login == "" || strings.ContainsAny(login, " \t\r\n/") {
		return store.User{}, errors.New("The identity provider returned no usable login.")
	}
	// The default display name is the first and last name.
	name := strings.TrimSpace(claimStr(claims, "given_name") + " " + claimStr(claims, "family_name"))
	if name == "" {
		name = claimStr(claims, "name")
	}
	if name == "" {
		name = login
	}
	return store.User{Login: login, Email: email, Name: name, AvatarURL: claimStr(claims, "picture")}, nil
}

func (a *oidcAuth) logout(w http.ResponseWriter, r *http.Request) {
	a.setCookie(w, sessionCookie, "", -1)
	w.WriteHeader(http.StatusNoContent)
}

func claimStr(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return strings.TrimSpace(s)
}
