package daytona

import (
	"errors"

	"github.com/7ar-chat/harness/adapters/sandbox/backends"
	"github.com/7ar-chat/harness/core/sandbox"
)

// Name is the name this backend registers under (SANDBOX_BACKEND=daytona).
const Name = "daytona"

// BackendFromSettings builds the Daytona backend from the registry settings. It fails when the API key is empty.
// Refusing unrestricted egress in production is the composition root's check, made before the backend is built.
func BackendFromSettings(s backends.DaytonaSettings) (sandbox.Backend, error) {
	if s.APIKey == "" {
		return nil, errors.New("SANDBOX_BACKEND=daytona needs DAYTONA_API_KEY")
	}
	b, err := New(OptionsFromSettings(s))
	if err != nil {
		return nil, err
	}
	return b, nil
}

// OptionsFromSettings maps the registry settings to the Daytona options.
func OptionsFromSettings(s backends.DaytonaSettings) Options {
	return Options{
		APIKey: s.APIKey, URL: s.URL, Target: s.Target, Snapshot: s.Snapshot,
		TimeoutSeconds: s.TimeoutSeconds, EgressAllowlist: s.EgressAllowlist, EgressDenyCIDRs: s.EgressDenyCIDRs, Images: s.Images,
	}
}

func init() {
	backends.Register(Name, func(s backends.Settings) (sandbox.Backend, error) {
		return BackendFromSettings(s.Daytona)
	})
}
