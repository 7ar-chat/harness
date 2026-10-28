package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
	skillsfs "github.com/GhaziBenDahmane/harness/features/skills/skillsfs"
	"github.com/GhaziBenDahmane/harness/features/verify"
	"github.com/GhaziBenDahmane/harness/sandbox"
	"github.com/GhaziBenDahmane/harness/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/store"
)

// Lazy skills: skill text never needs a sandbox, and a sandbox only gets the
// skills its run needs (loaded with load_skill in the thread, requested for
// the run, referenced by the command about to run, and their dependencies).

var builtinNames = []string{"charts", "docx", "pdf", "pptx", "xlsx"}

// newLazyRig is the v2 rig with the real built-in skills.
func newLazyRig(t *testing.T, turns ...llmtest.Turn) *v2rig {
	t.Helper()
	return newV2RigWith(t, skillsfs.Builtin(), false, turns...)
}

// installedSkills lists the built-ins installed in sb.
func installedSkills(sb *sandboxtest.Sandbox) []string {
	var out []string
	for _, n := range builtinNames {
		if _, ok := sb.File("/skills/" + n + "/SKILL.md"); ok {
			out = append(out, n)
		}
	}
	return out
}

// execAt is the index of the first command of sb containing sub (-1: none).
func execAt(sb *sandboxtest.Sandbox, sub string) int {
	for i, e := range sb.Execs() {
		if strings.Contains(e.Command, sub) {
			return i
		}
	}
	return -1
}

func toolResults(r *v2rig, runID string) []string {
	var out []string
	for _, e := range find(r.events(runID), "TOOL_CALL_RESULT") {
		out = append(out, e["content"].(string))
	}
	return out
}

