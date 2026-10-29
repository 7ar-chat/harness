package httpserver_test

import (
	"errors"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"testing"

	httpserver "github.com/GhaziBenDahmane/harness/hosts/web"
)

func TestStaticTokenIsRequiredForTheAPIButNotForTheUIOrHealth(t *testing.T) {
	// Arrange
	r := newRig(t, rigOpts{web: httpserver.Options{Token: "s3cret", LocalUser: "me"}})
	cases := []struct {
		name, path string
		hdr        []string
		want       int
	}{
		{"no token", "/api/threads", nil, 401},
		{"wrong token", "/api/threads", []string{"Authorization", "Bearer nope"}, 401},
		{"wrong scheme", "/api/threads", []string{"Authorization", "Basic s3cret"}, 401},
		{"right token", "/api/threads", []string{"Authorization", "Bearer s3cret"}, 200},
		{"legacy route needs it too", "/threads", nil, 401},
		{"health is open", "/healthz", nil, 200},
		{"ui is open", "/", nil, 200},
		{"ui assets are open", "/ui/" + uiAssetsOf(t)[0], nil, 200},
	}

	for _, c := range cases {
		// Act
		resp := r.do("GET", c.path, "", "", c.hdr...)

		// Assert
		if resp.StatusCode != c.want {
			t.Errorf("%s: status %d, want %d", c.name, resp.StatusCode, c.want)
		}
	}
}

func TestDefaultAuthenticatorIsASingleLocalUser(t *testing.T) {
	r := newRig(t, rigOpts{rawAuth: true})
	// no X-User, no token: acts as "local"
	resp := r.do("POST", "/api/threads", "", `{}`)
	if resp.StatusCode != 201 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestACustomAuthenticatorDecidesWhoTheCallerIs(t *testing.T) {
	// Arrange
	auth := func(req *http.Request) (string, error) {
		if u := req.Header.Get("X-Proxy-User"); u != "" {
			return u, nil
		}
		return "", errors.New("anonymous")
	}
	r := newRig(t, rigOpts{web: httpserver.Options{Authenticate: auth}})

	// Act
	anon := r.do("GET", "/api/threads", "", "")
	created := r.do("POST", "/api/threads", "", `{}`, "X-Proxy-User", "bob")

	// Assert
	if anon.StatusCode != 401 || created.StatusCode != 201 {
		t.Fatalf("anonymous %d, bob %d", anon.StatusCode, created.StatusCode)
	}
}

func TestCORSAllowsOnlyListedOrigins(t *testing.T) {
	// Arrange
	r := newRig(t, rigOpts{web: httpserver.Options{
		Token: "t", AllowedOrigins: []string{"https://app.example.com"},
	}})

	// Act
	preflight := r.do("OPTIONS", "/api/threads", "", "", "Origin", "https://app.example.com",
		"Access-Control-Request-Method", "POST", "Access-Control-Request-Headers", "authorization")
	allowed := r.do("GET", "/api/threads", "", "", "Origin", "https://app.example.com", "Authorization", "Bearer t")
	foreign := r.do("GET", "/api/threads", "", "", "Origin", "https://evil.example", "Authorization", "Bearer t")
	foreignPreflight := r.do("OPTIONS", "/api/threads", "", "", "Origin", "https://evil.example")
	sameOrigin := r.do("GET", "/api/threads", "", "", "Origin", r.srv.URL, "Authorization", "Bearer t")

	// Assert
	if preflight.StatusCode != 204 || preflight.Header.Get("Access-Control-Allow-Origin") != "https://app.example.com" ||
		!strings.Contains(preflight.Header.Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Fatalf("preflight %d %v", preflight.StatusCode, preflight.Header)
	}
	if allowed.StatusCode != 200 || allowed.Header.Get("Access-Control-Allow-Origin") != "https://app.example.com" {
		t.Fatalf("allowed origin: %d %v", allowed.StatusCode, allowed.Header)
	}
	if foreign.StatusCode != 403 || foreign.Header.Get("Access-Control-Allow-Origin") != "" || foreignPreflight.StatusCode != 403 {
		t.Fatalf("foreign origin: %d / preflight %d", foreign.StatusCode, foreignPreflight.StatusCode)
	}
	if sameOrigin.StatusCode != 200 || sameOrigin.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("same origin: %d %v", sameOrigin.StatusCode, sameOrigin.Header)
	}
}

