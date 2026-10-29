package opensandbox_test

import (
	"github.com/GhaziBenDahmane/harness/adapters/sandbox/backends"
	"testing"

	"github.com/GhaziBenDahmane/harness/adapters/sandbox/opensandbox"
	"github.com/GhaziBenDahmane/harness/config"
)

var _ = opensandbox.BackendFromConfig // importing the package registers the backend

func TestOpenSandboxRegistersItselfAsABuiltin(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  config.Config
		nil_ bool
		err  bool
	}{
		{"opensandbox URL alone", config.Config{OpenSandboxURL: "http://localhost:8080", SandboxEgressUnrestricted: true}, false, false},
		{"opensandbox without URL", config.Config{SandboxBackend: "opensandbox"}, true, true},
	} {
		b, err := backends.NewBackend(&c.cfg)
		if (err != nil) != c.err || (b == nil) != c.nil_ {
			t.Errorf("%s: backend %v, err %v", c.name, b, err)
		}
	}
}
