package config

import (
	"os"
	"path/filepath"
)

// DirName is the name of the configuration directory of a project
// (<repo>/.har/) and of the user (~/.har/). It is the only place the name is
// defined: every path below it derives from it.
const DirName = ".har"

// Files and folders inside a configuration directory.
const (
	ConfigFile       = "config.yml"
	PermissionsFile  = "permissions.yml"
	InstructionsFile = "instructions.md"
	MCPFile          = "mcp.json"
	SkillsFolder     = "skills"
	MemoryFolder     = "memory"
)

// UserDir is ~/.har ("" when the home directory is unknown).
func UserDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, DirName)
	}
	return ""
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
