package httpapi_test

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/hosts/httpapi"
)

const rustrakDSN = "https://abc123@rustrak.example.com/7"

func TestClientConfigIsEmptyAndTheCSPStaysSelfByDefault(t *testing.T) {
	// Arrange
	r := newRig(t, rigOpts{})

	// Act
	resp := r.do("GET", "/api/client-config", "", "")
	body, _ := io.ReadAll(resp.Body)
	csp := resp.Header.Get("Content-Security-Policy")

	// Assert
	if resp.StatusCode != 200 || strings.TrimSpace(string(body)) != "{}" {
		t.Errorf("status %d body %q, want 200 {}", resp.StatusCode, body)
	}
	if !strings.Contains(csp, "script-src 'self';") || !strings.Contains(csp, "connect-src 'self';") {
		t.Errorf("csp = %q, want script-src and connect-src limited to 'self'", csp)
	}
}

func TestClientConfigIsPublicAndReturnsWhatTheOperatorSet(t *testing.T) {
	// Arrange
	r := newRig(t, rigOpts{web: httpapi.Options{Client: httpapi.ClientConfig{
		SentryDSN: rustrakDSN, SentryEnv: "production", SentryRel: "r1",
		UmamiURL: "https://umami.example.com/", UmamiID: "site-1",
	}}})

	// Act: no user at all
	resp := r.do("GET", "/api/client-config", "", "")
	var got struct {
		Sentry struct{ DSN, Environment, Release string }
		Umami  struct{ ScriptURL, WebsiteID string }
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)

	// Assert
	if resp.StatusCode != 200 {
		t.Fatalf("status %d, want 200 without authentication", resp.StatusCode)
	}
	if got.Sentry.DSN != rustrakDSN || got.Sentry.Environment != "production" || got.Sentry.Release != "r1" ||
		got.Umami.ScriptURL != "https://umami.example.com/script.js" || got.Umami.WebsiteID != "site-1" {
		t.Errorf("got %+v", got)
	}
}

func TestTheCSPAllowsExactlyTheConfiguredOrigins(t *testing.T) {
	// Arrange
	r := newRig(t, rigOpts{web: httpapi.Options{Client: httpapi.ClientConfig{
		SentryDSN: rustrakDSN, UmamiURL: "https://umami.example.com", UmamiID: "site-1",
	}}})

	// Act
	csp := r.do("GET", "/", "", "").Header.Get("Content-Security-Policy")

	// Assert: Umami may load its script and post to its API; the tracker only receives events
	if !strings.Contains(csp, "script-src 'self' https://umami.example.com;") {
		t.Errorf("script-src: %q", csp)
	}
	if !strings.Contains(csp, "connect-src 'self' https://umami.example.com https://rustrak.example.com;") {
		t.Errorf("connect-src: %q", csp)
	}
	for _, bad := range []string{"unsafe-eval", "unsafe-inline'; img", "*", "default-src 'self' http"} {
		if strings.Contains(strings.Replace(csp, "style-src-attr 'unsafe-inline'", "", 1), bad) {
			t.Errorf("csp contains %q: %q", bad, csp)
		}
	}
	if !strings.Contains(csp, "default-src 'self';") || !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "object-src 'none'") {
		t.Errorf("the rest of the policy changed: %q", csp)
	}
}

func TestOnlyTheTrackerIsAllowedWhenUmamiIsOff(t *testing.T) {
	// Arrange
	r := newRig(t, rigOpts{web: httpapi.Options{Client: httpapi.ClientConfig{SentryDSN: rustrakDSN}}})

	// Act
	csp := r.do("GET", "/", "", "").Header.Get("Content-Security-Policy")

	// Assert: no extra script origin, only a connect origin
	if !strings.Contains(csp, "script-src 'self';") || !strings.Contains(csp, "connect-src 'self' https://rustrak.example.com;") {
		t.Errorf("csp = %q", csp)
	}
}

func TestAMisconfiguredClientIsRefusedAtStartup(t *testing.T) {
	for name, c := range map[string]httpapi.ClientConfig{
		"a DSN without a key":         {SentryDSN: "https://rustrak.example.com/7"},
		"a DSN without a project":     {SentryDSN: "https://abc@rustrak.example.com/"},
		"a DSN that is not a URL":     {SentryDSN: "not a dsn"},
		"a DSN with a script scheme":  {SentryDSN: "javascript://abc@x/1"},
		"Umami without a website id":  {UmamiURL: "https://umami.example.com"},
		"a website id without Umami":  {UmamiID: "site-1"},
		"an Umami URL with a path":    {UmamiURL: "https://umami.example.com/x", UmamiID: "s"},
		"an Umami URL that is a bare": {UmamiURL: "umami.example.com", UmamiID: "s"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := httpapi.NewServer(nil, httpapi.Options{Client: c})
			if err == nil {
				t.Fatal("accepted")
			}
			if strings.Contains(err.Error(), "abc@") {
				t.Errorf("the error leaks the DSN key: %v", err)
			}
		})
	}
}
