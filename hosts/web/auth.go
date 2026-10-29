package httpserver

import (
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// Authenticator maps a request to the user it acts for. A non-nil error (or an
// empty user) answers 401. Plug in your own for multi-user deployments (a
// session cookie, a JWT, a proxy header).
type Authenticator func(*http.Request) (user string, err error)

// LocalAuthenticator is the default: a single user (default "local"). With a
// non-empty token every request must carry "Authorization: Bearer <token>".
func LocalAuthenticator(user, token string) Authenticator {
	if user == "" {
		user = "local"
	}
	return func(r *http.Request) (string, error) {
		if token == "" {
			return user, nil
		}
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			return "", errors.New("invalid token")
		}
		return user, nil
	}
}

// csp is strict: the UI loads only its own scripts and styles, never inline.
// Inline style attributes are allowed (component libraries position popovers with them); inline
// <style> elements and every inline script stay blocked.
const csp = "default-src 'self'; script-src 'self'; style-src 'self'; style-src-attr 'unsafe-inline'; img-src 'self' data: https:; connect-src 'self'; " +
	"object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// wrap adds the security headers, the Host guard, the cross-origin guard and CORS.
func (s *server) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		if len(s.o.AllowedHosts) > 0 && !hostAllowed(r.Host, s.o.AllowedHosts) {
			writeError(w, http.StatusForbidden, "forbidden_host", "Host not allowed.")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r.Host) {
			if !originAllowed(origin, s.o.AllowedOrigins) {
				writeError(w, http.StatusForbidden, "forbidden_origin", "Origin not allowed.")
				return
			}
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Expose-Headers", "Content-Disposition")
			if r.Method == http.MethodOptions {
				h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Last-Event-ID, X-Harness-User")
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func sameOrigin(origin, host string) bool {
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, host)
}

func originAllowed(origin string, allowed []string) bool {
	for _, a := range allowed {
		if a == "*" || strings.EqualFold(strings.TrimRight(a, "/"), origin) {
			return true
		}
	}
	return false
}

func hostAllowed(hostport string, allowed []string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	for _, a := range allowed {
		if strings.EqualFold(a, host) {
			return true
		}
	}
	return false
}
