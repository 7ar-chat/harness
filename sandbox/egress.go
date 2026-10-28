package sandbox

import (
	"github.com/GhaziBenDahmane/harness/config"
)

// BackendFromConfig builds the sandbox backend of a process (api, runtime or
// all) from the configuration: every process that can create a sandbox (agent
// tools, Office previews, restores) gets the same options and egress policy,
// and every one of them applies the production refusal of unrestricted egress
// (C-02: the api used to skip it). It returns a nil Backend when OPENSANDBOX_URL
// is empty.
func BackendFromConfig(cfg *config.Config) (Backend, error) {
	if !cfg.SandboxEnabled() {
		return nil, nil
	}
	if _, err := cfg.EgressMode(); err != nil {
		return nil, err
	}
	b, err := NewOpenSandbox(OptionsFromConfig(cfg))
	if err != nil {
		return nil, err
	}
	return b, nil
}

// OptionsFromConfig maps the configuration to the OpenSandbox options.
func OptionsFromConfig(cfg *config.Config) OpenSandboxOptions {
	return OpenSandboxOptions{
		URL: cfg.OpenSandboxURL, APIKey: cfg.OpenSandboxAPIKey, UseServerProxy: cfg.OpenSandboxUseServerProxy,
		Image: cfg.OpenSandboxImage, CPU: cfg.SandboxCPU, Memory: cfg.SandboxMemory,
		TimeoutSeconds: cfg.SandboxTimeoutSeconds, EgressAllowlist: cfg.SandboxEgressAllowlist,
		EgressDenyCIDRs: cfg.SandboxEgressDenyCIDRs,
	}
}
