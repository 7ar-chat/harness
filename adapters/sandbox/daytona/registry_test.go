package daytona_test

import (
	"testing"

	"github.com/7ar-chat/harness/adapters/sandbox/backends"
	"github.com/7ar-chat/harness/adapters/sandbox/daytona"
)

func TestDaytonaRegistersItselfAsABuiltin(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  backends.Settings
		err  bool
	}{
		{"with an API key", backends.Settings{Name: "daytona", Daytona: backends.DaytonaSettings{APIKey: "k"}}, false},
		{"without an API key", backends.Settings{Name: "daytona"}, true},
	} {
		// Act
		b, err := backends.NewBackend(c.cfg)
		// Assert
		if (err != nil) != c.err || (b == nil) != c.err {
			t.Errorf("%s: backend %v, err %v", c.name, b, err)
		}
	}
}

func TestBackendFromSettingsNamesTheMissingVariable(t *testing.T) {
	// Act
	_, err := daytona.BackendFromSettings(backends.DaytonaSettings{})
	// Assert
	if err == nil || err.Error() != "SANDBOX_BACKEND=daytona needs DAYTONA_API_KEY" {
		t.Errorf("got %v", err)
	}
}
