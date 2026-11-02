package config_test

import (
	"strings"
	"testing"
)

const e2bAuth = `{"providers": {"e2b": {"api_key": "e2b_from_auth_json"}, "daytona": {"api_key": "dt_from_auth_json"}}}`

func TestHostedSandboxKeysComeFromTheEnvironmentFirstThenAuthJSON(t *testing.T) {
	cases := []struct {
		name           string
		auth           string
		environ        []string
		wantE2B, wantD string
	}{
		{"auth.json alone", e2bAuth, nil, "e2b_from_auth_json", "dt_from_auth_json"},
		{"the environment wins", e2bAuth, []string{"E2B_API_KEY=e2b_env", "DAYTONA_API_KEY=dt_env"}, "e2b_env", "dt_env"},
		{"only one in the environment", e2bAuth, []string{"E2B_API_KEY=e2b_env"}, "e2b_env", "dt_from_auth_json"},
		{"nothing anywhere", "", nil, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := keyOf(t, "", "", tc.auth, tc.environ...)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.E2BAPIKey != tc.wantE2B || cfg.DaytonaAPIKey != tc.wantD {
				t.Fatalf("E2B %q (want %q), Daytona %q (want %q)", cfg.E2BAPIKey, tc.wantE2B, cfg.DaytonaAPIKey, tc.wantD)
			}
		})
	}
}

func TestSelectingE2BNamesTheBackendAndReadsItsSettings(t *testing.T) {
	cfg, err := keyOf(t, "", "", e2bAuth, "SANDBOX_BACKEND=E2B", "E2B_TEMPLATE=mine", "E2B_DOMAIN=e2b.example")
	if err != nil {
		t.Fatal(err)
	}

	if cfg.SandboxBackendName() != "e2b" || !cfg.RemoteSandbox() || cfg.E2BTemplate != "mine" || cfg.E2BDomain != "e2b.example" {
		t.Fatalf("backend %q remote %v template %q domain %q", cfg.SandboxBackendName(), cfg.RemoteSandbox(), cfg.E2BTemplate, cfg.E2BDomain)
	}
}

func TestProductionHostedBackendsNeedAnEgressAllowlistLikeOpenSandbox(t *testing.T) {
	for _, backend := range []string{"e2b", "daytona"} {
		t.Run(backend, func(t *testing.T) {
			open, _ := keyOf(t, "", "", "", "SANDBOX_BACKEND="+backend, "HARNESS_PRODUCTION=1")
			listed, _ := keyOf(t, "", "", "", "SANDBOX_BACKEND="+backend, "HARNESS_PRODUCTION=1", "SANDBOX_EGRESS_ALLOWLIST=pypi.org")

			_, errOpen := open.EgressMode()
			mode, errListed := listed.EgressMode()

			if errOpen == nil || !strings.Contains(errOpen.Error(), "SANDBOX_EGRESS_ALLOWLIST") || errListed != nil || mode != "allowlist" {
				t.Fatalf("open: %v; listed: %q %v", errOpen, mode, errListed)
			}
		})
	}
}

func TestALocalSandboxKeepsItsOwnIsolationAndNeedsNoEgressList(t *testing.T) {
	cfg, _ := keyOf(t, "", "", "", "SANDBOX_BACKEND=local", "HARNESS_PRODUCTION=1")

	mode, err := cfg.EgressMode()

	if err != nil || mode != "disabled" || cfg.RemoteSandbox() {
		t.Fatalf("local: %q %v remote=%v", mode, err, cfg.RemoteSandbox())
	}
}

func TestSandboxImagesMapAProfileToAnImageAndTheProfileDefaultsToWork(t *testing.T) {
	cfg, err := keyOf(t, "", "", "", "SANDBOX_IMAGES= Work = harness-office , coding=base ")
	if err != nil {
		t.Fatal(err)
	}

	if cfg.SandboxImages["work"] != "harness-office" || cfg.SandboxImages["coding"] != "base" || len(cfg.SandboxImages) != 2 {
		t.Fatalf("images %v", cfg.SandboxImages)
	}
	if cfg.SandboxProfile() != "work" {
		t.Fatalf("profile %q, want work when AGENT_PROFILE is unset", cfg.SandboxProfile())
	}
	coding, _ := keyOf(t, "", "", "", "AGENT_PROFILE=Coding")
	if coding.SandboxProfile() != "coding" || len(coding.SandboxImages) != 0 {
		t.Fatalf("profile %q images %v", coding.SandboxProfile(), coding.SandboxImages)
	}
}

func TestSandboxImagesFromConfigYml(t *testing.T) {
	cfg, err := keyOf(t, "", "sandbox:\n  images:\n    work: harness-office\n    coding: base\n", "")
	if err != nil {
		t.Fatal(err)
	}

	if cfg.SandboxImages["work"] != "harness-office" || cfg.SandboxImages["coding"] != "base" {
		t.Fatalf("images %v", cfg.SandboxImages)
	}
}

func TestSandboxImagesRefusesAnEntryWithoutAnImage(t *testing.T) {
	for _, bad := range []string{"work", "work=", "=img"} {
		if _, err := keyOf(t, "", "", "", "SANDBOX_IMAGES="+bad); err == nil || !strings.Contains(err.Error(), "SANDBOX_IMAGES") {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
}
