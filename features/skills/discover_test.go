package skills_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/7ar-chat/harness/features/skills"
)

func writeSkill(t *testing.T, base, name, description string) {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: " + description + "\n---\n\nText.\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverReadsUserThenProjectDirectoriesInPriorityOrder(t *testing.T) {
	// Arrange: the same skill in every directory; the last source in the documented order wins.
	home, repo := t.TempDir(), t.TempDir()
	for _, d := range []string{".har", ".agents", ".claude"} {
		writeSkill(t, filepath.Join(home, d, "skills"), "same", "user "+d)
		writeSkill(t, filepath.Join(repo, d, "skills"), "same", "project "+d)
		writeSkill(t, filepath.Join(repo, d, "skills"), "only"+d[1:], "only here")
	}
	writeSkill(t, filepath.Join(home, ".claude", "skills"), "userclaude", "from the user's Claude skills")
	reg := skills.NewRegistry(skillFS("same", "builtin", "B"), nil)

	// Act
	n := reg.Discover(skills.DiscoverOptions{Home: home, ForeignUserDirs: true, ProjectRoot: repo})
	same, _, err := reg.ByName(context.Background(), "same")

	// Assert
	if err != nil || same.Description != "project .claude" || same.Source != skills.SourceProject || !same.Untrusted() {
		t.Fatalf("skill %+v err %v", same, err)
	}
	if n != 3+3+3+1 {
		t.Fatalf("added %d", n)
	}
	for _, name := range []string{"onlyhar", "onlyagents", "onlyclaude", "userclaude"} {
		if !reg.Exists(context.Background(), name) {
			t.Errorf("%s not discovered", name)
		}
	}
	if s, _, _ := reg.ByName(context.Background(), "userclaude"); s.Untrusted() {
		t.Error("a user skill must be trusted")
	}
}

func TestDiscoverSkipsForeignUserAndProjectDirectoriesWhenNotAsked(t *testing.T) {
	// Arrange
	home, repo := t.TempDir(), t.TempDir()
	writeSkill(t, filepath.Join(home, ".claude", "skills"), "foreign", "x")
	writeSkill(t, filepath.Join(home, ".har", "skills"), "mine", "x")
	writeSkill(t, filepath.Join(repo, ".har", "skills"), "proj", "x")
	reg := skills.NewRegistry(nil, nil)

	// Act
	reg.Discover(skills.DiscoverOptions{Home: home})

	// Assert
	ctx := context.Background()
	if !reg.Exists(ctx, "mine") || reg.Exists(ctx, "foreign") || reg.Exists(ctx, "proj") {
		t.Fatalf("skills %+v", reg.List(ctx))
	}
}

func TestSkillsDirIsAnExtraUserDirectoryAndProjectSymlinksAreIgnored(t *testing.T) {
	// Arrange
	home, repo, extra, outside := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	writeSkill(t, extra, "extra", "x")
	writeSkill(t, outside, "linked", "x")
	linkDir := filepath.Join(repo, ".har", "skills")
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "linked"), filepath.Join(linkDir, "linked")); err != nil {
		t.Skip("no symlinks")
	}
	userLinks := filepath.Join(home, ".har", "skills")
	if err := os.MkdirAll(userLinks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "linked"), filepath.Join(userLinks, "userlinked")); err != nil {
		t.Fatal(err)
	}
	reg := skills.NewRegistry(nil, nil)

	// Act
	reg.Discover(skills.DiscoverOptions{Home: home, SkillsDir: extra, ProjectRoot: repo})

	// Assert
	ctx := context.Background()
	if !reg.Exists(ctx, "extra") {
		t.Error("SKILLS_DIR skills missing")
	}
	if reg.Exists(ctx, "linked") {
		t.Error("a project symlink must not be followed")
	}
}
