package config

import (
	"os"
	"path/filepath"
	"strings"
)

// SkillsPath is the user skills directory: SKILLS_DIR, else
// ~/.config/harness/skills ("" when the home directory is unknown).
func (c *Config) SkillsPath() string {
	if d := strings.TrimSpace(c.SkillsDir); d != "" {
		return d
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", "harness", "skills")
	}
	return ""
}
