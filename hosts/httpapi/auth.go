package httpapi

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

// PreviewPrefix is the path under which project previews are served; the server's security headers and origin guard
// leave it to the handler that serves it.
const PreviewPrefix = "/preview/"

// wrap adds the security headers, the Host guard, the cross-origin guard and CORS.
func (s *Server) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", s.csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		if len(s.O.AllowedHosts) > 0 && !hostAllowed(r.Host, s.O.AllowedHosts) {
			writeError(w, http.StatusForbidden, "forbidden_host", "Host not allowed.")
			return
		}
		if strings.HasPrefix(r.URL.Path, PreviewPrefix) {
			// A project preview (see the product package) is a different application served under this host: it sets
			// its own security headers, and what lets a request in is the unguessable address, not the origin (the
			// preview runs in a sandboxed frame whose requests carry Origin: null).
			next.ServeHTTP(w, r)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r.Host) {
			if !originAllowed(origin, s.O.AllowedOrigins) {
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
