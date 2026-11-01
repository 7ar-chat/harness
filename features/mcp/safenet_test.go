package mcptools

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/7ar-chat/harness/config"
)

func cfgFor(url string) config.MCPServer { return config.MCPServer{Name: "x", URL: url} }

func TestBlockedCoversEveryNonPublicRange(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1", true}, {"127.255.255.254", true}, {"::1", true},
		{"10.1.2.3", true}, {"172.16.0.1", true}, {"172.31.255.255", true}, {"192.168.1.1", true},
		{"fc00::1", true}, {"fd12:3456::1", true},
		{"169.254.169.254", true}, {"169.254.0.1", true}, {"fe80::1", true},
		{"0.0.0.0", true}, {"::", true}, {"0.1.2.3", true},
		{"224.0.0.1", true}, {"ff02::1", true}, {"ff05::2", true},
		{"255.255.255.255", true}, {"240.0.0.1", true},
		{"100.64.0.1", true}, {"100.127.255.255", true},
		{"::ffff:127.0.0.1", true}, {"::ffff:10.0.0.1", true}, {"::ffff:169.254.169.254", true}, {"::ffff:192.168.0.1", true},
		{"64:ff9b::7f00:1", true}, {"64:ff9b::a9fe:a9fe", true}, {"64:ff9b:1::1", true},
		{"2002:7f00:1::1", true}, {"2001:0:4136:e378:8000:63bf:3fff:fdd2", true},
		{"::7f00:1", true},
		{"fe80::1%eth0", true},
		// public addresses stay reachable, including the neighbours of the blocked ranges
		{"93.184.216.34", false}, {"8.8.8.8", false}, {"1.1.1.1", false},
		{"100.63.255.255", false}, {"100.128.0.1", false}, {"172.15.255.255", false}, {"172.32.0.1", false},
		{"2606:4700:4700::1111", false}, {"::ffff:8.8.8.8", false},
	}
	for _, c := range cases {
		a, err := netip.ParseAddr(c.addr)
		if err != nil {
			t.Fatalf("%s: %v", c.addr, err)
		}
		if got := blocked(a); got != c.want {
			t.Errorf("blocked(%s) = %v, want %v", c.addr, got, c.want)
		}
	}
	if !blocked(netip.Addr{}) {
		t.Error("the zero address must be blocked")
	}
}

// fakeNet is a guard whose names resolve as the test says and whose connections all go to one test server, so the
// checks run for real without the test needing a public address.
func fakeNet(t *testing.T, srv *httptest.Server, answers func(host string) []netip.Addr) (*http.Transport, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var dialed []string
	g := &guard{
		lookup: func(_ context.Context, host string) ([]netip.Addr, error) { return answers(host), nil },
		dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			mu.Lock()
			dialed = append(dialed, address)
			mu.Unlock()
			var d net.Dialer
			return d.DialContext(ctx, network, srv.Listener.Addr().String())
		},
	}
	tr := safeTransport(g)
	tr.DisableKeepAlives = true
	t.Cleanup(tr.CloseIdleConnections)
	return tr, &dialed
}

func get(tr http.RoundTripper, url string) (*http.Response, error) {
	c := &http.Client{Transport: tr, CheckRedirect: SafeCheckRedirect}
	return c.Get(url)
}

func TestSafeTransportRefusesALoopbackServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "secret") }))
	defer srv.Close()
	tr := SafeTransport()
	defer tr.CloseIdleConnections()

	resp, err := get(tr, srv.URL)

	if err == nil {
		resp.Body.Close()
		t.Fatal("a server on 127.0.0.1 was reachable")
	}
	if !strings.Contains(err.Error(), errDestination.Error()) {
		t.Fatalf("error %v", err)
	}
	for _, leak := range []string{"loopback", "private", "metadata", "link-local"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error leaks %q: %v", leak, err)
		}
	}
}

