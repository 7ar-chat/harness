package skills_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/GhaziBenDahmane/harness/engine/tools"
	"github.com/GhaziBenDahmane/harness/features/skills"
	skillsfs "github.com/GhaziBenDahmane/harness/features/skills/skillsfs"
	"github.com/GhaziBenDahmane/harness/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/store"
	"github.com/GhaziBenDahmane/harness/store/memstore"
)

func TestReferencedFindsOnlyKnownSkillFoldersAtTheStartOfAWord(t *testing.T) {
	// Referenced returns the known skills a command names as /skills/<name>/ or /skills/<name> ending a word; other paths, unknown names and file names like /skills/docx.zip do not count.
	// Arrange
	known := []string{"docx", "xlsx", "shared-kit"}
	cases := map[string][]string{
		"python3 /skills/docx/scripts/new_document.py --out a.docx": {"docx"},
		"ls /skills/xlsx": {"xlsx"},
		`cat "/skills/xlsx/SKILL.md";ls '/skills/shared-kit'`:        {"xlsx", "shared-kit"},
		"ls /home/skills/docx/ /x/skills/xlsx/":                      nil,
		"cp /skills/docx.zip . && echo /skills/docx. ":               nil,
		"python3 /skills/unknown/x.py /skills/docx/a /skills/docx/b": {"docx"},
		"python3 /skills/docx_old/x.py":                              nil,
	}

	// Act
	got := map[string][]string{}
	for cmd := range cases {
		got[cmd] = skills.Referenced(cmd, known)
	}

	// Assert
	for cmd, want := range cases {
		if !slices.Equal(got[cmd], want) {
			t.Errorf("Referenced(%q) = %v, want %v", cmd, got[cmd], want)
		}
	}
}

func TestScriptPathsListsTheWorkspaceFilesACommandRuns(t *testing.T) {
	// ScriptPaths returns the /workspace files a command runs (interpreters, ./x, nested bash -lc "..."), never module or inline-code runs (-m, -c, -mpip) nor files outside /workspace.
	// Arrange
	cases := map[string][]string{
		"python3 build.py": {"/workspace/build.py"},
		"cd /workspace && python3 -u .work/c.py --x 1":        {"/workspace/.work/c.py"},
		"node /workspace/a.js; bash ./b.sh":                   {"/workspace/a.js", "/workspace/b.sh"},
		"./run.sh && /workspace/tools/x":                      {"/workspace/run.sh", "/workspace/tools/x"},
		`bash -lc "python3 /workspace/.work/n.py"`:            {"/workspace/.work/n.py"},
		"python3 -m http.server 8000":                         nil,
		"python3 -mpip install pandas":                        nil,
		`python3 -c "import x" && sh -c 'echo hi'`:            nil,
		"node -e 'console.log(1)'":                            nil,
		"python3 /skills/docx/scripts/x.py /etc/passwd":       nil,
		"python3 ../../etc/x.py":                              nil,
		"python3 $SCRIPT":                                     nil,
		"python3 a.py; python3 a.py; python3 /workspace/a.py": {"/workspace/a.py"},
	}

	// Act
	got := map[string][]string{}
	for cmd := range cases {
		got[cmd] = skills.ScriptPaths(cmd)
	}

	// Assert
	for cmd, want := range cases {
		if !slices.Equal(got[cmd], want) {
			t.Errorf("ScriptPaths(%q) = %v, want %v", cmd, got[cmd], want)
		}
	}
}

func TestLoadedInListsTheSkillsAHistoryLoadedOrUsed(t *testing.T) {
	// LoadedIn returns the skills of successful load_skill calls, of /skills/<name>/ paths in written file content (write_file, edit_file) and of "was installed" notes; failed calls do not count.
	// Arrange
	call := func(id, name, args string) store.ToolCall {
		return store.ToolCall{ID: id, Type: "function", Function: store.ToolCallFunction{Name: name, Arguments: args}}
	}
	msgs := []*store.Message{
		{Role: store.RoleAssistant, ToolCalls: []store.ToolCall{
			call("c1", "load_skill", `{"name":"docx"}`),
			call("c2", "load_skill", `{"name":"pptx"}`),
			call("c3", "write_file", `{"path":"a.py","content":"sys.path.insert(0, '/skills/shared-kit/scripts')"}`),
			call("c4", "edit_file", `{"path":"b.py","oldText":"x","newText":"open('/skills/xlsx/SKILL.md')"}`),
			call("c5", "write_file", `{"path":"c.py","content":"open('/skills/charts/x')"}`),
		}},
		{Role: store.RoleTool, ToolCallID: "c1", Content: "# Skill: docx"},
		{Role: store.RoleTool, ToolCallID: "c2", Content: "Error", Error: "unknown skill"},
		{Role: store.RoleTool, ToolCallID: "c5", Content: "Error", Error: "symlink"},
		{Role: store.RoleTool, ToolCallID: "c6", Content: `{"exitCode":1,"note":"Skill pdf was installed; run the command again."}`},
	}

	// Act
	got := skills.LoadedIn(msgs)

	// Assert
	if want := []string{"docx", "shared-kit", "xlsx", "pdf"}; !slices.Equal(got, want) {
		t.Fatalf("LoadedIn = %v, want %v", got, want)
	}
}

func TestConcurrentInstallsFromTwoRuntimesInstallASkillOnce(t *testing.T) {
	// Two runtimes (installers sharing Redis) preparing commands on the same sandbox at once install xlsx once: the second waits and finds it installed.
	// Arrange
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	st := memstore.New()
	reg := skills.NewRegistry(skillsfs.Builtin(), st)
	sb := sandboxtest.NewSandbox("sb-1")
	sb.SetWriteErr(func(string) error { time.Sleep(2 * time.Millisecond); return nil }) // slow uploads widen the race
	rc := tools.RunContext{ThreadID: "th-1", RunID: "run-1", Login: "alice"}
	replicas := []*skills.Installer{{Registry: reg, Redis: rdb}, {Registry: reg, Redis: rdb}}

	// Act
	var wg sync.WaitGroup
	errs := make([]error, len(replicas))
	for k, inst := range replicas {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[k] = inst.EnsureCommandSkills(context.Background(), rc, sb, "python3 /skills/xlsx/scripts/recalc.py a.xlsx")
		}()
	}
	wg.Wait()

	// Assert
	protects := 0
	for _, e := range sb.Execs() {
		if strings.Contains(e.Command, "a-w /skills/xlsx") {
			protects++
		}
	}
	if errs[0] != nil || errs[1] != nil || protects != 1 {
		t.Fatalf("errors %v, xlsx installed %d times, want once", errs, protects)
	}
}
