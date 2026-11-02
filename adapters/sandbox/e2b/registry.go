package e2b

import (
	"errors"

	"github.com/7ar-chat/harness/adapters/sandbox/backends"
	"github.com/7ar-chat/harness/core/sandbox"
)

// Name is the name this backend registers under (SANDBOX_BACKEND=e2b).
const Name = "e2b"

// BackendFromSettings builds the E2B backend from the registry settings. It fails when the API key is empty.
func BackendFromSettings(s backends.E2BSettings) (sandbox.Backend, error) {
	if s.APIKey == "" {
		return nil, errors.New("SANDBOX_BACKEND=e2b needs an API key: set E2B_API_KEY or run `harness auth login e2b`")
	}
	b, err := New(OptionsFromSettings(s))
	if err != nil {
		return nil, err
	}
	return b, nil
}

// OptionsFromSettings maps the registry settings to the E2B options.
func OptionsFromSettings(s backends.E2BSettings) Options {
	return Options{
		APIKey: s.APIKey, Template: s.Template, Domain: s.Domain, TimeoutSeconds: s.TimeoutSeconds,
		EgressAllowlist: s.EgressAllowlist, EgressDenyCIDRs: s.EgressDenyCIDRs, Images: s.Images,
	}
}

func init() {
	backends.Register(Name, func(s backends.Settings) (sandbox.Backend, error) {
		return BackendFromSettings(s.E2B)
	})
}
