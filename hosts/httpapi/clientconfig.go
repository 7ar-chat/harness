package httpapi

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ClientConfig tells the browser UI where to send its own errors and usage statistics. Both are optional and
// off by default; the UI reads them from GET /api/client-config at startup, so one build serves every install.
//
//	SentryDSN  a Sentry-protocol DSN (Rustrak, Sentry, GlitchTip) for errors raised in the browser
//	UmamiURL   the Umami server (https://umami.example.com); the UI loads <UmamiURL>/script.js
//	UmamiID    the Umami website id of this UI
//
// The Content-Security-Policy stays at 'self' unless these are set; then it allows exactly the origins they name
// (script-src and connect-src for Umami, connect-src for the tracker) and nothing else.
type ClientConfig struct {
	SentryDSN   string
	SentryEnv   string
	SentryRel   string
	UmamiURL    string
	UmamiID     string
	umamiOrigin string
	sentryOrig  string
}

// clientConfigJSON is the public answer of GET /api/client-config.
type clientConfigJSON struct {
	Sentry *struct {
		DSN         string `json:"dsn"`
		Environment string `json:"environment,omitempty"`
		Release     string `json:"release,omitempty"`
	} `json:"sentry,omitempty"`
	Umami *struct {
		ScriptURL string `json:"scriptUrl"`
		WebsiteID string `json:"websiteId"`
	} `json:"umami,omitempty"`
}

// normalise validates c and fills in the derived origins. An unusable value is an error rather than being
// ignored, so a typo in an environment variable is found at startup.
func (c ClientConfig) normalise() (ClientConfig, error) {
	if c.SentryDSN != "" {
		u, err := url.Parse(c.SentryDSN)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User.Username() == "" || strings.Trim(u.Path, "/") == "" {
			return c, fmt.Errorf("client error tracking: %q is not a DSN (want https://<key>@<host>/<project>)", redact(c.SentryDSN))
		}
		c.sentryOrig = u.Scheme + "://" + u.Host
	}
	if (c.UmamiURL == "") != (c.UmamiID == "") {
		return c, fmt.Errorf("client analytics: the Umami URL and the website id go together")
	}
	if c.UmamiURL != "" {
		u, err := url.Parse(strings.TrimRight(c.UmamiURL, "/"))
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Path != "" {
			return c, fmt.Errorf("client analytics: %q is not an origin (want https://umami.example.com)", c.UmamiURL)
		}
		c.umamiOrigin = u.Scheme + "://" + u.Host
		c.UmamiURL = c.umamiOrigin
	}
	return c, nil
}

// redact hides the key of a DSN in error messages.
func redact(dsn string) string {
	if u, err := url.Parse(dsn); err == nil && u.User != nil {
		u.User = url.User("***")
		return u.String()
	}
	return "<invalid>"
}

// contentSecurityPolicy is the UI's policy: strict 'self', plus the origins of c. The UI loads only its own scripts
// and styles, never inline. Inline style attributes are allowed (component libraries position popovers with them);
// inline <style> elements and every inline script stay blocked.
func contentSecurityPolicy(c ClientConfig) string {
	script, connect := "'self'", "'self'"
	if c.umamiOrigin != "" {
		script += " " + c.umamiOrigin
		connect += " " + c.umamiOrigin
	}
	if c.sentryOrig != "" && c.sentryOrig != c.umamiOrigin {
		connect += " " + c.sentryOrig
	}
	return "default-src 'self'; script-src " + script + "; style-src 'self'; style-src-attr 'unsafe-inline'; img-src 'self' data: https:; connect-src " + connect + "; " +
		"object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
}

func (s *Server) clientConfig(w http.ResponseWriter, _ *http.Request) {
	var out clientConfigJSON
	if c := s.O.Client; c.SentryDSN != "" {
		out.Sentry = &struct {
			DSN         string `json:"dsn"`
			Environment string `json:"environment,omitempty"`
			Release     string `json:"release,omitempty"`
		}{c.SentryDSN, c.SentryEnv, c.SentryRel}
	}
	if c := s.O.Client; c.UmamiURL != "" {
		out.Umami = &struct {
			ScriptURL string `json:"scriptUrl"`
			WebsiteID string `json:"websiteId"`
		}{c.UmamiURL + "/script.js", c.UmamiID}
	}
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, out)
}
