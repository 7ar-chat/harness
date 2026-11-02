package backends_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/adapters/sandbox/backends"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
)

func TestRegisteredBackendsAreSelectedBySandboxBackend(t *testing.T) {
	// Arrange
	fake := sandboxtest.NewBackend()
	backends.Register("registry-test", func(backends.Settings) (sandbox.Backend, error) { return fake, nil })

	// Act
	got, err := backends.NewBackend(backends.Settings{Name: " Registry-Test "})

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
		cfg  backends.Settings
		nil_ bool
		err  bool
	}{
		{"default is none", backends.Settings{}, true, false},
		{"none", backends.Settings{Name: "none", OpenSandbox: backends.OpenSandboxSettings{URL: "http://x"}}, true, false},
	} {
		b, err := backends.NewBackend(c.cfg)
		if (err != nil) != c.err || (b == nil) != c.nil_ {
			t.Errorf("%s: backend %v, err %v", c.name, b, err)
		}
	}
}

func TestAnUnknownBackendNamesTheRegisteredOnes(t *testing.T) {
	_, err := backends.NewBackend(backends.Settings{Name: "no-such-backend"})
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
	backends.Register("none", func(backends.Settings) (sandbox.Backend, error) { return nil, nil })
}

func TestRegisterRefusesAnEmptyNameOrANilFactory(t *testing.T) {
	cases := map[string]struct {
		name string
		f    backends.Factory
	}{
		"empty name":                 {"", func(backends.Settings) (sandbox.Backend, error) { return nil, nil }},
		"blank name":                 {"  ", func(backends.Settings) (sandbox.Backend, error) { return nil, nil }},
		"nil factory":                {"nil-factory-test", nil},
		"case-insensitive duplicate": {"NONE", func(backends.Settings) (sandbox.Backend, error) { return nil, nil }},
	}
	for name, c := range cases {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: no panic", name)
				}
			}()
			backends.Register(c.name, c.f)
		}()
	}
	if slices.Contains(backends.Registered(), "nil-factory-test") {
		t.Error("a refused registration was recorded")
	}
}

func TestAnUnknownOpenSandboxBackendExplainsHowToRegisterIt(t *testing.T) {
	// The opensandbox package registers itself on import; this test binary does not import it.
	if slices.Contains(backends.Registered(), "opensandbox") {
		t.Skip("opensandbox is linked into this binary")
	}

	// Act
	_, err := backends.NewBackend(backends.Settings{Name: "OpenSandbox"})

	// Assert
	if !errors.Is(err, backends.ErrUnknownBackend) || !strings.Contains(err.Error(), "adapters/sandbox/opensandbox") {
		t.Fatalf("err = %v", err)
	}
	if _, other := backends.NewBackend(backends.Settings{Name: "nope"}); strings.Contains(other.Error(), "adapters/sandbox/opensandbox") {
		t.Errorf("the opensandbox hint leaked into an unrelated error: %v", other)
	}
}

func TestRegisteredListsTheNamesSorted(t *testing.T) {
	// Arrange
	backends.Register("zz-sorted-test", func(backends.Settings) (sandbox.Backend, error) { return nil, nil })
	backends.Register("aa-sorted-test", func(backends.Settings) (sandbox.Backend, error) { return nil, nil })

	// Act
	got := backends.Registered()

	// Assert
	if !slices.IsSorted(got) || !slices.Contains(got, "aa-sorted-test") || !slices.Contains(got, "zz-sorted-test") || !slices.Contains(got, backends.None) {
		t.Fatalf("Registered() = %v", got)
	}
}
