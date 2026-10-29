package runtime_test

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"github.com/GhaziBenDahmane/harness/engine/agui"
	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/engine/tools"
	"github.com/GhaziBenDahmane/harness/store"
)

// delegateProvider offers a tool that reports sub-agent activity like explore does.
type delegateProvider struct{}

func (delegateProvider) Name() string { return "delegate" }

func (delegateProvider) Tools(context.Context, tools.RunContext) ([]tool.BaseTool, func(), error) {
	t, err := utils.InferTool("delegate", "runs a sub-agent", func(ctx context.Context, in struct{}) (string, error) {
		id, _ := tools.CallFromContext(ctx)
		tools.Live(ctx, agui.SubagentEvent(map[string]any{"toolCallId": id, "phase": agui.SubagentStart, "agent": "explore", "task": "find it"}))
		tools.Live(ctx, agui.SubagentEvent(map[string]any{"toolCallId": id, "phase": agui.SubagentEnd, "agent": "explore"}))
		tools.SubagentUsage(ctx, "cheap-model", 100, 20, 40)
		return "report", nil
	})
	return []tool.BaseTool{t}, nil, err
}

func TestProcessEmitsSubagentEventsLiveAndCountsTheirUsage(t *testing.T) {
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: streamedCall("call_1", "delegate", `{}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
	)
	r.agent.Tools = tools.NewRegistry(delegateProvider{})
	_, run := r.queued("look around")

	// Act
	got := r.process(run)

	// Assert
	evs := r.events(run.ID)
	subs := find(evs, "CUSTOM:work.subagent")
	if got.Status != store.StatusCompleted || len(subs) != 2 {
		t.Fatalf("status %s, %d subagent events in %v", got.Status, len(subs), types(evs))
	}
	if v := subs[0]["value"].(map[string]any); v["toolCallId"] != "call_1" || v["phase"] != "start" {
		t.Fatalf("first event %v", v)
	}
	var found map[string]any
	for _, u := range find(evs, "RUN_FINISHED")[0]["usage"].([]any) {
		if m := u.(map[string]any); m["model"] == "cheap-model (sub-agent)" {
			found = m
		}
	}
	if found == nil || found["inputTokens"] != float64(100) || found["outputTokens"] != float64(20) || found["cachedInputTokens"] != float64(40) {
		t.Fatalf("sub-agent usage %v in %v", found, find(evs, "RUN_FINISHED"))
	}
}
