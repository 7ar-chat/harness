package e2b

import (
	"net/netip"
	"strings"
)

// DefaultEgressDenyCIDRs keep a sandbox in unrestricted mode away from private networks, link-local addresses
// (including cloud metadata) and CGNAT. E2B's denyOut takes IPv4 CIDRs and addresses and rejects 0.0.0.0/8.
var DefaultEgressDenyCIDRs = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "100.64.0.0/10"}

// networkConfig is the "network" object of a new sandbox, never nil in effect: without it a sandbox can reach
// private ranges.
//   - allowlist mode: deny everything (0.0.0.0/0), allow the listed domains or addresses (allow wins over deny);
//   - unrestricted mode: allow by default but deny the private and link-local ranges.
func networkConfig(o Options) map[string]any {
	var allow []string
	for _, d := range o.EgressAllowlist {
		if d = strings.TrimSpace(d); d != "" {
			allow = append(allow, d)
		}
	}
	if len(allow) > 0 {
		return map[string]any{"allowOut": allow, "denyOut": []string{"0.0.0.0/0"}}
	}
	deny := supportedDenies(o.EgressDenyCIDRs)
	if len(deny) == 0 {
		deny = DefaultEgressDenyCIDRs
	}
	return map[string]any{"denyOut": deny}
}

// supportedDenies keeps what E2B's denyOut accepts from a configured list (the harness default also names IPv6
// ranges and 0.0.0.0/8, which E2B rejects with HTTP 400): IPv4 addresses and prefixes, except "this network".
func supportedDenies(in []string) []string {
	var out []string
	for _, e := range in {
		e = strings.TrimSpace(e)
		if p, err := netip.ParsePrefix(e); err == nil {
			if p.Addr().Is4() && p.Masked() != netip.MustParsePrefix("0.0.0.0/8") {
				out = append(out, e)
			}
			continue
		}
		if a, err := netip.ParseAddr(e); err == nil && a.Is4() {
			out = append(out, e)
		}
	}
	return out
}
