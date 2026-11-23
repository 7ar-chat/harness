package harness_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
)

type credFunc func(ctx context.Context, login, executor string) (map[string]string, error)

func (f credFunc) Credentials(ctx context.Context, login, executor string) (map[string]string, error) {
	return f(ctx, login, executor)
}

// A run whose model calls run_executor: the CLI runs in the run's sandbox with the user's sealed credentials in its
// environment only, its output reaches the model's tool result redacted, and the task is not in the command line.
func TestRunExecutorEndToEndThroughTheHarness(t *testing.T) {
	const key = "sk-live-this-is-a-secret-0123456789"
	var mu sync.Mutex
	var cliCmd string
	var cliEnv map[string]string
	be := sandboxtest.NewBackend()
	be.OnCreate = func(sb *sandboxtest.Sandbox) {
		sb.SetExec(func(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
			switch {
			case strings.HasPrefix(req.Command, "codex exec"):
				mu.Lock()
				cliCmd, cliEnv = req.Command, req.Env
				mu.Unlock()
				if req.OnStdout != nil {
					req.OnStdout(`{"type":"item.completed","item":{"type":"agent_message","text":"patched with ` + key + `"}}` + "\n")
					req.OnStdout(`{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":5}}` + "\n")
				}
			}
			return sandbox.ExecResult{}, nil
		})
	}
	model := llmtest.New(
		llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, "c1", "run_executor", `{"executor":"codex","task":"patch the bug"}`), "tool_calls")}},
		llmtest.Turn{Chunks: llmtest.Text("done")},
		llmtest.Turn{Chunks: llmtest.Text("A title")},
	)
	h, err := harness.New(harness.Options{
		Config: featureConfig(t, nil), Backend: be, Models: llmtest.Factory{M: model},
		Features: append(harness.ProfileCoding(), harness.Executors(harness.ExecutorsOptions{
			Credentials: credFunc(func(_ context.Context, login, name string) (map[string]string, error) {
				if login == "alice" && name == "codex" {
					return map[string]string{"OPENAI_API_KEY": key, "PATH": "/evil"}, nil
				}
				return nil, nil
			}),
		})),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go func() { _ = h.Run(ctx) }()
	th, _ := h.NewThread(ctx, "alice", "")
	run, err := h.Send(ctx, "alice", th.ID, "fix it", harness.RunOptions{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	var last string
	for ev := range h.Events(ctx, run.ID, 0) {
		last = ev.Type
	}
	if last != agui.RunFinished {
		t.Fatalf("last event %q", last)
	}

	mu.Lock()
	defer mu.Unlock()
	if cliCmd == "" {
		t.Fatal("the CLI never ran")
	}
	if strings.Contains(cliCmd, "patch the bug") || !strings.Contains(cliCmd, `"$HARNESS_TASK"`) {
		t.Errorf("command %q", cliCmd)
	}
	if cliEnv["HARNESS_TASK"] != "patch the bug" || cliEnv["OPENAI_API_KEY"] != key {
		t.Errorf("env %v", cliEnv)
	}
	if _, ok := cliEnv["PATH"]; ok {
		t.Error("an undeclared variable was injected")
	}
	// The second request carries the tool result: redacted, with the summary.
	calls := model.Calls()
	var all string
	for _, m := range calls[1].Messages {
		all += m.Content + "\n"
	}
	if strings.Contains(all, key) || !strings.Contains(all, "patched with ***") {
		t.Errorf("the tool result leaked or lacks the summary: %s", all)
	}
	// The tool is offered to the model, with the executors in its description.
	var found bool
	for _, ti := range calls[0].Tools {
		if ti.Name == "run_executor" && strings.Contains(ti.Desc, "claude-code") {
			found = true
		}
	}
	if !found {
		t.Error("run_executor was not offered")
	}
}

func TestExecutorsFeatureNeedsASandbox(t *testing.T) {
	_, err := harness.New(harness.Options{Config: featureConfig(t, nil), Features: []harness.Feature{harness.Executors(harness.ExecutorsOptions{})}})
	if err == nil || !strings.Contains(err.Error(), "executors") {
		t.Fatalf("err = %v", err)
	}
}
