package product

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	sessionCookie = "harness_session"
	flowCookie    = "harness_oidc_flow"
)

// sessions signs and reads the cookies of a login: the session itself and the short-lived state of a login flow.
// The OIDC login and the GitHub login share one, so a session is valid whichever way it was opened.
type sessions struct {
	key    []byte
	ttl    time.Duration
	secure bool
}

// newSessions returns the cookie signer; without a key (16 bytes at least) a random one is used, which ends every session
// when the server restarts.
func newSessions(key []byte, ttl time.Duration, secure bool) sessions {
	if len(key) < 16 {
		key = make([]byte, 32)
		_, _ = rand.Read(key)
		slog.Warn("httpserver: no session key; sessions end when the server restarts")
	}
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	return sessions{key: key, ttl: ttl, secure: secure}
}

// authenticator maps the session cookie to a login.
func (a *sessions) authenticator() Authenticator {
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

// start opens the session of login.
func (a *sessions) start(w http.ResponseWriter, login string) {
	a.setCookie(w, sessionCookie, a.seal(map[string]any{"l": login, "e": time.Now().Add(a.ttl).Unix()}), int(a.ttl.Seconds()))
}

// seal signs v: base64(json).base64(hmac).
func (a *sessions) seal(v any) string {
	b, _ := json.Marshal(v)
	body := base64.RawURLEncoding.EncodeToString(b)
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *sessions) open(token string, v any) error {
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

func (a *sessions) setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true,
		Secure: a.secure, SameSite: http.SameSiteLaxMode})
}

// logout ends the session.
func (a *sessions) logout(w http.ResponseWriter, _ *http.Request) {
	a.setCookie(w, sessionCookie, "", -1)
	w.WriteHeader(http.StatusNoContent)
}
