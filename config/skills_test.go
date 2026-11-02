package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/7ar-chat/harness/config"
)

func TestSkillsPathIsSKILLS_DIRElseTheUserConfigDirectory(t *testing.T) {
	// SKILLS_DIR wins; without it the user skills live in ~/.config/harness/skills.
	// Arrange
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	set, _ := config.Load()
	set.SkillsDir = "/data/skills"

	// Act
	unset, _ := config.Load()

	// Assert
	if got := set.SkillsPath(); got != "/data/skills" {
		t.Fatalf("SKILLS_DIR path %q", got)
	}
	if got, want := unset.SkillsPath(), filepath.Join(home, ".har", "skills"); got != want {
		t.Fatalf("default path %q, want %q", got, want)
	}
}
