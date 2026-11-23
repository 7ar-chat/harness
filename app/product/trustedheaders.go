package product

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"strings"
	"sync"

	"github.com/7ar-chat/harness/core/store"
)

// Enterprise single sign-on through a trusted gateway: an identity-aware proxy (oauth2-proxy, Pomerium, Cloudflare
// Access, an API gateway in front of the company IdP) authenticates the person and forwards who they are in request
// headers. The server believes those headers only when the request also carries a shared secret that proves it came
// through the gateway; a request with identity headers and a missing or wrong secret is refused (401), never read
// as anonymous or sent on to the cookie login.
//
//	HARNESS_TRUST_IDENTITY_HEADERS=1       turn it on (team mode); refused at start without the secret
//	HARNESS_IDENTITY_HEADER_SECRET         the shared secret (at least 16 characters); the gateway adds it to every request
//	HARNESS_IDENTITY_SECRET_HEADER         name of the header that carries it            (default X-Harness-Gateway-Secret)
//	HARNESS_IDENTITY_LOGIN_HEADER          the user's login, required                    (default X-Forwarded-User)
//	HARNESS_IDENTITY_EMAIL_HEADER          the user's email                              (default X-Forwarded-Email)
//	HARNESS_IDENTITY_NAME_HEADER           the user's display name                       (default X-Forwarded-Name)
//
// It works beside OIDC and the GitHub login: a request without the login header goes on to the session cookie. The
// first request of a person creates their account, approved (the gateway already admitted them); HARNESS_ADMINS
// names the administrators by login or email as usual. The server must not be reachable except through the gateway:
// the secret is the only thing that tells the gateway from anyone else.

// TrustedHeaders configures the trusted-gateway login.
type TrustedHeaders struct {
	// Secret is the shared secret the gateway sends in SecretHeader.
	Secret []byte
	// SecretHeader, LoginHeader, EmailHeader and NameHeader name the headers (defaults above).
	SecretHeader string
	LoginHeader  string
	EmailHeader  string
	NameHeader   string
}

// MinIdentitySecret is the shortest shared secret accepted.
const MinIdentitySecret = 16

// TrustedHeadersFromEnv reads HARNESS_TRUST_IDENTITY_HEADERS and its companions; nil when it is off. It is an error
// to turn it on without a secret.
func TrustedHeadersFromEnv(getenv func(string) string) (*TrustedHeaders, error) {
	switch strings.ToLower(strings.TrimSpace(getenv("HARNESS_TRUST_IDENTITY_HEADERS"))) {
	case "1", "true", "yes", "on":
	default:
		return nil, nil
	}
	secret := getenv("HARNESS_IDENTITY_HEADER_SECRET")
	if len(secret) < MinIdentitySecret {
		return nil, fmt.Errorf("HARNESS_TRUST_IDENTITY_HEADERS needs HARNESS_IDENTITY_HEADER_SECRET (at least %d characters): without a shared secret anyone could claim to be anyone", MinIdentitySecret)
	}
	t := &TrustedHeaders{Secret: []byte(secret),
		SecretHeader: headerOr(getenv, "HARNESS_IDENTITY_SECRET_HEADER", "X-Harness-Gateway-Secret"),
		LoginHeader:  headerOr(getenv, "HARNESS_IDENTITY_LOGIN_HEADER", "X-Forwarded-User"),
		EmailHeader:  headerOr(getenv, "HARNESS_IDENTITY_EMAIL_HEADER", "X-Forwarded-Email"),
		NameHeader:   headerOr(getenv, "HARNESS_IDENTITY_NAME_HEADER", "X-Forwarded-Name")}
	return t, t.validate()
}

func headerOr(getenv func(string) string, name, def string) string {
	if v := strings.TrimSpace(getenv(name)); v != "" {
		return v
	}
	return def
}