func TestSafeTransportIgnoresTheProxyEnvironment(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:9")
	if SafeTransport().Proxy != nil {
		t.Fatal("a proxy would connect on our behalf and hide the destination from the check")
	}
}

func TestAHostnameThatRebindsToAPrivateAddressIsRefusedAtConnect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") }))
	defer srv.Close()
	var mu sync.Mutex
	lookups := 0
	tr, dialed := fakeNet(t, srv, func(string) []netip.Addr {
		mu.Lock()
		defer mu.Unlock()
		lookups++
		if lookups == 1 {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}
		}
		return []netip.Addr{netip.MustParseAddr("169.254.169.254")}
	})

	first, err := get(tr, "http://rebind.example/")
	if err != nil {
		t.Fatalf("the public answer was refused: %v", err)
	}
	first.Body.Close()
	second, err := get(tr, "http://rebind.example/")

	if err == nil {
		second.Body.Close()
		t.Fatal("the private answer of the second lookup was connected to")
	}
	if len(*dialed) != 1 || !strings.HasPrefix((*dialed)[0], "93.184.216.34:") {
		t.Fatalf("dialled %v: the private address must never be dialled", *dialed)
	}
}

func TestAnAnswerMixingPublicAndPrivateAddressesIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	tr, dialed := fakeNet(t, srv, func(string) []netip.Addr {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("10.0.0.5")}
	})

	resp, err := get(tr, "http://mixed.example/")

	if err == nil {
		resp.Body.Close()
		t.Fatal("a name with a private address among its answers was connected to")
	}
	if len(*dialed) != 0 {
		t.Fatalf("dialled %v", *dialed)
	}
}

func TestARedirectToALoopbackAddressIsRefused(t *testing.T) {
	var hits sync.Map
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Store("internal", true)
		io.WriteString(w, "internal secret")
	}))
	defer internal.Close()
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL, http.StatusFound)
	}))
	defer public.Close()
	tr, _ := fakeNet(t, public, func(string) []netip.Addr { return []netip.Addr{netip.MustParseAddr("93.184.216.34")} })

	resp, err := get(tr, "http://public.example/")

	if err == nil {
		resp.Body.Close()
		t.Fatal("the redirect to 127.0.0.1 was followed")
	}
	if _, ok := hits.Load("internal"); ok {
		t.Fatal("the internal server received the request")
	}
}

func TestRedirectsAreLimitedAndNeverDowngradeToPlainHTTP(t *testing.T) {
	mk := func(scheme string, n int) (*http.Request, []*http.Request) {
		req := httptest.NewRequest("GET", scheme+"://x.example/", nil)
		via := make([]*http.Request, n)
		for i := range via {
			via[i] = httptest.NewRequest("GET", "https://x.example/", nil)
		}
		return req, via
	}
	req, via := mk("https", 3)
	if err := SafeCheckRedirect(req, via); err != nil {
		t.Errorf("the third redirect was refused: %v", err)
	}
	req, via = mk("https", 4)
	if err := SafeCheckRedirect(req, via); err == nil {
		t.Error("a fourth redirect was followed")
	}
	req, via = mk("http", 1)
	if err := SafeCheckRedirect(req, via); err == nil {
		t.Error("an https to http downgrade was followed")
	}
	plain := httptest.NewRequest("GET", "http://x.example/", nil)
	if err := SafeCheckRedirect(httptest.NewRequest("GET", "http://y.example/", nil), []*http.Request{plain}); err != nil {
		t.Errorf("http to http was refused: %v", err)
	}
}

func TestListToolsReportsRejectedCredentialsAndHidesTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad key sk-secret-body", http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := listTools(context.Background(), cfgFor(srv.URL), "t", 5e9, http.DefaultTransport)

	if !errors.Is(err, ErrRejected) {
		t.Fatalf("error %v", err)
	}
	if strings.Contains(err.Error(), "sk-secret-body") {
		t.Fatalf("the response body leaked: %v", err)
	}
}
