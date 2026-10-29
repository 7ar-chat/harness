package main

import (
	"os"
	"path/filepath"

	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/hosts/tui"
)

// Files of the terminal UI inside the user directory.
const (
	tuiConfigFile = "tui.json"
	tuiStateFile  = "tui-state.json"
)

// tuiRunOptions finds the user's tui.json and the state file. Only the user
// directory is read (never <project>/.har): a repository may not rebind the
// user's keys. HARNESS_NO_USER_DIR gives the defaults and no state file.
func tuiRunOptions() tui.RunOptions {
	dir := config.UserDir()
	if dir == "" {
		return tui.RunOptions{}
	}
	data, _ := os.ReadFile(filepath.Join(dir, tuiConfigFile))
	return tui.RunOptions{ConfigData: data, StatePath: filepath.Join(dir, tuiStateFile)}
}
