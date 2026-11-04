package product

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/engine/tools"
	"github.com/7ar-chat/harness/hosts/httpapi"
)

// portSandbox is a sandbox whose one port is an httptest server.
type portSandbox struct {
	*hostSandbox
	url     string
	headers map[string]string
}

func (p portSandbox) Endpoint(context.Context, int) (sandbox.Endpoint, error) {
	return sandbox.Endpoint{URL: p.url, Headers: p.headers}, nil
}

type oneSandbox struct {
	sandbox.Manager
	sb  sandbox.Sandbox
	err error
}

func (m oneSandbox) Get(context.Context, string) (sandbox.Sandbox, error) { return m.sb, m.err }
func (m oneSandbox) Acquire(context.Context, string, string) (sandbox.Sandbox, error) {
	return m.sb, m.err
}

func manifestIn(t *testing.T, h *hostSandbox, yaml string) {
	t.Helper()
	ctx := context.Background()
	_ = h.MkdirAll(ctx, RepoDir+"/.har")
	if err := h.WriteFile(ctx, RepoDir+"/"+ManifestPath, []byte(yaml)); err != nil {
		t.Fatal(err)
	}
}

func newPreview(sb sandbox.Sandbox, err error) *Preview {
	return &Preview{Key: []byte("test-key"), mgr: oneSandbox{sb: sb, err: err}}
}

func TestThePreviewAddressIsSignedPerThread(t *testing.T) {
	pv := newPreview(nil, nil)
	base := pv.Base("thread-1")
	id := strings.TrimSuffix(strings.TrimPrefix(base, httpapi.PreviewPrefix), "/")
	if th, ok := pv.threadOf(id); !ok || th != "thread-1" {
		t.Fatalf("own address rejected: %q %v", th, ok)
	}
	other := newPreview(nil, nil)
	other.Key = []byte("another")
	if _, ok := other.threadOf(id); ok {
		t.Error("an address signed with another key was accepted")
	}
	if _, ok := pv.threadOf("thread-2." + strings.SplitN(id, ".", 2)[1]); ok {
		t.Error("a signature was reused for another thread")
	}
	for _, bad := range []string{"", "nodot", ".x", "thread-1.", "thread-1.zzz"} {
		if _, ok := pv.threadOf(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestTheProxyStripsThePrefixAndKeepsTheVisitorsIdentityOut(t *testing.T) {
	// Arrange: the app echoes what it received and tries to set a cookie and to be framed by nobody.
	var gotPath, gotCookie, gotAuth, gotHost, gotToken string
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotCookie, gotAuth, gotHost, gotToken = r.URL.Path, r.Header.Get("Cookie"), r.Header.Get("Authorization"), r.Host, r.Header.Get("X-Token")
		w.Header().Set("Set-Cookie", "session=evil")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Location", "/login")
		_, _ = io.WriteString(w, "hello")
	}))
	defer app.Close()
	hs := newHostSandbox(t)
	manifestIn(t, hs, "run: x\nport: 8000\nbase: strip")
	pv := newPreview(portSandbox{hostSandbox: hs, url: app.URL, headers: map[string]string{"X-Token": "sbx"}}, nil)
	front := httptest.NewServer(pv)
	defer front.Close()

	// Act
	req, _ := http.NewRequest("GET", front.URL+pv.Base("t1")+"api/items?x=1", nil)
	req.Header.Set("Cookie", "harness_session=secret")
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// Assert
	if string(body) != "hello" || gotPath != "/api/items" {
		t.Errorf("body %q, path %q", body, gotPath)
	}
	if gotCookie != "" || gotAuth != "" {
		t.Errorf("identity leaked to the app: cookie=%q auth=%q", gotCookie, gotAuth)
	}
	if gotToken != "sbx" || !strings.HasPrefix(gotHost, "127.0.0.1") {
		t.Errorf("provider header %q, host %q", gotToken, gotHost)
	}
	if resp.Header.Get("Set-Cookie") != "" || resp.Header.Get("X-Frame-Options") != "" {
		t.Errorf("response headers: %v", resp.Header)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.HasPrefix(csp, "sandbox allow-scripts") || strings.Contains(csp, "allow-same-origin") {
		t.Errorf("csp = %q", csp)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("cors = %q", resp.Header.Get("Access-Control-Allow-Origin"))
	}
	if loc := resp.Header.Get("Location"); loc != pv.Base("t1")+"login" {
		t.Errorf("location = %q", loc)
	}
}

func TestAnAppThatServesItsOwnBaseKeepsThePrefix(t *testing.T) {
	var gotPath string
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gotPath = r.URL.Path }))
	defer app.Close()
	hs := newHostSandbox(t)
	manifestIn(t, hs, "run: x\nport: 5173\nbase: app")
	pv := newPreview(portSandbox{hostSandbox: hs, url: app.URL}, nil)
	front := httptest.NewServer(pv)
	defer front.Close()

	resp, err := http.Get(front.URL + pv.Base("t1") + "src/main.jsx")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if want := pv.Base("t1") + "src/main.jsx"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
}

