package skills_test

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/engine/tools"
	"github.com/7ar-chat/harness/features/skills"
)

func TestSkillPathsDetectsTheBareSkillsFolderAsAPathArgument(t *testing.T) {
	// A bare /skills (end of text, space, separator, glob) means "list the skills"; a path inside a word, a file-like name or another folder does not.
	cases := map[string]bool{
		"ls /skills":              true,
		"ls /skills/":             true,
		"ls /skills/*":            true,
		"cd /skills; ls":          true,
		"find /skills | head":     true,
		"(ls /skills)":            true,
		"ls /skills\nls":          true,
		"ls /skillset":            false,
		"ls /home/skills":         false,
		"ls /skills.d":            false,
		"echo my/skills/":         false,
		"cat /skills/docx/x.md":   false,
		"ls /skills/docx":         false,
		"rm -rf /skills&&echo ok": true,
	}
	for text, want := range cases {
		// Act
		_, root := skills.SkillPaths(text)

		// Assert
		if root != want {
			t.Errorf("SkillPaths(%q) root = %v, want %v", text, root, want)
		}
	}
}

func TestSkillPathsNamesOnlyWholeSkillFolders(t *testing.T) {
	// Names stop at the first non-slug character, are listed once in order, and a name glued to a longer word (file extension, camel case) is ignored.
	// Act
	names, _ := skills.SkillPaths("cp /skills/docx/a /skills/pdf-tools/b; cat /skills/docx/c '/skills/xlsx' /skills/Docx/x /skills/a_b/ /skills/zip.tar")

	// Assert
	if want := []string{"docx", "pdf-tools", "xlsx"}; !slices.Equal(names, want) {
		t.Fatalf("names %v, want %v", names, want)
	}
}

func TestScriptPathsIgnoresInlineCodeFlagsOfEveryInterpreter(t *testing.T) {
	// Flags that run inline code or a module name no file for node, bash and long options; ordinary flags before a file do not hide it; at most four scripts are returned.
	cases := map[string][]string{
		"node --eval 'x()'":       nil,
		"node --print 1":          nil,
		"node --no-warnings a.js": {"/workspace/a.js"},
		"bash -c 'echo'":          nil,
		"sh -lc 'echo'":           nil,
		"bash -x run.sh":          {"/workspace/run.sh"},
		"python3 --version":       nil,
		"python3 a.py; python3 b.py; python3 c.py; python3 d.py; python3 e.py": {"/workspace/a.py", "/workspace/b.py", "/workspace/c.py", "/workspace/d.py"},
	}
	for cmd, want := range cases {
		// Act
		got := skills.ScriptPaths(cmd)

		// Assert
		if !slices.Equal(got, want) {
			t.Errorf("ScriptPaths(%q) = %v, want %v", cmd, got, want)
		}
	}
}

func TestLoadedInIgnoresMalformedAndUnrelatedCalls(t *testing.T) {
	// Tool calls with unparsable arguments, other tools, and results without an install note add nothing; repeated skills are listed once; a multi-skill note lists each.
	// Arrange
	msgs := []*store.Message{
		{Role: store.RoleAssistant, ToolCalls: []store.ToolCall{
			{ID: "1", Function: store.ToolCallFunction{Name: "load_skill", Arguments: "{broken"}},
			{ID: "2", Function: store.ToolCallFunction{Name: "write_file", Arguments: "[1]"}},
			{ID: "3", Function: store.ToolCallFunction{Name: "bash", Arguments: `{"name":"docx","content":"/skills/pdf/"}`}},
			{ID: "4", Function: store.ToolCallFunction{Name: "load_skill", Arguments: `{"name":" docx "}`}},
			{ID: "5", Function: store.ToolCallFunction{Name: "load_skill", Arguments: `{"name":"docx"}`}},
			{ID: "6", Function: store.ToolCallFunction{Name: "load_skill", Arguments: `{"name":""}`}},
		}},
		{Role: store.RoleTool, ToolCallID: "x", Content: "Skills pdf, xlsx were installed; run the command again."},
		{Role: store.RoleUser, Content: "Skill ghost was installed; run the command again."},
	}

	// Act
	got := skills.LoadedIn(msgs)

	// Assert
	if want := []string{"docx", "pdf", "xlsx"}; !slices.Equal(got, want) {
		t.Fatalf("LoadedIn = %v, want %v", got, want)
	}
}

