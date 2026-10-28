package runtime_test

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/agent"
	"github.com/GhaziBenDahmane/harness/agent/prompts"
	"github.com/GhaziBenDahmane/harness/artifacts"
	"github.com/GhaziBenDahmane/harness/compaction"
	"github.com/GhaziBenDahmane/harness/goals"
	"github.com/GhaziBenDahmane/harness/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/memory"
	"github.com/GhaziBenDahmane/harness/skills"
	skillsfs "github.com/GhaziBenDahmane/harness/skills/skillsfs"
	"github.com/GhaziBenDahmane/harness/store"
	"github.com/GhaziBenDahmane/harness/tools/harnesstools"
	"github.com/GhaziBenDahmane/harness/websearch"
	"github.com/GhaziBenDahmane/harness/workspace"
)

var updatePrompts = flag.Bool("update-prompts", false, "rewrite testdata/prompts/*.golden")

// promptRig wires the prompt sections like cmd/work/main.go (stream C2) with
// a variant; the run has a goal and a recalled memory.
func promptRig(t *testing.T, variant string, turns ...llmtest.Turn) (*v2rig, *store.Run) {
	t.Helper()
	r := newV2Rig(t, false, turns...)
	h := &r.agent.Harness
	h.Sections = []agent.PromptSection{
		r.reg.PromptSection,
		agent.Legacy(artifacts.PromptSection, ""),
		agent.Legacy(func(context.Context, agent.Input) string { return harnesstools.Instructions }, ""),
		memory.PromptSection(r.mem),
		goals.PromptSection(r.st),
	}
	h.Context = []agent.PromptSection{goals.ContextSection(r.st), memory.RecallSection(r.mem)}
	h.PromptVariant = func(string) string { return variant }
	r.remember("Prefers tables over charts", "memories/semantic/preferences/tables.md")
	ctx := context.Background()
	th, _ := r.st.CreateThread(ctx, "alice", "t")
	g, err := r.st.PutGoal(ctx, "alice", th.ID, "Produce /workspace/outputs/q3.xlsx", 5)
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := r.st.CreateRun(ctx, goals.NewRun(g, "alice", "gpt-test", "", "", goals.StartMessage(g.Text)))
	if err != nil {
		t.Fatal(err)
	}
	return r, run
}

func golden(t *testing.T, name, got string) string {
	t.Helper()
	p := filepath.Join("testdata", "prompts", name+".golden")
	if *updatePrompts {
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("missing %s (run with -update-prompts): %v", p, err)
	}
	return string(want)
}