func (t *TrustedHeaders) validate() error {
	if len(t.Secret) < MinIdentitySecret {
		return fmt.Errorf("the identity header secret needs at least %d characters", MinIdentitySecret)
	}
	t.SecretHeader = http.CanonicalHeaderKey(strings.TrimSpace(firstNonEmpty(t.SecretHeader, "X-Harness-Gateway-Secret")))
	t.LoginHeader = http.CanonicalHeaderKey(strings.TrimSpace(firstNonEmpty(t.LoginHeader, "X-Forwarded-User")))
	t.EmailHeader = http.CanonicalHeaderKey(strings.TrimSpace(firstNonEmpty(t.EmailHeader, "X-Forwarded-Email")))
	t.NameHeader = http.CanonicalHeaderKey(strings.TrimSpace(firstNonEmpty(t.NameHeader, "X-Forwarded-Name")))
	seen := map[string]bool{}
	for _, h := range []string{t.SecretHeader, t.LoginHeader, t.EmailHeader, t.NameHeader} {
		if seen[h] {
			return fmt.Errorf("the identity headers must be distinct (%s is used twice)", h)
		}
		seen[h] = true
	}
	return nil
}

var (
	errNoGatewaySecret  = errors.New("identity headers without the gateway secret")
	errBadGatewaySecret = errors.New("identity headers with a wrong gateway secret")
)

// trustedAuth authenticates requests that come through the gateway and hands every other request to next.
type trustedAuth struct {
	cfg   TrustedHeaders
	next  Authenticator
	admit func(ctx context.Context, u store.User) (*store.User, error)

	mu   sync.Mutex
	seen map[string]string // login -> email|name last recorded
}

func newTrustedAuth(cfg TrustedHeaders, next Authenticator, admit func(context.Context, store.User) (*store.User, error)) (*trustedAuth, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &trustedAuth{cfg: cfg, next: next, admit: admit, seen: map[string]string{}}, nil
}

// normalizeLogin lower-cases and trims the login; it must be one printable token (no spaces, no control characters).
func normalizeLogin(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || len(s) > 200 {
		return "", false
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7f {
			return "", false
		}
	}
	return s, true
}

func (a *trustedAuth) authenticate(r *http.Request) (string, error) {
	raw := r.Header.Get(a.cfg.LoginHeader)
	gotSecret := r.Header.Get(a.cfg.SecretHeader)
	if raw == "" && gotSecret == "" {
		if a.next == nil {
			return "", errors.New("no credentials")
		}
		return a.next(r)
	}
	if gotSecret == "" {
		return "", errNoGatewaySecret
	}
	if subtle.ConstantTimeCompare([]byte(gotSecret), a.cfg.Secret) != 1 {
		return "", errBadGatewaySecret
	}
	login, ok := normalizeLogin(raw)
	if !ok {
		return "", errors.New("the gateway sent no usable login")
	}
	email := strings.TrimSpace(r.Header.Get(a.cfg.EmailHeader))
	if email != "" {
		if addr, err := mail.ParseAddress(email); err == nil {
			email = strings.ToLower(addr.Address)
		} else {
			email = ""
		}
	}
	name := strings.TrimSpace(r.Header.Get(a.cfg.NameHeader))
	if len(name) > 200 {
		name = name[:200]
	}
	if err := a.record(r.Context(), login, email, name); err != nil {
		return "", err
	}
	return login, nil
}

// record creates or refreshes the account when the gateway's description of the person changed.
func (a *trustedAuth) record(ctx context.Context, login, email, name string) error {
	fp := email + "|" + name
	a.mu.Lock()
	same := a.seen[login] == fp
	a.mu.Unlock()
	if same {
		return nil
	}
	if _, err := a.admit(ctx, store.User{Login: login, Email: email, Name: name}); err != nil {
		return err
	}
	a.mu.Lock()
	if len(a.seen) > 10000 {
		a.seen = map[string]string{}
	}
	a.seen[login] = fp
	a.mu.Unlock()
	return nil
}