func TestARunThatOnlyLoadsASkillStartsNoSandbox(t *testing.T) {
	// load_skill returns the skill text without creating a sandbox: a run that only loads a skill and answers starts none.
	// Arrange
	r := newLazyRig(t,
		llmtest.Turn{Chunks: streamedCall("call_1", "load_skill", `{"name":"docx"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Here is how to write the memo.")},
	)
	_, run := r.queued("how should a memo be laid out?")

	// Act
	got := r.process(run)

	// Assert
	if n := len(r.backend.Created()); n != 0 {
		t.Fatalf("%d sandboxes created, want 0", n)
	}
	res := toolResults(r, run.ID)
	if got.Status != store.StatusCompleted || len(res) != 1 || !strings.Contains(res[0], "# Skill: docx") ||
		!strings.Contains(res[0], "/skills/docx/ (read-only) from your first command") || strings.Contains(res[0], "Installed at") ||
		!strings.Contains(res[0], "run `python3 /skills/docx/scripts/X.py` from /workspace; never cd into or write to the skill folder.") {
		t.Fatalf("status %s results %q", got.Status, res)
	}
}

func TestLoadSkillWorksWhenTheUserIsAtTheSandboxQuota(t *testing.T) {
	// A user whose 5 sandboxes are all in use can still load a skill: load_skill neither creates a sandbox nor fails on the quota.
	// Arrange
	r := newLazyRig(t,
		llmtest.Turn{Chunks: streamedCall("call_1", "load_skill", `{"name":"xlsx"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
	)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		th, _ := r.st.CreateThread(ctx, "alice", "other")
		if _, err := r.mgr.Acquire(ctx, th.ID, "alice"); err != nil {
			t.Fatal(err)
		}
	}
	_, run := r.queued("how do I build a workbook?")

	// Act
	r.process(run)

	// Assert
	res := find(r.events(run.ID), "TOOL_CALL_RESULT")
	if len(res) != 1 || res[0]["metadata"] != nil || !strings.Contains(res[0]["content"].(string), "# Skill: xlsx") {
		t.Fatalf("results %v", res)
	}
	if n := len(r.backend.Created()); n != 5 {
		t.Fatalf("%d sandboxes, want the 5 of the other threads", n)
	}
}

func TestLoadThenRunInstallsOnlyThatSkillAndItsDependencies(t *testing.T) {
	// load_skill docx then a docx script: one sandbox, docx installed before the command, no other skill.
	// Arrange
	r := newLazyRig(t,
		llmtest.Turn{Chunks: streamedCall("call_1", "load_skill", `{"name":"docx"}`)},
		llmtest.Turn{Chunks: streamedCall("call_2", "execute_command", `{"command":"python3 /skills/docx/scripts/new_document.py --spec /workspace/.work/memo.json --out /workspace/outputs/memo.docx"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
	)
	_, run := r.queued("write a memo")

	// Act
	r.process(run)

	// Assert
	if n := len(r.backend.Created()); n != 1 {
		t.Fatalf("%d sandboxes, want 1", n)
	}
	sb := r.sandbox()
	if got := installedSkills(sb); !equal(got, []string{"docx"}) {
		t.Fatalf("installed %v, want docx", got)
	}
	install, command := execAt(sb, "a-w /skills/docx"), execAt(sb, "python3 /skills/docx/scripts/new_document.py")
	if install < 0 || command < install {
		t.Fatalf("install at %d, command at %d", install, command)
	}
}

func TestACommandUsingASkillPathInstallsThatSkillFirst(t *testing.T) {
	// A command naming /skills/xlsx/... installs xlsx (and its dependencies) before it runs, without a load_skill and without unrelated skills.
	// Arrange
	r := newLazyRig(t,
		llmtest.Turn{Chunks: streamedCall("call_1", "execute_command", `{"command":"python3 /skills/xlsx/scripts/recalc.py /workspace/outputs/book.xlsx"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
	)
	_, run := r.queued("recalculate my workbook")

	// Act
	r.process(run)

	// Assert
	sb := r.sandbox()
	if got := installedSkills(sb); !equal(got, []string{"xlsx"}) {
		t.Fatalf("installed %v, want xlsx", got)
	}
	install, command := execAt(sb, "a-w /skills/xlsx"), execAt(sb, "python3 /skills/xlsx/scripts/recalc.py")
	if install < 0 || command < install {
		t.Fatalf("install at %d, command at %d", install, command)
	}
}

func TestPostWriteVerificationInstallsOnlyItsCheckerSkill(t *testing.T) {
	// The runtime's checker of a written .docx finds the docx skill installed before it runs, though no skill was loaded, and no unrelated skill is installed.
	// Arrange
	r := newLazyRig(t,
		llmtest.Turn{Chunks: streamedCall("call_1", "write_file", `{"path":"/workspace/outputs/memo.docx","content":"not really a docx"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
	)
	r.agent.Harness.Checker = verify.New(verify.Options{Sandbox: r.mgr, Messages: r.st, EnsureSkill: r.inst.EnsureSkill})
	_, run := r.queued("write a memo")

	// Act
	r.process(run)

	// Assert
	sb := r.sandbox()
	install, check := execAt(sb, "a-w /skills/docx"), execAt(sb, "python3 /skills/docx/scripts/inspect_docx.py --check")
	if install < 0 || check < install {
		t.Fatalf("install at %d, checker at %d", install, check)
	}
	if got := installedSkills(sb); !equal(got, []string{"docx"}) {
		t.Fatalf("installed %v, want docx", got)
	}
}

func TestASkillLoadedInAnEarlierRunIsInstalledOnTheNextRunsFirstCommand(t *testing.T) {
	// A skill loaded in an earlier run of the thread (no sandbox then) is installed in the sandbox a later run creates, before its first command.
	// Arrange
	r := newLazyRig(t,
		llmtest.Turn{Chunks: streamedCall("call_1", "load_skill", `{"name":"pptx"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Shall I build it?")},
		llmtest.Turn{Chunks: streamedCall("call_2", "execute_command", `{"command":"ls /workspace"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
	)
	th, first := r.queued("plan a deck")
	r.process(first)
	created := len(r.backend.Created())
	second := r.queuedOn(th, "yes, build it")

	// Act
	r.process(second)

	// Assert
	if created != 0 || len(r.backend.Created()) != 1 {
		t.Fatalf("sandboxes after the first run %d, after the second %d", created, len(r.backend.Created()))
	}
	sb := r.sandbox()
	if got := installedSkills(sb); !equal(got, []string{"pptx"}) {
		t.Fatalf("installed %v, want pptx", got)
	}
	install, command := execAt(sb, "a-w /skills/pptx"), execAt(sb, "ls /workspace")
	if install < 0 || command < install {
		t.Fatalf("install at %d, command at %d", install, command)
	}
}

func TestABackgroundJobUsingASkillPathInstallsThatSkillFirst(t *testing.T) {
	// A background job whose command names /skills/charts/... gets charts installed before it starts, and nothing else.
	// Arrange
	r := newLazyRig(t,
		llmtest.Turn{Chunks: streamedCall("call_1", "execute_command", `{"command":"python3 /skills/charts/scripts/make_chart.py --spec /workspace/.work/c.json --out /workspace/outputs/c.png","background":true}`)},
		llmtest.Turn{Chunks: llmtest.Text("Started")},
	)
	_, run := r.queued("chart it in the background")

	// Act
	r.process(run)

	// Assert
	sb := r.sandbox()
	if got := installedSkills(sb); !equal(got, []string{"charts"}) {
		t.Fatalf("installed %v, want charts", got)
	}
	install, command := execAt(sb, "a-w /skills/charts"), execAt(sb, "python3 /skills/charts/scripts/make_chart.py")
	if install < 0 || command < install {
		t.Fatalf("install at %d, job at %d", install, command)
	}
}

func TestLoadSkillWithARunningSandboxInstallsAtOnce(t *testing.T) {
	// With the thread's sandbox already running, load_skill installs the skill and its dependencies there at once (no new sandbox) and says so.
	// Arrange
	r := newLazyRig(t,
		llmtest.Turn{Chunks: streamedCall("call_1", "execute_command", `{"command":"ls /workspace"}`)},
		llmtest.Turn{Chunks: streamedCall("call_2", "load_skill", `{"name":"pdf"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
	)
	_, run := r.queued("make a pdf")

	// Act
	r.process(run)

	// Assert
	res := toolResults(r, run.ID)
	if len(r.backend.Created()) != 1 || len(res) != 2 || !strings.Contains(res[1], "Installed at /skills/pdf/ (read-only).") {
		t.Fatalf("sandboxes %d results %q", len(r.backend.Created()), res)
	}
	if got := installedSkills(r.sandbox()); !equal(got, []string{"pdf"}) {
		t.Fatalf("installed %v, want pdf", got)
	}
}

func TestSkillsIndexAndLoadSkillSayLoadingStartsNothing(t *testing.T) {
	// The skills index of the system prompt and the load_skill description both say that loading a skill starts nothing and that a skill's folder appears once loaded or named.
	// Arrange
	r := newLazyRig(t, llmtest.Turn{Chunks: llmtest.Text("Hi")})
	_, run := r.queued("hello")

	// Act
	r.process(run)

	// Assert
	call := r.model.Calls()[0]
	if prompt := call.Messages[0].Content; !strings.Contains(prompt, "Loading a skill starts nothing") ||
		!strings.Contains(prompt, "a skill's folder /skills/<name>/ appears once you load it (load_skill) or name its path") {
		t.Fatalf("prompt %q", prompt)
	}
	for _, ti := range call.Tools {
		if ti.Name == "load_skill" && (!strings.Contains(ti.Desc, "starts nothing") || !strings.Contains(ti.Desc, "appears once you load it")) {
			t.Fatalf("load_skill description %q", ti.Desc)
		}
	}
}

// fakePython stands for python3 in the sandbox: a .py script under /workspace
// prints "ok" when charts is installed, else fails like a missing
// `import make_chart`; "cat > /workspace/.work/b.py <<'EOF'" writes its heredoc.
func fakePython(sb *sandboxtest.Sandbox) sandboxtest.ExecFunc {
	return func(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
		cmd := req.Command
		if _, rest, ok := strings.Cut(cmd, "cat > /workspace/.work/b.py <<'EOF'\n"); ok {
			body, _, _ := strings.Cut(rest, "\nEOF")
			sb.Put("/workspace/.work/b.py", body+"\n")
		}
		if !strings.Contains(cmd, "python3 ") || strings.Contains(cmd, "python3 /skills/") {
			return sandbox.ExecResult{}, nil
		}
		if _, ok := sb.File("/skills/charts/scripts/make_chart.py"); ok {
			req.OnStdout("ok\n")
			return sandbox.ExecResult{}, nil
		}
		req.OnStderr("Traceback (most recent call last):\n  File \"/workspace/.work/b.py\", line 3, in <module>\nModuleNotFoundError: No module named 'make_chart'\n")
		return sandbox.ExecResult{ExitCode: 1}, nil
	}
}

// withFakePython makes every new sandbox of the rig answer python3 with fakePython (put: files placed at creation).
func withFakePython(r *v2rig, put map[string]string) {
	r.backend.OnCreate = func(sb *sandboxtest.Sandbox) {
		for p, c := range put {
			sb.Put(p, c)
		}
		sb.SetExec(sandboxtest.FindExec(sb, fakePython(sb)))
	}
}

const chartsNote = "Skill charts was installed; run the command again."

func TestAModelWrittenScriptImportingASkillWorksOnTheFirstRun(t *testing.T) {
	// A script written with write_file that imports /skills/charts/scripts/make_chart.py gets charts installed, so its first run succeeds.
	// Arrange
	r := newLazyRig(t,
		llmtest.Turn{Chunks: streamedCall("call_1", "write_file", `{"path":"/workspace/.work/build.py","content":"import sys\nsys.path.insert(0, \"/skills/charts/scripts\")\nimport make_chart\nprint(\"ok\")\n"}`)},
		llmtest.Turn{Chunks: streamedCall("call_2", "execute_command", `{"command":"python3 /workspace/.work/build.py"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
	)
	withFakePython(r, nil)
	_, run := r.queued("build it with the chart helpers")

	// Act
	r.process(run)

	// Assert
	res := toolResults(r, run.ID)
	if len(res) != 2 || !strings.Contains(res[1], `"stdout":"ok\n"`) || strings.Contains(res[1], "exitCode\":1") || strings.Contains(res[1], chartsNote) {
		t.Fatalf("results %q", res)
	}
	if got := installedSkills(r.sandbox()); !equal(got, []string{"charts"}) {
		t.Fatalf("installed %v, want charts", got)
	}
}

func TestAWorkspaceScriptIsScannedForSkillPathsBeforeItRuns(t *testing.T) {
	// A command running a script already in /workspace (here by a relative path) gets the skills that script names installed before it runs.
	// Arrange
	r := newLazyRig(t,
		llmtest.Turn{Chunks: streamedCall("call_1", "execute_command", `{"command":"cd /workspace && python3 .work/c.py"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
	)
	withFakePython(r, map[string]string{"/workspace/.work/c.py": "import sys\nsys.path.insert(0, '/skills/charts/scripts')\nimport make_chart\n"})
	_, run := r.queued("run my script")

	// Act
	r.process(run)

	// Assert
	res := toolResults(r, run.ID)
	if len(res) != 1 || !strings.Contains(res[0], `"stdout":"ok\n"`) || strings.Contains(res[0], chartsNote) {
		t.Fatalf("results %q", res)
	}
	if got := installedSkills(r.sandbox()); !equal(got, []string{"charts"}) {
		t.Fatalf("installed %v, want charts", got)
	}
}

func TestACommandFailingOnAMissingSkillModuleInstallsItAndSaysToRunAgain(t *testing.T) {
	// A heredoc script whose `import make_chart` fails gets charts installed and the note "Skill charts was installed; run the command again." (not re-run); the next run works.
	// Arrange
	heredoc := `cat > /workspace/.work/b.py <<'EOF'\nimport os, sys\nsys.path.insert(0, os.path.join(\"/skills\", \"charts\", \"scripts\"))\nimport make_chart\nEOF\npython3 /workspace/.work/b.py`
	r := newLazyRig(t,
		llmtest.Turn{Chunks: streamedCall("call_1", "execute_command", `{"command":"`+heredoc+`"}`)},
		llmtest.Turn{Chunks: streamedCall("call_2", "execute_command", `{"command":"python3 /workspace/.work/b.py"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
	)
	withFakePython(r, nil)
	_, run := r.queued("chart it")

	// Act
	r.process(run)

	// Assert
	res := toolResults(r, run.ID)
	if len(res) != 2 || !strings.Contains(res[0], `"exitCode":1`) || !strings.Contains(res[0], chartsNote) ||
		!strings.Contains(res[1], `"stdout":"ok\n"`) || strings.Contains(res[1], chartsNote) {
		t.Fatalf("results %q", res)
	}
	sb := r.sandbox()
	if runs := strings.Count(strings.Join(r.execCommands(), "\n"), "python3 /workspace/.work/b.py"); runs != 2 {
		t.Fatalf("the script ran %d times, want 2 (no automatic re-run)", runs)
	}
	if got := installedSkills(sb); !equal(got, []string{"charts"}) {
		t.Fatalf("installed %v, want charts", got)
	}
}
