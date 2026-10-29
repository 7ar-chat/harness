package main

import (
	"os"
	"path/filepath"

	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/hosts/tui"
)

// tuiConfigFile is the user's TUI configuration inside the user directory.
const tuiConfigFile = "tui.json"

// tuiOptions finds the user's tui.json and the state directory. Only the user
// directory is read (never <project>/.har): a repository may not rebind the
// user's keys. HARNESS_NO_USER_DIR gives the defaults and no state.
func tuiOptions() tui.Options {
	dir := config.UserDir()
	if dir == "" {
		return tui.Options{}
	}
	data, _ := os.ReadFile(filepath.Join(dir, tuiConfigFile))
	return tui.Options{ConfigData: data, StateDir: dir}
}
