package opensandbox_test

import (
	"github.com/GhaziBenDahmane/harness/adapters/sandbox/opensandbox"
	"testing"
)

func TestSandboxesAlwaysGetANetworkPolicy(t *testing.T) {
	// Allowlist mode denies by default; unrestricted mode allows but denies private/link-local CIDRs (other sandboxes, host, metadata).
	// Arrange
	allow := opensandbox.OpenSandboxOptions{EgressAllowlist: []string{"pypi.org", "mirror.example.com"}}
	open := opensandbox.OpenSandboxOptions{EgressDenyCIDRs: []string{"10.0.0.0/8", "169.254.0.0/16"}}
	none := opensandbox.OpenSandboxOptions{}

	// Act
	pa, po, pn := opensandbox.EgressPolicy(allow), opensandbox.EgressPolicy(open), opensandbox.EgressPolicy(none)

	// Assert
	if pa == nil || pa.DefaultAction != "deny" || len(pa.Egress) != 2 || pa.Egress[1].Action != "allow" || pa.Egress[1].Target != "mirror.example.com" {
		t.Fatalf("allowlist policy %+v", pa)
	}
	if po == nil || po.DefaultAction != "allow" || len(po.Egress) != 2 || po.Egress[0].Action != "deny" || po.Egress[1].Target != "169.254.0.0/16" {
		t.Fatalf("unrestricted policy %+v", po)
	}
	if pn == nil || pn.DefaultAction != "allow" || len(pn.Egress) != len(opensandbox.DefaultEgressDenyCIDRs) {
		t.Fatalf("default unrestricted policy %+v", pn)
	}
}
