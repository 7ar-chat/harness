package mcptools

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// errDestination is the one error a refused destination produces: it says nothing about which address class
// (loopback, private, metadata…) matched, so a caller cannot use the message to map the internal network.
var errDestination = errors.New("destination not allowed")

// maxRedirects bounds the redirects a guarded request follows.
const maxRedirects = 3

// Limits of the guarded transport. The response-header wait is above the tool call timeout: a server that answers a
// call as one JSON body sends its headers only when the result is ready.
const (
	safeDialTimeout           = 10 * time.Second
	safeTLSTimeout            = 10 * time.Second
	safeResponseHeaderTimeout = 150 * time.Second
	safeMaxResponseHeaderSize = 64 << 10
)

// blockedPrefixes are the ranges Is*() of net/netip does not already cover (or covers only in part).
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),          // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),      // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),       // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"),      // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),        // reserved, includes the broadcast address
	netip.MustParsePrefix("::/96"),              // IPv4-compatible
	netip.MustParsePrefix("64:ff9b::/96"),       // NAT64: reaches the embedded IPv4 address
	netip.MustParsePrefix("64:ff9b:1::/48"),     // local-use NAT64
	netip.MustParsePrefix("2002::/16"),          // 6to4
	netip.MustParsePrefix("2001::/32"),          // Teredo
	netip.MustParsePrefix("100::/64"),           // discard-only
	netip.MustParsePrefix("fc00::/7"),           // unique local
	netip.MustParsePrefix("fe80::/10"),          // link-local
	netip.MustParsePrefix("169.254.0.0/16"),     // link-local, cloud metadata
	netip.MustParsePrefix("10.0.0.0/8"),         // private
	netip.MustParsePrefix("172.16.0.0/12"),      // private
	netip.MustParsePrefix("192.168.0.0/16"),     // private
	netip.MustParsePrefix("255.255.255.255/32"), // broadcast
}

// blocked reports an address a guarded request must never connect to: anything that is not a plain public unicast
// address. An IPv4-mapped IPv6 address is judged as the IPv4 address it carries.
func blocked(a netip.Addr) bool {
	if !a.IsValid() {
		return true
	}
	a = a.WithZone("").Unmap()
	if a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// guard resolves and connects: it resolves the host once, refuses the connection when any resolved address is
// blocked, then dials the checked addresses themselves (never the name again), so a name that answers differently
// the next time (DNS rebinding) cannot lead to another address. The dialer's Control re-checks the address that is
// really connected to.
type guard struct {
	lookup func(ctx context.Context, host string) ([]netip.Addr, error)
	dial   func(ctx context.Context, network, address string) (net.Conn, error)
}

func newGuard() *guard {
	d := &net.Dialer{Timeout: safeDialTimeout, KeepAlive: 30 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return errDestination
		}
		a, err := netip.ParseAddr(host)
		if err != nil || blocked(a) {
			return errDestination
		}
		return nil
	}}
	return &guard{
		lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
		dial: d.DialContext,
	}
}

func (g *guard) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errDestination
	}
	var addrs []netip.Addr
	if a, perr := netip.ParseAddr(host); perr == nil {
		addrs = []netip.Addr{a}
	} else if addrs, err = g.lookup(ctx, host); err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, errDestination
	}
	for _, a := range addrs {
		if blocked(a) {
			return nil, errDestination
		}
	}
	var lastErr error
	for _, a := range addrs {
		c, err := g.dial(ctx, network, net.JoinHostPort(a.WithZone("").String(), port))
		if err == nil {
			return c, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, lastErr
}

// SafeTransport is the transport for a server a user supplied: it refuses to connect to a loopback, private,
// link-local (cloud metadata included), CGNAT, multicast or unspecified address, or to an IPv6 form that embeds an
// IPv4 one. It ignores the proxy environment (a proxy would connect for us and hide the destination from the check)
// and is never http.DefaultTransport.
func SafeTransport() *http.Transport { return safeTransport(newGuard()) }

func safeTransport(g *guard) *http.Transport {
	return &http.Transport{
		Proxy:                  nil,
		DialContext:            g.DialContext,
		ForceAttemptHTTP2:      true,
		TLSHandshakeTimeout:    safeTLSTimeout,
		ResponseHeaderTimeout:  safeResponseHeaderTimeout,
		MaxResponseHeaderBytes: safeMaxResponseHeaderSize,
		MaxIdleConns:           10,
		MaxIdleConnsPerHost:    2,
		IdleConnTimeout:        60 * time.Second,
	}
}

// SafeCheckRedirect is the http.Client.CheckRedirect of a guarded client: at most 3 redirects, never from https
// to http. The transport refuses a redirect to a blocked address when it dials.
func SafeCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > maxRedirects {
		return errors.New("too many redirects")
	}
	if len(via) > 0 && via[len(via)-1].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return errors.New("redirect to an insecure URL refused")
	}
	return nil
}
