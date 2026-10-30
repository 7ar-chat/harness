package opensandbox_test

import (
	"testing"

	"github.com/GhaziBenDahmane/harness/adapters/sandbox/backends"
	"github.com/GhaziBenDahmane/harness/adapters/sandbox/opensandbox"
)

var _ = opensandbox.BackendFromSettings // importing the package registers the backend

func TestOpenSandboxRegistersItselfAsABuiltin(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  backends.Settings
		nil_ bool
		err  bool
	}{
		{"opensandbox with a URL", backends.Settings{Name: "opensandbox", OpenSandbox: backends.OpenSandboxSettings{URL: "http://localhost:8080"}}, false, false},
		{"opensandbox without URL", backends.Settings{Name: "opensandbox"}, true, true},
	} {
		b, err := backends.NewBackend(c.cfg)
		if (err != nil) != c.err || (b == nil) != c.nil_ {
			t.Errorf("%s: backend %v, err %v", c.name, b, err)
		}
	}
}
