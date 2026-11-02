package config

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/7ar-chat/harness/core/layout"
)

// DirName is the name of the configuration directory of a project
// (<repo>/.har/) and of the user (~/.har/). It is the only place the name is
// defined (in core/layout, for packages that must not import config): every
// path below it derives from it.
const DirName = layout.DirName

// Files and folders inside a configuration directory.
const (
	ConfigFile       = "config.yml"
	PermissionsFile  = "permissions.yml"
	InstructionsFile = "instructions.md"
	MCPFile          = "mcp.json"
	SkillsFolder     = layout.SkillsFolder
	MemoryFolder     = layout.MemoryFolder
	AuthFile         = "auth.json"
	AgentsFile       = "AGENTS.md"
	SessionsFolder   = "sessions"
)

// Environment variables that place or disable the user directory.
const (
	// HomeEnv overrides the location of the user directory (default ~/.har):
	// for tests, containers and servers.
	HomeEnv = "HAR_HOME"
	// NoUserDirEnv disables reading anything from the user directory (any
	// non-empty value but 0/false/no/off): for servers, containers and CI that must be hermetic.
	NoUserDirEnv = "HARNESS_NO_USER_DIR"
)

// NoUserDir reports HARNESS_NO_USER_DIR: nothing is read from the user directory.
func NoUserDir() bool { return noUserDir(os.Getenv) }

func noUserDir(get func(string) string) bool {
	switch strings.ToLower(strings.TrimSpace(get(NoUserDirEnv))) {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}

// HomeDir is the user directory whether or not it is disabled: $HAR_HOME, else
// ~/.har ("" when the home directory is unknown). Commands that write to it
// (harness auth, harness config init) use it; everything that reads uses UserDir.
func HomeDir() string { return homeDir(os.Getenv) }

func homeDir(get func(string) string) string {
	if d := strings.TrimSpace(get(HomeEnv)); d != "" {
		return d
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, DirName)
	}
	return ""
}

// UserDir is the directory every user-level file is read from: $HAR_HOME, else
// ~/.har. It is "" when the home directory is unknown or HARNESS_NO_USER_DIR
// is set. All readers of the user directory go through it.
func UserDir() string { return userDirFrom(os.Getenv) }

func userDirFrom(get func(string) string) string {
	if noUserDir(get) {
		return ""
	}
	return homeDir(get)
}

// FindProjectDir walks up from start to the git root (the first directory
// holding a .git entry) or the filesystem root and returns the nearest
// <dir>/.har directory and the project root (its parent, or the git root when
// there is no .har). found is false when there is no .har directory. The user
// directory itself is never a project directory.
func FindProjectDir(start string) (dir, root string, found bool) {
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", "", false
	}
	user := UserDir()
	for d := abs; ; d = filepath.Dir(d) {
		cand := filepath.Join(d, DirName)
		if cand != user {
			if st, err := os.Stat(cand); err == nil && st.IsDir() {
				return cand, d, true
			}
		}
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return "", d, false
		}
		if filepath.Dir(d) == d {
			return "", abs, false
		}
	}
}
