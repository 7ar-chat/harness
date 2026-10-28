package runtime_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/sandbox"
	"github.com/GhaziBenDahmane/harness/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/store"
)

func streamedCall(id, name string, argParts ...string) []*schema.Message {
	i := 0
	out := []*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{Index: &i, ID: id, Type: "function", Function: schema.FunctionCall{Name: name}}}}}
	for _, p := range argParts {
		out = append(out, &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{Index: &i, Function: schema.FunctionCall{Arguments: p}}}})
	}
	return out
}

func TestProcessRunsSandboxCommandAndFeedsResultBack(t *testing.T) {
	// A streamed execute_command call runs in a lazily created sandbox; its live output and result are emitted and returned to the model.
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: streamedCall("call_1", "execute_command", `{"comm`, `and":"echo hi"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
	)
	r.backend.OnCreate = func(sb *sandboxtest.Sandbox) {
		sb.SetExec(func(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
			req.OnStdout("hi\n")
			return sandbox.ExecResult{ExitCode: 0}, nil
		})
	}
	th, run := r.queued("say hi in bash")

	// Act
	got := r.process(run)

	// Assert
	evs := r.events(run.ID)
	want := []string{"RUN_STARTED", "STEP_STARTED", "TOOL_CALL_START", "TOOL_CALL_ARGS", "TOOL_CALL_ARGS", "TOOL_CALL_END",
		"CUSTOM:work.tool_output", "TOOL_CALL_RESULT", "CUSTOM:work.usage", "STEP_FINISHED",
		"STEP_STARTED", "TEXT_MESSAGE_START", "TEXT_MESSAGE_CONTENT", "TEXT_MESSAGE_END", "CUSTOM:work.usage", "STEP_FINISHED", "RUN_FINISHED"}
	if !equal(types(evs), want) || got.Status != store.StatusCompleted {
		t.Fatalf("status %s types %v", got.Status, types(evs))
	}
	start := find(evs, "TOOL_CALL_START")[0]
	if start["toolCallId"] != "call_1" || start["toolCallName"] != "execute_command" || start["parentMessageId"] == "" {
		t.Fatalf("start %v", start)
	}
	out := find(evs, "CUSTOM:work.tool_output")[0]["value"].(map[string]any)
	if out["toolCallId"] != "call_1" || out["stream"] != "stdout" || out["text"] != "hi\n" {
		t.Fatalf("tool output %v", out)
	}
	res := find(evs, "TOOL_CALL_RESULT")[0]
	var result struct {
		Stdout   string `json:"stdout"`
		ExitCode int    `json:"exitCode"`
	}
	_ = json.Unmarshal([]byte(res["content"].(string)), &result)
	if res["toolCallId"] != "call_1" || res["role"] != "tool" || result.Stdout != "hi\n" || res["metadata"] != nil {
		t.Fatalf("result %v", res)
	}
	sb := r.backend.Created()
	if len(sb) != 1 || sb[0].Execs()[0].Command != "echo hi" || sb[0].Execs()[0].Cwd != "/workspace" {
		t.Fatalf("sandboxes %v", sb)
	}
	if md := r.backend.CreateOptions()[0].Metadata; md["work-thread"] != th.ID || len(md["work-user"]) != 40 || md["work-user"] == "alice" {
		t.Fatalf("metadata %v", md)
	}
	second := r.model.Calls()[1].Messages
	toolMsg := second[len(second)-1]
	if toolMsg.Role != schema.Tool || toolMsg.ToolCallID != "call_1" || !strings.Contains(toolMsg.Content, `"stdout":"hi\n"`) {
		t.Fatalf("model did not get the tool result: %+v", toolMsg)
	}
	msgs := r.messages(th.ID)
	roles := []string{}
	for _, m := range msgs {
		roles = append(roles, m.Role)
	}
	if !equal(roles, []string{"user", "assistant", "tool", "assistant"}) || msgs[1].ToolCalls[0].Function.Arguments != `{"command":"echo hi"}` ||
		msgs[2].ToolCallID != "call_1" {
		t.Fatalf("messages %v %+v", roles, msgs)
	}
}

func TestProcessReturnsToolErrorsToTheModel(t *testing.T) {
	// Unknown tools and invalid JSON arguments become error results for the model, not run failures.
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "c1", "delete_everything", `{}`), llmtest.ToolCall(1, "c2", "execute_command", `{bad`)}},
		llmtest.Turn{Chunks: llmtest.Text("Sorry")},
	)
	th, run := r.queued("q")

	// Act
	got := r.process(run)

	// Assert
	results := find(r.events(run.ID), "TOOL_CALL_RESULT")
	if got.Status != store.StatusCompleted || len(results) != 2 {
		t.Fatalf("status %s results %v", got.Status, results)
	}
	for _, res := range results {
		if res["metadata"].(map[string]any)["isError"] != true {
			t.Fatalf("result not flagged as error: %v", res)
		}
	}
	if !strings.Contains(results[0]["content"].(string), "unknown tool") || !strings.Contains(results[1]["content"].(string), "not valid JSON") {
		t.Fatalf("results %v", results)
	}
	msgs := r.messages(th.ID)
	if msgs[2].Error == "" || msgs[3].Error == "" {
		t.Fatalf("tool messages without error: %+v %+v", msgs[2], msgs[3])
	}
	if len(r.backend.Created()) != 0 {
		t.Fatal("a sandbox was created for invalid calls")
	}
}

func TestProcessCapsToolOutputGivenToTheModel(t *testing.T) {
	// Tool output beyond AGENT_MAX_TOOL_OUTPUT_CHARS (the spill threshold never exceeds it) is cut to head and tail with a note that the full output was not kept (no spiller here).
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "c1", "read_file", `{"path":"big.txt"}`)}},
		llmtest.Turn{Chunks: llmtest.Text("ok")},
	)
	r.agent.MaxToolOutputChars = 500
	r.backend.OnCreate = func(sb *sandboxtest.Sandbox) {
		sb.Put("/workspace/big.txt", "FIRST\n"+strings.Repeat("line\n", 1000)+"LAST")
	}
	_, run := r.queued("read it")

	// Act
	r.process(run)

	// Assert
	content := find(r.events(run.ID), "TOOL_CALL_RESULT")[0]["content"].(string)
	if len([]rune(content)) > 500 || !strings.Contains(content, "FIRST") || !strings.Contains(content, "LAST") || !strings.Contains(content, "characters not shown; the full output could not be kept") {
		t.Fatalf("content (%d chars): %q", len(content), content)
	}
}
