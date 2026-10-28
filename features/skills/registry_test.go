package skills_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/GhaziBenDahmane/harness/features/skills"
	"github.com/GhaziBenDahmane/harness/store"
)

func skillFS(name, description, body string) fstest.MapFS {
	return fstest.MapFS{name + "/SKILL.md": {Data: []byte("---\nname: " + name + "\ndescription: " + description + "\n---\n\n" + body + "\n")}}
}

type runSkills map[string][]string

func (r runSkills) RunSkills(_ context.Context, runID string) ([]string, error) {
	return r[runID], nil
}

func TestLaterSourcesOverrideEarlierOnesByName(t *testing.T) {
	// A user skill named like a built-in replaces it; the registry lists each name once, sorted, with its source.
	// Arrange
	reg := skills.NewRegistry(skillFS("alpha", "Builtin alpha.", "A"), nil)
	reg.AddFS(skills.SourceUser, skillFS("alpha", "User alpha.", "U"))
	reg.AddFS(skills.SourceUser, skillFS("beta", "User beta.", "B"))

	// Act
	list := reg.List(context.Background())

	// Assert
	if len(list) != 2 || list[0].Name != "alpha" || list[0].Source != skills.SourceUser || list[0].Description != "User alpha." ||
		list[1].Name != "beta" || list[1].Source != skills.SourceUser {
		t.Fatalf("list %+v", list)
	}
}

func TestAddDirReadsSkillFoldersFromTheHostAndIgnoresAMissingDirectory(t *testing.T) {
	// A host directory with <name>/SKILL.md folders is a skill source; a folder without SKILL.md or a missing directory adds nothing.
	// Arrange
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "notes", "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "notes", "SKILL.md"), []byte("---\nname: notes\ndescription: My notes.\n---\n\nText.\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "notes", "scripts", "x.py"), []byte("print(1)\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, "not-a-skill"), 0o755)
	reg := skills.NewRegistry(nil, nil)

	// Act
	n := reg.AddDir(skills.SourceUser, dir)
	missing := reg.AddDir(skills.SourceUser, filepath.Join(dir, "nope"))
	s, pkg, err := reg.ByName(context.Background(), "notes")

	// Assert
	if n != 1 || missing != 0 || err != nil || s.Source != skills.SourceUser || len(pkg.Files) != 2 {
		t.Fatalf("added %d/%d, err %v, skill %+v, files %d", n, missing, err, s, len(pkg.Files))
	}
}

func TestInvalidOrMisnamedSkillFoldersAreSkipped(t *testing.T) {
	// A folder whose frontmatter name differs from the folder name, or without a description, is not registered.
	// Arrange
	fsys := fstest.MapFS{
		"wrong-dir/SKILL.md": {Data: []byte("---\nname: other\ndescription: Mismatch.\n---\n\nX.\n")},
		"empty/SKILL.md":     {Data: []byte("---\nname: empty\n---\n\nX.\n")},
	}
	reg := skills.NewRegistry(fsys, nil)

	// Act
	list := reg.List(context.Background())

	// Assert
	if len(list) != 0 {
		t.Fatalf("list %+v", list)
	}
}

func TestPlanAttachesBuiltinsByDefaultAndRequestedSkillsExplicitly(t *testing.T) {
	// Built-ins are attached by default; a skill requested for the run (by name, any source) is attached explicitly; unknown names are ignored; project skills are untrusted.
	// Arrange
	reg := skills.NewRegistry(skillFS("alpha", "Builtin alpha.", "A"), runSkills{"run-1": {"beta", "ghost"}})
	reg.AddFS(skills.SourceProject, skillFS("beta", "Project beta.", "B"))

	// Act
	plan, err := reg.Plan(context.Background(), &store.Run{ID: "run-1"})

	// Assert
	if err != nil || len(plan.Attached) != 2 || len(plan.Catalog) != 2 {
		t.Fatalf("plan %+v err %v", plan, err)
	}
	a, b := plan.Attached[0], plan.Attached[1]
	if a.Skill.Name != "alpha" || a.Explicit || a.Reason != skills.AttachDefault ||
		b.Skill.Name != "beta" || !b.Explicit || b.Reason != skills.AttachRequested || !b.Skill.Untrusted() {
		t.Fatalf("attached %+v", plan.Attached)
	}
}
