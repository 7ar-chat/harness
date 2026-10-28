package config

import (
	"fmt"
	"net/netip"
	"regexp"
	"strings"
)

// hostPattern is a DNS name, optionally with a leading "*." wildcard.
var hostPattern = regexp.MustCompile(`^(\*\.)?([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)*[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

// validateEgress refuses sandbox egress entries the provider would misread
// (stream A, C-19): every SANDBOX_EGRESS_DENY_CIDRS entry must be a CIDR or an
// IP, every SANDBOX_EGRESS_ALLOWLIST entry a host name ("*.x" allowed), an IP
// or a CIDR. A typo must stop the process, not silently open the network.
func (c *Config) validateEgress() error {
	for _, e := range c.SandboxEgressDenyCIDRs {
		if !isIPOrPrefix(e) {
			return fmt.Errorf("SANDBOX_EGRESS_DENY_CIDRS: %q is not a valid CIDR or IP address", e)
		}
	}
	for _, e := range c.SandboxEgressAllowlist {
		if !isIPOrPrefix(e) && (len(e) > 253 || !hostPattern.MatchString(e)) {
			return fmt.Errorf("SANDBOX_EGRESS_ALLOWLIST: %q is not a host name, IP address or CIDR (no scheme, port or path)", e)
		}
	}
	return nil
}

func isIPOrPrefix(s string) bool {
	if strings.Contains(s, "/") {
		_, err := netip.ParsePrefix(s)
		return err == nil
	}
	_, err := netip.ParseAddr(s)
	return err == nil
}
