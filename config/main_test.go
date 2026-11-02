package config_test

import (
	"os"
	"testing"
)

// TestMain keeps the tests independent of the developer's own ~/.har: the home
// directory is an empty temporary one.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "harness-config-test-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
