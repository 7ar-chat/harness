package netguard

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
)

func TestBlocked(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "192.168.0.1", "172.16.5.5", "169.254.169.254", "100.64.0.1", "::1", "fe80::1", "fc00::1", "::ffff:127.0.0.1", "0.0.0.0", "224.0.0.1", "64:ff9b::7f00:1"} {
		if !Blocked(netip.MustParseAddr(s)) {
			t.Errorf("%s must be blocked", s)
		}
	}
	for _, s := range []string{"93.184.216.34", "8.8.8.8", "2606:4700:4700::1111"} {
		if Blocked(netip.MustParseAddr(s)) {
			t.Errorf("%s must be allowed", s)
		}
	}
}

// A public name that resolves to a public address, whose server redirects to a private one: the redirect is refused
// at dial time, and a name that resolves to a private address is refused outright (DNS pointing inward).
func TestGuardRefusesRedirectsAndNamesThatResolveInward(t *testing.T) {
	var inner atomic.Int32
	inside := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { inner.Add(1) }))
	defer inside.Close()
	outside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, inside.URL, http.StatusFound)
	}))
	defer outside.Close()
	real, _ := net.ResolveTCPAddr("tcp", strings.TrimPrefix(outside.URL, "http://"))

	g := &guard{
		lookup: func(_ context.Context, host string) ([]netip.Addr, error) {
			switch host {
			case "public.example":
				return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
			case "rebind.example":
				return []netip.Addr{netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("10.0.0.7")}, nil
			}
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
		dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, real.String())
		},
	}
	c := &http.Client{Transport: safeTransport(g), CheckRedirect: SafeCheckRedirect}
	resp, err := c.Get("http://public.example/")
	if err == nil {
		resp.Body.Close()
		t.Fatal("the redirect to a private address must fail")
	}
	if inner.Load() != 0 {
		t.Fatal("the private server was reached")
	}
	for _, host := range []string{"rebind.example", "other.example"} {
		if resp, err := c.Get("http://" + host + "/"); err == nil {
			resp.Body.Close()
			t.Fatalf("%s resolves inward and must be refused", host)
		}
	}
}

func TestRedirectLimits(t *testing.T) {
	via := make([]*http.Request, 4)
	if SafeCheckRedirect(&http.Request{}, via) == nil {
		t.Fatal("too many redirects")
	}
	https, _ := http.NewRequest("GET", "https://a/", nil)
	plain, _ := http.NewRequest("GET", "http://b/", nil)
	if SafeCheckRedirect(plain, []*http.Request{https}) == nil {
		t.Fatal("downgrade to http")
	}
}
