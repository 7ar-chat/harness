package runtime_test

import (
	"archive/zip"
	"bytes"
	"context"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/GhaziBenDahmane/harness/agent/prompts"
	"github.com/GhaziBenDahmane/harness/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/store"
)

// Materialization of attached skills (Work: codex.py "Attached Skills" section
// and _sync_attached_skills_to_workspace): the session's skill-creator and
// selected versions are installed with their files before the first sandbox
// command; defaults only once loaded or used (lazy skills).

func protectCount(execs []string, dir string) int {
	n := 0
	for _, c := range execs {
		if strings.Contains(c, "chmod -R a+rX,a-w "+dir) {
			n++
		}
	}
	return n
}

func (v *v2rig) execCommands() []string {
	var out []string
	for _, sb := range v.backend.Created() {
		for _, e := range sb.Execs() {
			out = append(out, e.Command)
		}
	}
	return out
}

func TestDefaultSkillsAreListedButInstalledOnlyWhenACommandUsesThem(t *testing.T) {
	// Built-ins are Work Chat's studio-work-default skills: listed once in the skills index (no "Attached Skills" block, A-08) but not installed
	// with every sandbox (lazy skills): the first command naming /skills/fixture-doc installs it, and later commands and runs reinstall nothing.
	// Arrange
	r := newV2Rig(t, false,
		llmtest.Turn{Chunks: streamedCall("call_1", "execute_command", `{"command":"ls /workspace"}`)},
		llmtest.Turn{Chunks: streamedCall("call_2", "execute_command", `{"command":"ls /skills/fixture-doc"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
		llmtest.Turn{Chunks: streamedCall("call_3", "execute_command", `{"command":"python3 /skills/fixture-doc/scripts/hello.py"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
	)
	th, run := r.queued("hello")

	// Act
	r.process(run)
	r.process(r.queuedOn(th, "again"))

	// Assert
	prompt := r.model.Calls()[0].Messages[0].Content
	if !strings.Contains(prompt, "<skills>\n"+prompts.SkillsIntro+"\n- fixture-doc: Create fixture documents for tests.\n</skills>") ||
		strings.Contains(prompt, "Attached Skills") || strings.Count(prompt, "fixture-doc") != 1 {
		t.Fatalf("prompt %q", prompt)
	}
	if strings.Contains(prompt, `## Skill "fixture-doc"`) {
		t.Fatal("a default skill's SKILL.md is injected like a mention")
	}
	sb := r.sandbox()
	install, first, naming := execAt(sb, "a-w /skills/fixture-doc"), execAt(sb, "ls /workspace"), execAt(sb, "ls /skills/fixture-doc")
	if _, ok := sb.File("/skills/fixture-doc/scripts/hello.py"); !ok || install < first || naming < install {
		t.Fatalf("install at %d, first command at %d, command naming the skill at %d", install, first, naming)
	}
	if n := protectCount(r.execCommands(), "/skills/fixture-doc"); n != 1 {
		t.Fatalf("installed %d times, want once", n)
	}
}

func TestDisabledDefaultSkillsAreNotAttached(t *testing.T) {
	// The user's toggle still applies: a disabled built-in is neither listed nor installed.
	// Arrange
	r := newV2Rig(t, false,
		llmtest.Turn{Chunks: streamedCall("call_1", "execute_command", `{"command":"ls"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
	)
	_ = r.st.SetSkillEnabled(context.Background(), "alice", "builtin:fixture-doc", false)
	_, run := r.queued("hello")

	// Act
	r.process(run)

	// Assert
	if prompt := r.model.Calls()[0].Messages[0].Content; strings.Contains(prompt, "Attached Skills") {
		t.Fatalf("prompt %q", prompt)
	}
	if _, ok := r.sandbox().File("/skills/fixture-doc/SKILL.md"); ok {
		t.Fatal("disabled skill installed")
	}
}

func TestSelectedSkillVersionIsTheOneMaterialized(t *testing.T) {
	// A run attaching "name@versionId" installs and injects that version (here a draft), not the published one.
	// Arrange
	r := newV2Rig(t, false,
		llmtest.Turn{Chunks: streamedCall("call_1", "execute_command", `{"command":"cat /skills/my-notes/SKILL.md"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
	)
	ctx := context.Background()
	s, err := r.reg.Upload(ctx, "alice", skillZip(t, map[string]string{"SKILL.md": "---\nname: my-notes\ndescription: Notes.\n---\n\nPublished text.\n"}))
	if err != nil {
		t.Fatal(err)
	}
	draft, err := r.reg.AddEditorVersion(ctx, "alice", s.ID, "2.0.0", "---\nname: my-notes\ndescription: Notes.\n---\n\nDraft text.\n", false, false)
	if err != nil {
		t.Fatal(err)
	}
	th, _ := r.st.CreateThread(ctx, "alice", "t")
	run := r.create(th, store.NewRun{Message: store.Message{Role: store.RoleUser, Content: "go"}, Skills: []string{"my-notes@" + draft.Latest.ID}})

	// Act
	r.process(run)

	// Assert
	prompt := r.model.Calls()[0].Messages[0].Content
	if !strings.Contains(prompt, "- my-notes: <skill") || !strings.Contains(prompt, "Draft text.") || strings.Contains(prompt, "Published text.") {
		t.Fatalf("prompt %q", prompt)
	}
	if md, _ := r.sandbox().File("/skills/my-notes/SKILL.md"); !strings.Contains(md, "Draft text.") {
		t.Fatalf("installed %q", md)
	}
}

func TestSkillCreationSessionAttachesTheSkillCreator(t *testing.T) {
	// A thread in mode skill_creation gets Work's skill-creator attached (with its scripts) and the package instruction.
	// Arrange
	r := newV2Rig(t, false,
		llmtest.Turn{Chunks: streamedCall("call_1", "execute_command", `{"command":"ls /skills/anthropic-skill-creator"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
	)
	if _, err := r.reg.LoadAnthropic(skillCreatorFixture(t)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	th, _ := r.st.CreateThread(ctx, "alice", "t")
	if err := r.st.CreateSkillCreationSession(ctx, store.SkillCreationSession{ThreadID: th.ID, UserLogin: "alice", PackageRoot: "/workspace/generated-skill"}); err != nil {
		t.Fatal(err)
	}
	run := r.queuedOn(th, "Help me create a new skill.")

	// Act
	r.process(run)

	// Assert
	prompt := r.model.Calls()[0].Messages[0].Content
	if !strings.Contains(prompt, "- anthropic-skill-creator: ") || !strings.Contains(prompt, "## Skill creation") ||
		!strings.Contains(prompt, "Store every complete generated skill package only under /workspace/generated-skill/<skill-name>.") {
		t.Fatalf("prompt %q", prompt)
	}
	sb := r.sandbox()
	for _, p := range []string{"SKILL.md", "LICENSE.txt", "scripts/package_skill.py"} {
		if _, ok := sb.File("/skills/anthropic-skill-creator/" + p); !ok {
			t.Errorf("missing /skills/anthropic-skill-creator/%s", p)
		}
	}
}

// skillCreatorFixture is a one-skill Anthropic catalog (manifest + zip package)
// standing in for the licensed skill-creator package.
func skillCreatorFixture(t *testing.T) fs.FS {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{
		"SKILL.md":                 "---\nname: skill-creator\ndescription: Create skills.\n---\nCreate skills.\n",
		"LICENSE.txt":              "test license",
		"scripts/package_skill.py": "print('package')\n",
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	manifest := `{"version":"test","skills":[{"slug":"anthropic-skill-creator","display_name":"Skill Creator","package":"packages/anthropic-skill-creator.zip","summary":"Create skills."}]}`
	return fstest.MapFS{
		"manifest.json":                        {Data: []byte(manifest)},
		"packages/anthropic-skill-creator.zip": {Data: buf.Bytes()},
	}
}
