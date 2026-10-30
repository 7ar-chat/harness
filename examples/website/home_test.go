package main

import (
	"os"
	"testing"
)

// TestMain keeps the tests independent of the developer's own ~/.har: config.Load
// reads <home>/.har/config.yml, where e.g. `sandbox.backend: local` changes how
// sandboxes are created. The home directory is an empty temporary one.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "harness-website-test-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	os.Unsetenv("HAR_HOME")
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
