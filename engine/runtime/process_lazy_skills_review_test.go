package runtime_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/GhaziBenDahmane/harness/ai/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/core/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/features/skills"
)

// Lazy skills, review fixes: explicit defaults, the /skills folder, implicit
// dependencies of every skill source, install markers, script reads.

// hasMarker reports an install marker (/skills/<name>/.installed-<revision>).
func hasMarker(sb *sandboxtest.Sandbox, name string) bool {
	for _, p := range sb.Paths("/skills/" + name + "/") {
		if strings.HasPrefix(p, "/skills/"+name+"/.installed-") {
			return true
		}
	}
	return false
}

func countExecs(r *scriptedRig, sub string) int {
	n := 0
	for _, c := range r.execCommands() {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

func TestAMentionedDefaultSkillIsInstalledBeforeTheFirstCommand(t *testing.T) {
	// A default skill the user mentions (@docx) is installed with its dependencies when the sandbox starts, before a first command that does not name it.
	// Arrange
	r := newLazyRig(t,
		llmtest.Turn{Chunks: streamedCall("call_1", "execute_command", `{"command":"ls /workspace"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
	)
	th, _ := r.st.CreateThread(context.Background(), "alice", "t")
	run := r.create(th, store.NewRun{Message: store.Message{Role: store.RoleUser, Content: "@docx write a memo"}, Skills: []string{"docx"}})

	// Act
	r.process(run)

	// Assert
	sb := r.sandbox()
	if got := installedSkills(sb); !equal(got, []string{"docx"}) {
		t.Fatalf("installed %v, want docx", got)
	}
	if install, command := execAt(sb, "a-w /skills/docx"), execAt(sb, "ls /workspace"); install < 0 || command < install {
		t.Fatalf("install at %d, command at %d", install, command)
	}
}

func TestListingTheSkillsFolderInstallsTheIndexedSkills(t *testing.T) {
	// A command naming the bare /skills folder (ls /skills/) installs the skills of the run's index first, so the listing is not empty.
	// Arrange
	r := newLazyRig(t,
		llmtest.Turn{Chunks: streamedCall("call_1", "execute_command", `{"command":"ls /skills/"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
	)
	_, run := r.queued("which skills do you have?")

	// Act
	r.process(run)

	// Assert
	sb := r.sandbox()
	if got := installedSkills(sb); !equal(got, builtinNames) {
		t.Fatalf("installed %v, want %v", got, builtinNames)
	}
	if install, command := execAt(sb, "a-w /skills/charts"), execAt(sb, "ls /skills/"); install < 0 || command < install {
		t.Fatalf("install at %d, command at %d", install, command)
	}
}

const chartsScript = "import sys\nsys.path.insert(0, '/skills/charts/scripts')\nimport make_chart\n"

func TestAUserSkillUsingABuiltinWithoutDeclaringItGetsItInstalled(t *testing.T) {
	// A user-directory skill whose script imports /skills/charts/scripts (no "requires") gets charts installed with it.
	// Arrange
	r := newLazyRig(t,
		llmtest.Turn{Chunks: streamedCall("call_1", "execute_command", `{"command":"python3 /skills/my-tool/scripts/x.py"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
	)
	r.reg.AddFS(skills.SourceUser, fstest.MapFS{
		"my-tool/SKILL.md":     {Data: []byte("---\nname: my-tool\ndescription: My charting tool.\n---\n\nRun scripts/x.py.\n")},
		"my-tool/scripts/x.py": {Data: []byte(chartsScript)},
	})
	_, run := r.queued("use my tool")

	// Act
	r.process(run)

	// Assert
	sb := r.sandbox()
	if _, ok := sb.File("/skills/my-tool/scripts/x.py"); !ok {
		t.Fatal("my-tool not installed")
	}
	if got := installedSkills(sb); !equal(got, []string{"charts"}) {
		t.Fatalf("installed built-ins %v, want charts", got)
	}
}

func TestAnInstallFailingMidwayIsNotRecordedAndIsRetried(t *testing.T) {
	// An upload failing in the middle of an install leaves it unrecorded (no marker): the next command installs the skill again, completely, with its marker.
	// Arrange
	r := newLazyRig(t,
		llmtest.Turn{Chunks: streamedCall("call_1", "execute_command", `{"command":"python3 /skills/xlsx/scripts/recalc.py /workspace/outputs/b.xlsx"}`)},
		llmtest.Turn{Chunks: streamedCall("call_2", "execute_command", `{"command":"python3 /skills/xlsx/scripts/recalc.py /workspace/outputs/b.xlsx"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
	)
	var failed atomic.Bool
	r.backend.OnCreate = func(sb *sandboxtest.Sandbox) {
		sb.SetExec(sandboxtest.FindExec(sb, nil))
		sb.SetWriteErr(func(p string) error {
			if strings.HasSuffix(p, "/skills/xlsx/scripts/recalc.py") && failed.CompareAndSwap(false, true) {
				return errors.New("upload interrupted")
			}
			return nil
		})
	}
	_, run := r.queued("recalculate")

	// Act
	r.process(run)

	// Assert
	sb := r.sandbox()
	if _, ok := sb.File("/skills/xlsx/scripts/recalc.py"); !ok || !hasMarker(sb, "xlsx") {
		t.Fatalf("xlsx not completely installed: %v", sb.Paths("/skills/xlsx/"))
	}
	if prepares, protects := countExecs(r, "rm -rf /skills/xlsx"), protectCount(r.execCommands(), "/skills/xlsx"); prepares != 2 || protects != 1 {
		t.Fatalf("prepares %d protects %d, want 2 and 1", prepares, protects)
	}
}

func TestASkillRecordedInRedisButMissingInTheSandboxIsInstalledAgain(t *testing.T) {
	// Redis alone is not trusted: after a runtime restart, a skill recorded as installed whose files (and marker) are gone is installed again.
	// Arrange
	r := newLazyRig(t,
		llmtest.Turn{Chunks: streamedCall("call_1", "execute_command", `{"command":"python3 /skills/xlsx/scripts/recalc.py a.xlsx"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
		llmtest.Turn{Chunks: streamedCall("call_2", "execute_command", `{"command":"python3 /skills/xlsx/scripts/recalc.py a.xlsx"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
	)
	th, first := r.queued("recalculate")
	r.process(first)
	r.sandbox().RemoveTree("/skills/xlsx")
	r.wire() // a new runtime process: nothing remembered in memory

	// Act
	r.process(r.queuedOn(th, "again"))

	// Assert
	sb := r.sandbox()
	if _, ok := sb.File("/skills/xlsx/scripts/recalc.py"); !ok || len(r.backend.Created()) != 1 {
		t.Fatalf("xlsx not reinstalled (sandboxes %d)", len(r.backend.Created()))
	}
}

func TestASymlinkedScriptIsNotFollowedWhenScanned(t *testing.T) {
	// The scan of the scripts a command runs never follows a symlink: a /workspace link to a file naming /skills/xlsx/ installs nothing.
	// Arrange
	r := newLazyRig(t,
		llmtest.Turn{Chunks: streamedCall("call_1", "execute_command", `{"command":"python3 /workspace/.work/link.py"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
	)
	r.backend.OnCreate = func(sb *sandboxtest.Sandbox) {
		sb.SetExec(sandboxtest.FindExec(sb, nil))
		sb.Put("/tmp/real.py", "import sys\nsys.path.insert(0, '/skills/xlsx/scripts')\n")
		sb.Symlink("/tmp/real.py", "/workspace/.work/link.py")
	}
	_, run := r.queued("run it")

	// Act
	r.process(run)

	// Assert
	if got := installedSkills(r.sandbox()); len(got) != 0 {
		t.Fatalf("installed %v through a symlink", got)
	}
}

func TestScriptsAreReadOnceUnlessTheyChange(t *testing.T) {
	// A script run twice unchanged is downloaded once, and a script written with write_file is never downloaded (its content was scanned when written).
	// Arrange
	cmd := `{"command":"python3 .work/w.py && python3 .work/c.py"}`
	r := newLazyRig(t,
		llmtest.Turn{Chunks: streamedCall("call_1", "write_file", `{"path":".work/w.py","content":"`+strings.ReplaceAll(chartsScript, "\n", `\n`)+`"}`)},
		llmtest.Turn{Chunks: streamedCall("call_2", "execute_command", cmd)},
		llmtest.Turn{Chunks: streamedCall("call_3", "execute_command", cmd)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
	)
	r.backend.OnCreate = func(sb *sandboxtest.Sandbox) {
		sb.SetExec(sandboxtest.FindExec(sb, nil))
		sb.Put("/workspace/.work/c.py", "print('plain')\n")
	}
	_, run := r.queued("run them")

	// Act
	r.process(run)

	// Assert
	sb := r.sandbox()
	if n := sb.FileReads(); n != 1 {
		t.Fatalf("%d script downloads, want 1 (c.py once)", n)
	}
	if got := installedSkills(sb); !equal(got, []string{"charts"}) {
		t.Fatalf("installed %v, want charts", got)
	}
}

func TestSkillsFoundInWrittenFilesAndFailedCommandsComeBackInALaterRun(t *testing.T) {
	// Skills discovered from a written file (xlsx) and from a failed command's note (charts) are installed again in the new sandbox of a later run.
	// Arrange
	heredoc := `cat > /workspace/.work/b.py <<'EOF'\nimport os, sys\nsys.path.insert(0, os.path.join(\"/skills\", \"charts\", \"scripts\"))\nimport make_chart\nEOF\npython3 /workspace/.work/b.py`
	r := newLazyRig(t,
		llmtest.Turn{Chunks: streamedCall("call_1", "execute_command", `{"command":"`+heredoc+`"}`)},
		llmtest.Turn{Chunks: streamedCall("call_2", "write_file", `{"path":"/workspace/.work/x.py","content":"import sys\nsys.path.insert(0, '/skills/xlsx/scripts')\n"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
		llmtest.Turn{Chunks: streamedCall("call_3", "execute_command", `{"command":"ls /workspace"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
	)
	withFakePython(r, nil)
	th, first := r.queued("prepare")
	r.process(first)
	r.killed()

	// Act
	r.process(r.queuedOn(th, "continue"))

	// Assert
	sb := r.sandbox()
	if got := installedSkills(sb); len(r.backend.Created()) != 2 || !equal(got, []string{"charts", "xlsx"}) {
		t.Fatalf("sandboxes %d, installed %v, want charts and xlsx", len(r.backend.Created()), got)
	}
	if install, command := execAt(sb, "a-w /skills/"), execAt(sb, "ls /workspace"); install < 0 || command < install {
		t.Fatalf("install at %d, command at %d", install, command)
	}
}