func TestLegacyPromptVariantIsTheV2PromptVerbatim(t *testing.T) {
	// The legacy variant renders exactly the harness v2 system prompt (golden produced by the v2 code at a6c04d7b0 with the same skills, memory and goal, plus stream A's 50 MiB sentence in the file-reference section, round 3's language rule and the lazy-skills wording of load_skill), kept for A/B runs.
	// Arrange
	r, run := promptRig(t, prompts.Legacy, llmtest.Turn{Chunks: llmtest.Text("Hi")})

	// Act
	r.process(run)

	// Assert
	got := r.model.Calls()[0].Messages[0].Content
	if want := golden(t, "legacy", got); got != want {
		t.Fatalf("legacy prompt differs from v2.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestPromptVariantsRenderTheirReviewedText(t *testing.T) {
	// Each v3 variant renders its reviewed prompt for a typical run (skills index, memory, goal and recall in the closing context); run with -update-prompts after an intended change.
	for _, variant := range []string{prompts.Default, prompts.GPT56, prompts.DeepSeek} {
		t.Run(variant, func(t *testing.T) {
			// Arrange
			r, run := promptRig(t, variant, llmtest.Turn{Chunks: llmtest.Text("Hi")})

			// Act
			r.process(run)

			// Assert
			got := r.model.Calls()[0].Messages[0].Content
			if want := golden(t, variant, got); got != want {
				t.Fatalf("prompt changed.\n--- got ---\n%s", got)
			}
			t.Logf("%s: %d words, ~%d tokens", variant, len(strings.Fields(got)), compaction.EstimateText(got))
		})
	}
}

func TestDeepSeekVariantKeepsOnlyTheNonNegotiables(t *testing.T) {
	// The deepseek variant has the identity, deliverables, the unchangeable rules, the skills index, the enabled optional parts (memory policy) and the context, and none of the working-style guidance (no how-to-work list).
	// Arrange
	r, run := promptRig(t, prompts.DeepSeek, llmtest.Turn{Chunks: llmtest.Text("Hi")})

	// Act
	r.process(run)

	// Assert
	sys := r.model.Calls()[0].Messages[0].Content
	for _, want := range []string{"You are Work", "<deliverables>", "<rules_that_never_change>", "<skills>", "<context>", "Goal (", "<user_memories>", "<memory>"} {
		if !strings.Contains(sys, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, not := range []string{"<how_you_work>", "<conversation>", "update_plan"} {
		if strings.Contains(sys, not) {
			t.Errorf("unexpected %q", not)
		}
	}
}

func TestSystemPromptIsByteStableAcrossTheStepsOfARun(t *testing.T) {
	// Every model call of a run sends the same system prompt (static part first, dynamic context last), so the provider's prefix cache keeps hitting.
	// Arrange
	r, run := promptRig(t, prompts.Default,
		llmtest.Turn{Chunks: streamedCall("c1", "list_files", `{}`)},
		llmtest.Turn{Chunks: streamedCall("c2", "list_files", `{"depth":1}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
	)

	// Act
	r.process(run)

	// Assert
	calls := r.model.Calls()
	if len(calls) < 3 {
		t.Fatalf("calls %d", len(calls))
	}
	first := calls[0].Messages[0].Content
	for i, c := range calls[1:3] {
		if c.Messages[0].Content != first {
			t.Fatalf("system prompt of call %d differs", i+2)
		}
	}
	if !strings.HasSuffix(first, "</context>") || strings.Index(first, "Date (UTC)") < strings.Index(first, "</skills>") {
		t.Fatalf("dynamic context not last: %q", first)
	}
}

func TestPromptSizesOfATypicalRun(t *testing.T) {
	// For a typical run (the six built-in skills, memory with one recalled entry, goal, git checkpoints, web search) every v3 variant is at most half the size of the legacy prompt; sizes are logged for the docs.
	sizes := map[string]int{}
	for _, variant := range []string{prompts.Legacy, prompts.Default, prompts.GPT56, prompts.DeepSeek} {
		// Arrange
		r, run := promptRig(t, variant, llmtest.Turn{Chunks: llmtest.Text("Hi")})
		r.reg = skills.NewRegistry(skillsfs.Builtin(), r.st)
		h := &r.agent.Harness
		h.Sections[0] = r.reg.PromptSection
		h.Sections = append(h.Sections,
			agent.Part(prompts.Git, agent.Legacy(func(_ context.Context, in agent.Input) string { return workspace.GitPrompt }, prompts.GitCheckpoints)),
			agent.Part(prompts.Web, agent.Legacy(func(context.Context, agent.Input) string { return websearch.Instructions }, prompts.WebSearch)))
		legacyOrder := h.Sections
		if variant == prompts.Legacy { // v2 order: skills, file refs, git, plans, memory, goal, web
			legacyOrder = []agent.PromptSection{h.Sections[0], h.Sections[1], h.Sections[5], h.Sections[2], h.Sections[3], h.Sections[4], h.Sections[6]}
		}
		h.Sections = legacyOrder

		// Act
		r.process(run)

		// Assert
		call := r.model.Calls()[0]
		sys := call.Messages[0].Content
		sizes[variant] = compaction.EstimateText(sys)
		if dir := os.Getenv("WORK_PROMPT_DUMP"); dir != "" { // measurement aid (docs): the rendered prompts
			_ = os.WriteFile(filepath.Join(dir, variant+".txt"), []byte(sys), 0o644)
		}
		t.Logf("%-8s system prompt: %4d words, ~%4d tokens; tool schemas ~%d tokens (%d tools)", variant, len(strings.Fields(sys)),
			sizes[variant], compaction.EstimateTools(call.Tools), len(call.Tools))
	}
	for _, v := range []string{prompts.Default, prompts.GPT56, prompts.DeepSeek} {
		if sizes[v]*2 > sizes[prompts.Legacy] {
			t.Errorf("%s prompt ~%d tokens is not under half the legacy ~%d", v, sizes[v], sizes[prompts.Legacy])
		}
	}
}

func TestEveryPromptVariantAnswersInTheLanguageOfTheLatestUserMessage(t *testing.T) {
	// Every prompt variant tells the model to answer in the language of the user's latest message, whatever the language of the files (A8: an English request about a French file was answered in French).
	for _, variant := range []string{prompts.Default, prompts.GPT56, prompts.DeepSeek, prompts.Legacy} {
		t.Run(variant, func(t *testing.T) {
			// Arrange
			r, run := promptRig(t, variant, llmtest.Turn{Chunks: llmtest.Text("Hi")})

			// Act
			r.process(run)

			// Assert
			if sys := r.model.Calls()[0].Messages[0].Content; !strings.Contains(sys, "Answer in the language of the user's latest message, whatever the language of the files") {
				t.Fatalf("%s prompt has no language rule", variant)
			}
		})
	}
}

func TestEveryV3PromptVariantAsksToCrossCheckTheDeliverableAgainstItsSources(t *testing.T) {
	// The deliverable rules of every v3 variant (not legacy, kept verbatim) tell the model to cross-check every date, figure and named item of the deliverable against the sources before finishing and to say what it cross-checked (A10).
	for _, variant := range []string{prompts.Default, prompts.GPT56, prompts.DeepSeek, prompts.Legacy} {
		t.Run(variant, func(t *testing.T) {
			// Arrange
			r, run := promptRig(t, variant, llmtest.Turn{Chunks: llmtest.Text("Hi")})

			// Act
			r.process(run)

			// Assert
			sys := r.model.Calls()[0].Messages[0].Content
			rule := "Before you finish, cross-check every date, figure and named item in the deliverable against the sources, and say in your answer what you cross-checked."
			if strings.Contains(sys, rule) != (variant != prompts.Legacy) {
				t.Fatalf("%s prompt: cross-check rule present=%v", variant, strings.Contains(sys, rule))
			}
		})
	}
}
