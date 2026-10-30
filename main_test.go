package harness_test

import (
	"os"
	"testing"
)

// TestMain keeps the tests independent of the developer's own ~/.har,
// ~/.agents and ~/.claude (configuration, skills, instructions): the home
// directory is an empty temporary one.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "harness-test-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	os.Unsetenv("HAR_HOME")
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
