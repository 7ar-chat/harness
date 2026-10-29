package commands

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func write(t *testing.T, dir, rel, text string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func byName(s *Set, name string) (Command, bool) {
	for _, c := range s.List() {
		if c.Name == name {
			return c, true
		}
	}
	return Command{}, false
}

func TestProjectCommandsWinOverUserSkillAndBuiltinOnes(t *testing.T) {
	// Arrange
	user, project := t.TempDir(), t.TempDir()
	write(t, user, "review.md", "user review")
	write(t, project, "review.md", "project review")
	write(t, user, "deploy.md", "user deploy")
	write(t, project, "init.md", "project init")

	// Act
	s := Load(Options{UserDir: user, ProjectDir: project, Skills: []Skill{{Name: "deploy", Description: "skill"}, {Name: "pdf", Description: "make pdfs"}}})

	// Assert
	for name, want := range map[string]string{"review": SourceProject, "deploy": SourceUser, "init": SourceProject, "pdf": SourceSkill} {
		if c, ok := byName(s, name); !ok || c.Source != want {
			t.Errorf("%s = %+v, want source %s", name, c, want)
		}
	}
	if got, _ := s.Expand("review", ""); got != "project review" {
		t.Errorf("review expands to %q", got)
	}
}

func TestTheInitBuiltinIsListedWithoutAnyDirectory(t *testing.T) {
	// Act
	s := Load(Options{})

	// Assert
	list := s.List()
	if len(list) != 1 || list[0].Name != "init" || list[0].Source != SourceBuiltin {
		t.Fatalf("list = %+v", list)
	}
	if got, _ := s.Expand("init", ""); !strings.Contains(got, "AGENTS.md") {
		t.Errorf("init prompt = %q", got)
	}
}

func TestFrontmatterDescriptionHintAndNestedNames(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	write(t, dir, "git/commit.md", "---\ndescription: Commit well\nargument-hint: <scope>\n---\nWrite a commit for $1.\n")
	write(t, dir, "plain.md", "\nFirst line is the description\nmore")

	// Act
	s := Load(Options{ProjectDir: dir})

	// Assert
	c, ok := byName(s, "git:commit")
	if !ok || c.Description != "Commit well" || c.ArgHint != "<scope>" || c.Template != "Write a commit for $1." {
		t.Errorf("git:commit = %+v", c)
	}
	if p, _ := byName(s, "plain"); p.Description != "First line is the description" {
		t.Errorf("plain description = %q", p.Description)
	}
}

func TestBadFilesAreIgnoredWithAWarning(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	write(t, dir, "badyaml.md", "---\ndescription: [unclosed\n---\nbody")
	write(t, dir, "unclosed.md", "---\ndescription: x\nbody")
	write(t, dir, "empty.md", "---\ndescription: x\n---\n")
	write(t, dir, "bad name.md", "body")
	write(t, dir, "big.md", strings.Repeat("a", MaxFileBytes+1))
	write(t, dir, "good.md", "fine")
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "link.md")); err != nil {
		t.Skip("symlinks unavailable")
	}
	var warns []string

	// Act
	s := Load(Options{ProjectDir: dir, Warn: func(m string) { warns = append(warns, m) }})

	// Assert
	if len(s.List()) != 2 { // init and good
		t.Errorf("commands = %+v", s.List())
	}
	if len(warns) != 6 {
		t.Errorf("warnings = %q", warns)
	}
	if _, ok := byName(s, "link"); ok {
		t.Error("a symbolic link was followed")
	}
}

func TestExpandSubstitutesArgumentsAndPositionals(t *testing.T) {
	cases := []struct {
		name, template, args, want string
	}{
		{"arguments", "Do: $ARGUMENTS", "a  b", "Do: a  b"},
		{"positional", "$2 then $1", "x y", "y then x"},
		{"missing positional", "[$1][$2]", "x", "[x][]"},
		{"quoted", "$1|$2", `"two words" 'a b' c`, "two words|a b"},
		{"no placeholder appends", "Review the code", "src/main.go", "Review the code\n\nsrc/main.go"},
		{"no placeholder no args", "Review the code", "", "Review the code"},
		{"single pass", "$1 $2", "$2 x", "$2 x"},
		{"dollar ten is not a placeholder", "$10", "a", "a0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Act
			got := Expand(tc.template, tc.args)

			// Assert
			if got != tc.want {
				t.Errorf("Expand(%q, %q) = %q, want %q", tc.template, tc.args, got, tc.want)
			}
		})
	}
}

func TestExpandOfAnUnknownCommand(t *testing.T) {
	// Act
	_, err := Load(Options{}).Expand("nope", "")

	// Assert
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestFieldsGroupsQuotedWords(t *testing.T) {
	// Act
	got := Fields(`a "b c" '' d`)

	// Assert
	if want := []string{"a", "b c", "", "d"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Fields = %q, want %q", got, want)
	}
}
