package sandbox_test

import (
	"testing"

	"github.com/GhaziBenDahmane/harness/sandbox"
)

func TestSandboxesAlwaysGetANetworkPolicy(t *testing.T) {
	// Allowlist mode denies by default; unrestricted mode allows but denies private/link-local CIDRs (other sandboxes, host, metadata).
	// Arrange
	allow := sandbox.OpenSandboxOptions{EgressAllowlist: []string{"pypi.org", "artifactory.intramundi.com"}}
	open := sandbox.OpenSandboxOptions{EgressDenyCIDRs: []string{"10.0.0.0/8", "169.254.0.0/16"}}
	none := sandbox.OpenSandboxOptions{}

	// Act
	pa, po, pn := sandbox.EgressPolicy(allow), sandbox.EgressPolicy(open), sandbox.EgressPolicy(none)

	// Assert
	if pa == nil || pa.DefaultAction != "deny" || len(pa.Egress) != 2 || pa.Egress[1].Action != "allow" || pa.Egress[1].Target != "artifactory.intramundi.com" {
		t.Fatalf("allowlist policy %+v", pa)
	}
	if po == nil || po.DefaultAction != "allow" || len(po.Egress) != 2 || po.Egress[0].Action != "deny" || po.Egress[1].Target != "169.254.0.0/16" {
		t.Fatalf("unrestricted policy %+v", po)
	}
	if pn == nil || pn.DefaultAction != "allow" || len(pn.Egress) != len(sandbox.DefaultEgressDenyCIDRs) {
		t.Fatalf("default unrestricted policy %+v", pn)
	}
}