func TestWithoutAnAllowlistNoCrossOriginRequestGetsThrough(t *testing.T) {
	// A token-less local server must not be drivable by a web page on another origin.
	r := newRig(t, rigOpts{web: httpserver.Options{}})
	resp := r.do("POST", "/api/threads", "", `{}`, "Origin", "https://evil.example")
	if resp.StatusCode != 403 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestHostGuardRefusesUnknownHostNames(t *testing.T) {
	r := newRig(t, rigOpts{web: httpserver.Options{AllowedHosts: []string{"localhost", "127.0.0.1"}}})
	ok := r.do("GET", "/healthz", "", "") // httptest listens on 127.0.0.1
	req, _ := http.NewRequest("GET", r.srv.URL+"/healthz", nil)
	req.Host = "rebind.evil.example:8080"
	bad, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Body.Close()
	if ok.StatusCode != 200 || bad.StatusCode != 403 {
		t.Fatalf("127.0.0.1 %d, foreign host %d", ok.StatusCode, bad.StatusCode)
	}
}

func TestUIIsServedWithAStrictContentSecurityPolicy(t *testing.T) {
	r := newRig(t, rigOpts{})
	assets := uiAssetsOf(t)
	paths := []string{"/", "/api/models", "/healthz"}
	for _, a := range assets {
		paths = append(paths, "/ui/"+a)
	}
	for _, path := range paths {
		resp := r.do("GET", path, "alice", "")
		csp := resp.Header.Get("Content-Security-Policy")
		if resp.StatusCode != 200 || !strings.Contains(csp, "default-src 'self'") || strings.Contains(strings.Replace(csp, "style-src-attr 'unsafe-inline'", "", 1), "unsafe-") ||
			resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: status %d csp %q", path, resp.StatusCode, csp)
		}
	}
	index := r.do("GET", "/", "", "")
	var js, css *http.Response
	for _, a := range assets {
		switch {
		case strings.HasSuffix(a, ".js"):
			js = r.do("GET", "/ui/"+a, "", "")
		case strings.HasSuffix(a, ".css"):
			css = r.do("GET", "/ui/"+a, "", "")
		}
	}
	if js == nil || css == nil {
		t.Fatalf("index.html references %v: want a script and a stylesheet", assets)
	}
	if !strings.HasPrefix(index.Header.Get("Content-Type"), "text/html") ||
		!strings.Contains(js.Header.Get("Content-Type"), "javascript") || !strings.HasPrefix(css.Header.Get("Content-Type"), "text/css") {
		t.Fatalf("types: %q %q %q", index.Header.Get("Content-Type"), js.Header.Get("Content-Type"), css.Header.Get("Content-Type"))
	}
	if r.do("GET", "/ui/missing.js", "", "").StatusCode != 404 || r.do("GET", "/nope", "", "").StatusCode != 404 {
		t.Fatal("unknown paths must be 404")
	}
}

func TestNoUIOptionServesOnlyTheAPI(t *testing.T) {
	r := newRig(t, rigOpts{web: httpserver.Options{NoUI: true}})
	if r.do("GET", "/", "alice", "").StatusCode != 404 || r.do("GET", "/healthz", "alice", "").StatusCode != 200 {
		t.Fatal("NoUI must drop / and keep the API")
	}
}

// uiAssetsOf lists the built files the embedded index.html loads, read from the embedded FS.
func uiAssetsOf(t *testing.T) []string {
	t.Helper()
	b, err := fs.ReadFile(httpserver.UI(), "index.html")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range regexp.MustCompile(`(?:src|href)="/ui/([^"]+)"`).FindAllStringSubmatch(string(b), -1) {
		out = append(out, m[1])
	}
	if len(out) == 0 {
		t.Fatal("index.html references no built assets: run `make web`")
	}
	return out
}
