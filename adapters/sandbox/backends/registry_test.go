package backends_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/GhaziBenDahmane/harness/adapters/sandbox/backends"
	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/core/sandbox"
	"github.com/GhaziBenDahmane/harness/core/sandbox/sandboxtest"
)

func TestRegisteredBackendsAreSelectedBySandboxBackend(t *testing.T) {
	// Arrange
	fake := sandboxtest.NewBackend()
	backends.Register("registry-test", func(*config.Config) (sandbox.Backend, error) { return fake, nil })

	// Act
	got, err := backends.NewBackend(&config.Config{SandboxBackend: " Registry-Test "})

	// Assert
	if err != nil || got != sandbox.Backend(fake) {
		t.Fatalf("NewBackend = %v, %v", got, err)
	}
	if !slices.Contains(backends.Registered(), "registry-test") {
		t.Fatalf("Registered() = %v", backends.Registered())
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
	} {
		b, err := backends.NewBackend(&c.cfg)
		if (err != nil) != c.err || (b == nil) != c.nil_ {
			t.Errorf("%s: backend %v, err %v", c.name, b, err)
		}
	}
}

func TestAnUnknownBackendNamesTheRegisteredOnes(t *testing.T) {
	_, err := backends.NewBackend(&config.Config{SandboxBackend: "no-such-backend"})
	if !errors.Is(err, backends.ErrUnknownBackend) {
		t.Fatalf("err = %v", err)
	}
}

func TestRegisteringANameTwiceIsAProgrammingError(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	backends.Register("none", func(*config.Config) (sandbox.Backend, error) { return nil, nil })
}
