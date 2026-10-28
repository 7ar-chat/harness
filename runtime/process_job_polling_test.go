package runtime_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/store"
)

// lastToolResult is the content of the newest tool message of a model call.
func lastToolResult(c llmtest.Call) string {
	for i := len(c.Messages) - 1; i >= 0; i-- {
		if c.Messages[i].Role == schema.Tool {
			return c.Messages[i].Content
		}
	}
	return ""
}

func TestPollingARunningJobIsNeverRefusedAsALoop(t *testing.T) {
	// Identical job_output polls of a job that is still running are all executed (no loop-guard refusal), until the job reports finished.
	// Arrange
	release := make(chan struct{})
	r := newRig(t)
	r.agent.MaxSteps = 8
	r.backend.OnCreate = func(sb *sandboxtest.Sandbox) {
		sb.SetJobs(func(ctx context.Context, _ string, out func(string, string)) int {
			out("stdout", "building\n")
			select {
			case <-release:
				return 0
			case <-ctx.Done():
				return 143
			}
		})
	}
	jobID, polls := "", 0
	r.model = llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		switch {
		case jobID == "" && lastToolResult(c) == "":
			return llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "x1", "execute_command", `{"command":"make all","background":true}`)}}
		case jobID == "":
			var res struct {
				JobID string `json:"jobId"`
			}
			_ = json.Unmarshal([]byte(lastToolResult(c)), &res)
			jobID = res.JobID
		case strings.Contains(lastToolResult(c), `"status":"finished"`):
			return llmtest.Turn{Chunks: llmtest.Text("The build finished.")}
		}
		polls++
		if polls == 4 {
			close(release) // the 4th poll finds the job finished
			time.Sleep(50 * time.Millisecond)
		}
		return llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "p"+string(rune('0'+polls)), "job_output", `{"jobId":"`+jobID+`"}`)}}
	})
	r.agent.Models = llmtest.Factory{M: r.model}
	_, run := r.queued("build it")

	// Act
	got := r.process(run)

	// Assert
	results := find(r.events(run.ID), "TOOL_CALL_RESULT")
	if got.Status != store.StatusCompleted || len(results) != 5 {
		t.Fatalf("status %s results %d", got.Status, len(results))
	}
	for _, res := range results[1:] {
		if c := res["content"].(string); strings.Contains(c, "Not executed") {
			t.Fatalf("a poll was refused: %s", c)
		}
	}
	if c := results[4]["content"].(string); !strings.Contains(c, `"status":"finished"`) {
		t.Fatalf("last poll %s", c)
	}
}