func TestTheProxyRefusesForeignAddressesAndExplainsAMissingApp(t *testing.T) {
	pv := newPreview(nil, sandbox.ErrNoSandbox)
	front := httptest.NewServer(pv)
	defer front.Close()

	resp, _ := http.Get(front.URL + httpapi.PreviewPrefix + "t1.AAAAAAAAAAAAAAAAAAAAAA/")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("forged address: %d", resp.StatusCode)
	}
	resp, _ = http.Get(front.URL + pv.Base("t1"))
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "not running") {
		t.Errorf("no sandbox: %d %q", resp.StatusCode, body)
	}
	resp, _ = http.Get(front.URL + strings.TrimSuffix(pv.Base("t1"), "/"))
	resp.Body.Close()
	if resp.Request.URL.Path != pv.Base("t1") {
		t.Errorf("the bare address should redirect to the slash form, ended on %q", resp.Request.URL.Path)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestPreviewStartRunsTheManifestsCommandAndStopEndsIt(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed")
	}
	// Arrange: a real static server started from the manifest, in a real directory.
	ctx := context.Background()
	hs := newHostSandbox(t)
	port := freePort(t)
	manifestIn(t, hs, fmt.Sprintf("install: echo installing\nrun: python3 -m http.server %d --bind 127.0.0.1\nport: %d\nbase: strip", port, port))
	_ = hs.WriteFile(ctx, RepoDir+"/index.html", []byte("<h1>It works</h1>"))
	pv := newPreview(portSandbox{hostSandbox: hs, url: fmt.Sprintf("http://127.0.0.1:%d", port)}, nil)
	rc := tools.RunContext{ThreadID: "t1", Login: "alice"}
	t.Cleanup(func() { _, _ = pv.stop(ctx, rc) })

	// Act
	res, err := pv.start(ctx, rc)

	// Assert: it answers, the dependencies were installed first, and through the proxy the page is served.
	if err != nil {
		t.Fatal(err)
	}
	r := res.(previewResult)
	if r.State != "running" || r.URL != pv.Base("t1") || r.Port != port {
		t.Fatalf("result = %+v", r)
	}
	if !strings.Contains(pv.tail(ctx, hs), "installing") {
		t.Errorf("the install did not run: %q", pv.tail(ctx, hs))
	}
	front := httptest.NewServer(pv)
	defer front.Close()
	resp, err := http.Get(front.URL + pv.Base("t1"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "It works") {
		t.Errorf("proxied body %q", body)
	}

	// Act: restarting does not reinstall unchanged dependencies; stopping frees the port.
	if _, err := pv.start(ctx, rc); err != nil {
		t.Fatal(err)
	}
	if _, err := pv.stop(ctx, rc); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond); err != nil {
			return
		} else {
			c.Close()
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Error("the app still listens after preview_stop")
}

func TestPreviewStartReportsAMissingManifestAndACrashingApp(t *testing.T) {
	ctx := context.Background()
	rc := tools.RunContext{ThreadID: "t1", Login: "alice"}

	hs := newHostSandbox(t)
	pv := newPreview(portSandbox{hostSandbox: hs, url: "http://127.0.0.1:1"}, nil)
	res, _ := pv.start(ctx, rc)
	if r := res.(previewResult); r.State != "error" || !strings.Contains(r.Message, ManifestPath) {
		t.Errorf("missing manifest: %+v", r)
	}

	manifestIn(t, hs, fmt.Sprintf("run: echo boom >&2; exit 3\nport: %d", freePort(t)))
	res, _ = pv.start(ctx, rc)
	if r := res.(previewResult); r.State != "exited" || !strings.Contains(r.Log, "boom") {
		t.Errorf("crash: %+v", r)
	}

	noPorts := newPreview(hs, nil) // a sandbox without port access
	res, _ = noPorts.start(ctx, rc)
	if r := res.(previewResult); r.State != "unavailable" {
		t.Errorf("no ports: %+v", r)
	}
}
