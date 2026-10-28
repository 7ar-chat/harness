//go:build unix

package harness_test

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness"
	"github.com/GhaziBenDahmane/harness/agent"
	"github.com/GhaziBenDahmane/harness/agent/prompts"
	"github.com/GhaziBenDahmane/harness/compaction"
	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/store"
	"github.com/GhaziBenDahmane/harness/tools"
)

// fixedOverhead is what every request of a profile carries before the user speaks.
type fixedOverhead struct {
	Tools        []*schema.ToolInfo
	ToolTokens   map[string]int
	Sections     []string // names, in prompt order
	SectionTok   map[string]int
	BaseTokens   int
	System       string
	TotalTools   int
	TotalSystem  int
	ToolsJSONLen int
}

// sectionNames name harnessOptions' Sections, in order.
var sectionNames = []string{"skills", "artifacts", "git", "plan/ask_user", "memory", "goals"}

// measure builds the real tool list and system prompt of a profile with a
// local sandbox backend and a fake model.
func measure(t *testing.T, env map[string]string) fixedOverhead {
	t.Helper()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	t.Setenv("SANDBOX_BACKEND", "local")
	t.Setenv("LOCAL_WORKSPACE", t.TempDir())
	t.Setenv("LOCAL_STATE_DIR", t.TempDir())
	for k, v := range env {
		t.Setenv(k, v)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	h, err := harness.New(harness.Options{Config: cfg, Models: llmtest.Factory{M: llmtest.New()}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	list, cleanup, err := h.Agent.Tools.Tools(ctx, tools.RunContext{ThreadID: "t1", RunID: "r1", Login: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	out := fixedOverhead{ToolTokens: map[string]int{}, SectionTok: map[string]int{}}
	for _, tl := range list {
		info, err := tl.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out.Tools = append(out.Tools, info)
		out.ToolTokens[info.Name] = compaction.EstimateTools([]*schema.ToolInfo{info})
	}
	out.TotalTools = compaction.EstimateTools(out.Tools)
	b, _ := json.Marshal(out.Tools)
	out.ToolsJSONLen = len(b)

	model := "gpt-test"
	variant := prompts.Resolve(h.Agent.Harness.PromptVariant(model))
	in := agent.Input{Run: &store.Run{ID: "r1", ThreadID: "t1", UserLogin: "alice", Model: model}, Sandbox: true, Variant: variant}
	var sections []string
	for i, s := range h.Agent.Harness.Sections {
		text := strings.TrimSpace(s(ctx, in))
		name := fmt.Sprintf("section %d", i)
		if i < len(sectionNames) {
			name = sectionNames[i]
		}
		out.SectionTok[name] = compaction.EstimateText(text)
		out.Sections = append(out.Sections, name)
		if text != "" {
			sections = append(sections, text)
		}
	}
	out.System = prompts.Prompt{Variant: variant, Sandbox: true, Sections: sections}.Render("")
	out.BaseTokens = compaction.EstimateText(prompts.Prompt{Variant: variant, Sandbox: true}.Render(""))
	out.TotalSystem = compaction.EstimateText(out.System)
	return out
}

func (o fixedOverhead) report(t *testing.T, title string) {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "\n%s\n  %-28s %6s\n", title, "tool", "tokens")
	for _, info := range o.Tools {
		fmt.Fprintf(&b, "  %-28s %6d\n", info.Name, o.ToolTokens[info.Name])
	}
	fmt.Fprintf(&b, "  %-28s %6d  (%d tools, %d bytes JSON)\n", "TOOLS TOTAL", o.TotalTools, len(o.Tools), o.ToolsJSONLen)
	fmt.Fprintf(&b, "  %-28s %6d\n", "prompt: base+context", o.BaseTokens)
	for _, n := range o.Sections {
		fmt.Fprintf(&b, "  %-28s %6d\n", "prompt section: "+n, o.SectionTok[n])
	}
	fmt.Fprintf(&b, "  %-28s %6d  (%d bytes)\n", "SYSTEM TOTAL", o.TotalSystem, len(o.System))
	fmt.Fprintf(&b, "  %-28s %6d\n", "FIXED OVERHEAD", o.TotalTools+o.TotalSystem)
	t.Log(b.String())
}

func TestPromptSizeReport(t *testing.T) {
	// Arrange
	profile := cmp.Or(os.Getenv("PROMPTSIZE_PROFILE"), "coding")

	// Act
	o := measure(t, map[string]string{"AGENT_PROFILE": profile})

	// Assert
	o.report(t, "profile="+profile)
	if o.TotalTools == 0 || o.TotalSystem == 0 {
		t.Fatal("empty measurement")
	}
}
