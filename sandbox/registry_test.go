package sandbox_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/sandbox"
	"github.com/GhaziBenDahmane/harness/sandbox/sandboxtest"
)

func TestRegisteredBackendsAreSelectedBySandboxBackend(t *testing.T) {
	// Arrange
	fake := sandboxtest.NewBackend()
	sandbox.Register("registry-test", func(*config.Config) (sandbox.Backend, error) { return fake, nil })

	// Act
	got, err := sandbox.NewBackend(&config.Config{SandboxBackend: " Registry-Test "})

	// Assert
	if err != nil || got != sandbox.Backend(fake) {
		t.Fatalf("NewBackend = %v, %v", got, err)
	}
	if !slices.Contains(sandbox.Registered(), "registry-test") {
		t.Fatalf("Registered() = %v", sandbox.Registered())
	}
}

func TestBuiltinSelection(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  config.Config
		nil_ bool
		err  bool
	}{
		{"default is none", config.Config{}, true, false},
		{"none", config.Config{SandboxBackend: "none", OpenSandboxURL: "http://x"}, true, false},
		{"opensandbox URL alone", config.Config{OpenSandboxURL: "http://localhost:8080", SandboxEgressUnrestricted: true}, false, false},
		{"opensandbox without URL", config.Config{SandboxBackend: "opensandbox"}, true, true},
	} {
		b, err := sandbox.NewBackend(&c.cfg)
		if (err != nil) != c.err || (b == nil) != c.nil_ {
			t.Errorf("%s: backend %v, err %v", c.name, b, err)
		}
	}
}

func TestAnUnknownBackendNamesTheRegisteredOnes(t *testing.T) {
	_, err := sandbox.NewBackend(&config.Config{SandboxBackend: "no-such-backend"})
	if !errors.Is(err, sandbox.ErrUnknownBackend) {
		t.Fatalf("err = %v", err)
	}
}

func TestRegisteringANameTwiceIsAProgrammingError(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	sandbox.Register("none", func(*config.Config) (sandbox.Backend, error) { return nil, nil })
}
