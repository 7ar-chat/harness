package config

import (
	"path/filepath"
	"strings"
)

// SkillsPath is the user skills directory: SKILLS_DIR, else ~/.har/skills
// ("" when the home directory is unknown).
func (c *Config) SkillsPath() string {
	if d := strings.TrimSpace(c.SkillsDir); d != "" {
		return d
	}
	if u := UserDir(); u != "" {
		return filepath.Join(u, SkillsFolder)
	}
	return ""
}
