package config_test

import (
	"strings"
	"testing"

	"github.com/7ar-chat/harness/config"
)

func TestLoadRefusesMalformedEgressEntries(t *testing.T) {
	// Startup fails on a deny CIDR that does not parse or an allowlist entry that is not a host, IP or CIDR (C-19).
	// Arrange
	t.Setenv("SANDBOX_EGRESS_DENY_CIDRS", "10.0.0.0/8,10.0.0/8")
	_, badCIDR := config.Load()
	t.Setenv("SANDBOX_EGRESS_DENY_CIDRS", "10.0.0.0/8,fd00:ec2::254/128")
	t.Setenv("SANDBOX_EGRESS_ALLOWLIST", "https://pypi.org/simple")
	_, badHost := config.Load()
	t.Setenv("SANDBOX_EGRESS_ALLOWLIST", "pypi.org,*.pythonhosted.org,203.0.113.7,198.51.100.0/24")

	// Act
	cfg, okErr := config.Load()

	// Assert
	if badCIDR == nil || !strings.Contains(badCIDR.Error(), "10.0.0/8") || badHost == nil || !strings.Contains(badHost.Error(), "https://pypi.org/simple") {
		t.Fatalf("bad cidr %v, bad host %v", badCIDR, badHost)
	}
	if okErr != nil || len(cfg.SandboxEgressAllowlist) != 4 || len(cfg.SandboxEgressDenyCIDRs) != 2 {
		t.Fatalf("valid entries refused: %v %+v", okErr, cfg)
	}
}
