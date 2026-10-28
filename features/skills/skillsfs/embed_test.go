package skillsfs_test

import (
	"io/fs"
	"os"
	"testing"

	skillsfs "github.com/GhaziBenDahmane/harness/features/skills/skillsfs"
)

func TestOnlySkillFoldersAreEmbedded(t *testing.T) {
	// Every builtin/<name>/ with a SKILL.md is embedded, and nothing else (e.g. builtin/tests) ships in the binary.
	// Arrange
	dirs, err := os.ReadDir("builtin")
	if err != nil {
		t.Fatal(err)
	}

	// Act
	embedded, err := fs.ReadDir(skillsfs.Builtin(), ".")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range embedded {
		got[e.Name()] = true
	}
	for _, d := range dirs {
		_, statErr := os.Stat("builtin/" + d.Name() + "/SKILL.md")
		isSkill := d.IsDir() && statErr == nil
		if isSkill != got[d.Name()] {
			t.Errorf("%s: skill=%v embedded=%v", d.Name(), isSkill, got[d.Name()])
		}
	}
}
