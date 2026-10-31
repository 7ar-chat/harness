package agent

import (
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/tools"
)

// planModeTools are the tools a plan run keeps although they are not
// tools.ReadOnly: they only touch the run's own plan and questions, never the
// workspace, the sandbox, memory or an external system.
var planModeTools = map[string]bool{"update_plan": true, "ask_user": true}

// PlanModePrompt is the system-prompt section of a plan run.
const PlanModePrompt = `Plan mode: you are read-only in this run. You cannot create, edit or delete files, run commands or change anything else; the tools that could were removed. Investigate with the tools you have (read, search, list, look things up), then answer with a concrete plan the user can approve: the goal, the files and components involved, the steps in order, the risks and how to check the result. Ask the user (ask_user) when a decision is theirs to make. Do not claim to have changed anything; the user will switch to build mode to carry the plan out.`

// isPlanRun reports a run in plan mode.
func isPlanRun(r *store.Run) bool { return r != nil && r.Mode == store.ModePlan }

// planModeSection is the prompt section of a plan run ("" for a build run).
func planModeSection(in Input) string {
	if isPlanRun(in.Run) {
		return PlanModePrompt
	}
	return ""
}

// planAllows reports whether a plan run keeps the tool named name: a tool that
// declares itself side-effect free (tools.ReadOnly, which includes MCP tools
// annotated readOnlyHint) or one of planModeTools. Command execution, file
// writes, memory writes and every tool that does not say it is read-only are
// dropped from the tool set, so the model cannot call them: the restriction is
// the tool set, not the prompt.
func planAllows(t ai.BaseTool, name string) bool {
	if ro, ok := t.(tools.ReadOnly); ok && ro.ReadOnly() {
		return true
	}
	return planModeTools[name]
}
