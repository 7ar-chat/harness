package runtime_test

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/agent/prompts"
	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/features/skills"
)

// Materialization of attached skills: the skills requested for a run are
// installed with their files before the first sandbox command; defaults only
// once loaded or used (lazy skills).

func protectCount(execs []string, dir string) int {
	n := 0
	for _, c := range execs {
		if strings.Contains(c, "chmod -R a+rX,a-w "+dir) {
			n++
		}
	}
	return n
}

func (v *scriptedRig) execCommands() []string {
	var out []string
	for _, sb := range v.backend.Created() {
		for _, e := range sb.Execs() {
			out = append(out, e.Command)
		}
	}
	return out
}

func TestDefaultSkillsAreListedButInstalledOnlyWhenACommandUsesThem(t *testing.T) {
	// Built-ins are listed once in the skills index (no "Attached Skills" block, A-08) but not installed
	// with every sandbox (lazy skills): the first command naming /skills/fixture-doc installs it, and later commands and runs reinstall nothing.
	// Arrange
	r := newScriptedRig(t, false,
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

func TestRequestedUserDirectorySkillIsInstalledAndInjected(t *testing.T) {
	// A skill found in a skills directory (here the user source) and requested by name for the run is installed before the first command and its SKILL.md is injected; it is not fenced (the user's own skill).
	// Arrange
	r := newScriptedRig(t, false,
		llmtest.Turn{Chunks: streamedCall("call_1", "execute_command", `{"command":"cat /skills/my-notes/SKILL.md"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
	)
	r.reg.AddFS(skills.SourceUser, fstest.MapFS{
		"my-notes/SKILL.md":         {Data: []byte("---\nname: my-notes\ndescription: Notes.\n---\n\nUser notes text.\n")},
		"my-notes/scripts/count.py": {Data: []byte("print(1)\n")},
	})
	ctx := context.Background()
	th, _ := r.st.CreateThread(ctx, "alice", "t")
	run := r.create(th, store.NewRun{Message: store.Message{Role: store.RoleUser, Content: "go"}, Skills: []string{"my-notes"}})

	// Act
	r.process(run)

	// Assert
	prompt := r.model.Calls()[0].Messages[0].Content
	if !strings.Contains(prompt, "- my-notes: Notes.") || !strings.Contains(prompt, "User notes text.") || strings.Contains(prompt, `<skill source="user"`) {
		t.Fatalf("prompt %q", prompt)
	}
	sb := r.sandbox()
	if md, _ := sb.File("/skills/my-notes/SKILL.md"); !strings.Contains(md, "User notes text.") {
		t.Fatalf("installed %q", md)
	}
	if _, ok := sb.File("/skills/my-notes/scripts/count.py"); !ok {
		t.Fatal("script not installed")
	}
}

func TestUserDirectorySkillOverridesABuiltinOfTheSameName(t *testing.T) {
	// Later sources win: a user skill named like a built-in replaces it in the index.
	// Arrange
	r := newScriptedRig(t, false, llmtest.Turn{Chunks: llmtest.Text("Done")})
	r.reg.AddFS(skills.SourceUser, fstest.MapFS{
		"fixture-doc/SKILL.md": {Data: []byte("---\nname: fixture-doc\ndescription: My own fixture doc.\n---\n\nMine.\n")},
	})
	_, run := r.queued("hello")

	// Act
	r.process(run)

	// Assert
	if prompt := r.model.Calls()[0].Messages[0].Content; !strings.Contains(prompt, "- fixture-doc: My own fixture doc.") || strings.Contains(prompt, "Create fixture documents for tests.") {
		t.Fatalf("prompt %q", prompt)
	}
}
