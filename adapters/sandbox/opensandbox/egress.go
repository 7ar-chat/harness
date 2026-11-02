package opensandbox

import (
	"errors"

	"github.com/7ar-chat/harness/adapters/sandbox/backends"
	"github.com/7ar-chat/harness/core/sandbox"
)

// Name is the name this backend registers under (SANDBOX_BACKEND=opensandbox).
const Name = "opensandbox"

// BackendFromSettings builds the OpenSandbox backend from the registry settings:
// every process that can create a sandbox (agent tools, Office previews,
// restores) gets the same options and egress policy. It fails when the
// server URL is empty. Refusing unrestricted egress in production is the
// composition root's check, made before the backend is built.
func BackendFromSettings(s backends.OpenSandboxSettings) (sandbox.Backend, error) {
	if s.URL == "" {
		return nil, errors.New("SANDBOX_BACKEND=opensandbox needs OPENSANDBOX_URL")
	}
	b, err := NewOpenSandbox(OptionsFromSettings(s))
	if err != nil {
		return nil, err
	}
	return b, nil
}

// OptionsFromSettings maps the registry settings to the OpenSandbox options.
func OptionsFromSettings(s backends.OpenSandboxSettings) OpenSandboxOptions {
	return OpenSandboxOptions{
		URL: s.URL, APIKey: s.APIKey, UseServerProxy: s.UseServerProxy,
		Image: s.Image, CPU: s.CPU, Memory: s.Memory,
		TimeoutSeconds: s.TimeoutSeconds, EgressAllowlist: s.EgressAllowlist,
		EgressDenyCIDRs: s.EgressDenyCIDRs, CredentialProxy: s.CredentialProxy,
	}
}

func init() {
	backends.Register(Name, func(s backends.Settings) (sandbox.Backend, error) {
		return BackendFromSettings(s.OpenSandbox)
	})
}