func TestDirsOrdersUserThenForeignThenProjectAndDropsDuplicates(t *testing.T) {
	// Directories come lowest priority first: SKILLS_DIR, the user dir, the foreign user dirs, then the untrusted project dirs; a directory named twice is listed once (first position).
	// Arrange
	home, repo := filepath.Join(string(filepath.Separator), "h"), filepath.Join(string(filepath.Separator), "r")

	// Act
	full := skills.Dirs(skills.DiscoverOptions{SkillsDir: filepath.Join(home, ".har", "skills"), Home: home, ForeignUserDirs: true, ProjectRoot: repo})
	userDir := skills.Dirs(skills.DiscoverOptions{UserDir: filepath.Join(home, "cfg"), NoUserDir: false, SkillsDir: "  "})
	hermetic := skills.Dirs(skills.DiscoverOptions{UserDir: filepath.Join(home, "cfg"), NoUserDir: true, ProjectRoot: repo})

	// Assert
	var got []string
	for _, d := range full {
		got = append(got, d.Source+":"+d.Path)
	}
	want := []string{
		"user:" + filepath.Join(home, ".har", "skills"),
		"user:" + filepath.Join(home, ".agents", "skills"),
		"user:" + filepath.Join(home, ".claude", "skills"),
		"project:" + filepath.Join(repo, ".har", "skills"),
		"project:" + filepath.Join(repo, ".agents", "skills"),
		"project:" + filepath.Join(repo, ".claude", "skills"),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("dirs\n got %v\nwant %v", got, want)
	}
	if len(userDir) == 0 || userDir[0].Path != filepath.Join(home, "cfg", "skills") || userDir[0].Source != skills.SourceUser {
		t.Fatalf("UserDir: %+v", userDir)
	}
	if len(hermetic) != 3 {
		t.Fatalf("a hermetic server reads only the project dirs: %+v", hermetic)
	}
	for _, d := range hermetic {
		if d.Source != skills.SourceProject {
			t.Errorf("hermetic dir %+v", d)
		}
	}
}

func TestLoadSandboxReportsAGoneSandboxAndSkipsOversizedFolders(t *testing.T) {
	// A sandbox found gone is an error to the caller; a skill folder with too many files or a hidden/cache file is read without them or skipped, never half loaded.
	// Arrange
	gone := sandboxtest.NewSandbox("sb-gone")
	gone.Die()
	sb := sandboxtest.NewSandbox("sb-1")
	putSkill(sb, "/workspace/.har/skills", "tidy", "Tidy.")
	sb.Put("/workspace/.har/skills/tidy/.hidden", "x")
	sb.Put("/workspace/.har/skills/tidy/__pycache__/a.pyc", "x")
	sb.Put("/workspace/.har/skills/tidy/scripts/run.pyc", "x")
	sb.Put("/workspace/.har/skills/tidy/scripts/run.py", "print(1)")
	putSkill(sb, "/workspace/.har/skills", "crowded", "Too many files.")
	for i := range skills.MaxEntries + 1 {
		sb.Put(fmt.Sprintf("/workspace/.har/skills/crowded/f/%04d.txt", i), "x")
	}

	// Act
	_, goneErr := skills.LoadSandbox(context.Background(), gone)
	reg, err := skills.LoadSandbox(context.Background(), sb)

	// Assert
	if !sandbox.IsGone(goneErr) {
		t.Fatalf("gone sandbox error: %v", goneErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range reg.List(context.Background()) {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, []string{"tidy"}) {
		t.Fatalf("skills %v", names)
	}
	_, pkg, _ := reg.ByName(context.Background(), "tidy")
	if got := pkg.Paths(); !slices.Equal(got, []string{"SKILL.md", "scripts/run.py"}) {
		t.Fatalf("paths %v: hidden and compiled files must be left out like LoadFS does", got)
	}
}

func TestACommandOnAGoneSandboxFailsDiscoveryButThePromptStillRenders(t *testing.T) {
	// A sandbox found gone while reading the project skills fails the command hook (the caller recreates the sandbox) but only degrades the prompt to the global skills.
	// Arrange
	sb := sandboxtest.NewSandbox("sb-1")
	sb.Die()
	reg := skills.NewRegistry(skillFS("alpha", "Builtin alpha.", "A"), nil)
	inst := &skills.Installer{Registry: reg, SandboxProject: true, Sandboxes: staticManager{sb}}
	rc := tools.RunContext{ThreadID: "th-1", RunID: "run-1"}

	// Act
	err := inst.EnsureCommandSkills(context.Background(), rc, sb, "ls")
	section := inst.PromptSection(context.Background(), agent.Input{Run: &store.Run{ID: "run-2", ThreadID: "th-1"}, Sandbox: true})

	// Assert
	if !sandbox.IsGone(err) {
		t.Fatalf("EnsureCommandSkills error: %v", err)
	}
	if !strings.Contains(section, "- alpha: Builtin alpha.") {
		t.Fatalf("section %q", section)
	}
}
