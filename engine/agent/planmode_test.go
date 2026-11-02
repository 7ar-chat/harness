package agent

import (
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/core/store"
)

func TestPlanRunsKeepOnlyReadOnlyToolsAndThePlanAndQuestionTools(t *testing.T) {
	for name, tc := range map[string]struct {
		tool ai.BaseTool
		name string
		want bool
	}{
		"read-only tool":           {roTool{ro: true}, "read_file", true},
		"tool that says it writes": {roTool{ro: false}, "write_file", false},
		"tool with no declaration": {plainTool{}, "execute_command", false},
		"update_plan":              {plainTool{}, "update_plan", true},
		"ask_user":                 {plainTool{}, "ask_user", true},
		"memory write":             {plainTool{}, "memory_create", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := planAllows(tc.tool, tc.name); got != tc.want {
				t.Fatalf("planAllows(%s) = %v", tc.name, got)
			}
		})
	}
}

func TestPlanModePromptOnlyForPlanRuns(t *testing.T) {
	for mode, want := range map[string]string{store.ModePlan: PlanModePrompt, store.ModeBuild: "", "": ""} {
		if got := planModeSection(Input{Run: &store.Run{Mode: mode}}); got != want {
			t.Errorf("mode %q: section %q", mode, got)
		}
	}
	if got := planModeSection(Input{}); got != "" {
		t.Errorf("no run: %q", got)
	}
}
