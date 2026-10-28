package runtime_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/store"
)

// dumpCallUsing is dumpCall whose model call reports prompt tokens of its request.
func dumpCallUsing(n, promptTokens int) llmtest.Turn {
	c := llmtest.ToolCall(0, fmt.Sprintf("d%d", n), "dump", fmt.Sprintf(`{"n":%d}`, n))
	return llmtest.Turn{Chunks: []*schema.Message{llmtest.WithUsage(c, promptTokens, 10)}}
}

// stubCount counts the masked tool results of a request.
func stubCount(c llmtest.Call) int {
	n := 0
	for _, m := range c.Messages {
		if m.Role == schema.Tool && strings.HasPrefix(m.Content, "[dump result,") {
			n++
		}
	}
	return n
}

// prefixKept reports whether cur starts with prev, message for message.
func prefixKept(prev, cur llmtest.Call) bool {
	if len(cur.Messages) < len(prev.Messages) {
		return false
	}
	for j := range prev.Messages {
		if !sameMessage(prev.Messages[j], cur.Messages[j]) {
			return false
		}
	}
	return true
}

func TestProcessNeverMasksToolResultsAtLowContext(t *testing.T) {
	// With masking configured (keep 2), a run whose prompt stays below COMPACTION_MASK_FROM_FRACTION (0.5) × window masks nothing: every request starts with the previous request byte for byte, so the prompt cache keeps hitting (A1).
	// Arrange
	r := newRig(t, dumpCall(1), dumpCall(2), dumpCall(3), dumpCall(4), dumpCall(5), dumpCall(6), llmtest.Turn{Chunks: llmtest.Text("done")})
	r.agent.MaxSteps = 8
	r.agent.Harness.KeepRecentToolResults = 2
	r.withTools(dumpTool())
	_, run := r.queued("dump everything")

	// Act
	got := r.process(run)

	// Assert
	checkStrictOrder(t, r.events(run.ID))
	calls := r.model.Calls()
	if got.Status != store.StatusCompleted || len(calls) != 7 {
		t.Fatalf("status %s calls %d", got.Status, len(calls))
	}
	for i := 1; i < len(calls); i++ {
		if n := stubCount(calls[i]); n != 0 {
			t.Fatalf("call %d: %d masked results at low context", i, n)
		}
		if !prefixKept(calls[i-1], calls[i]) {
			t.Fatalf("call %d rewrote the previous request", i)
		}
	}
}

func TestProcessMasksInOneBatchOnceThePromptPassesTheMaskFraction(t *testing.T) {
	// Once the reported prompt passes COMPACTION_MASK_FROM_FRACTION × window, every tool result but the newest COMPACTION_KEEP_RECENT_TOOL_RESULTS is masked in one batch; the masked prefix then stays byte-identical while the prompt stays below the fraction (A1).
	// Arrange
	r := newRig(t, dumpCallUsing(1, 3000), dumpCallUsing(2, 3500), dumpCallUsing(3, 4000), dumpCallUsing(4, 70000),
		dumpCallUsing(5, 30000), dumpCallUsing(6, 31000), llmtest.Turn{Chunks: llmtest.Text("done")})
	r.agent.MaxSteps = 8
	r.agent.Harness.KeepRecentToolResults = 2
	r.agent.Harness.MaskFromFraction = 0.5 // window 128000: masking from 64000 prompt tokens
	r.withTools(dumpTool())
	_, run := r.queued("dump everything")

	// Act
	got := r.process(run)

	// Assert
	checkStrictOrder(t, r.events(run.ID))
	calls := r.model.Calls()
	if got.Status != store.StatusCompleted || len(calls) != 7 {
		t.Fatalf("status %s calls %d", got.Status, len(calls))
	}
	// Call 4 follows the call that reported 70000 tokens: d1 and d2 (all but the newest 2 of 4) are masked at once.
	for k, want := range map[int]int{1: 0, 2: 0, 3: 0, 4: 2, 5: 2, 6: 2} {
		if n := stubCount(calls[k]); n != want {
			t.Fatalf("call %d: %d stubs, want %d", k, n, want)
		}
	}
	for i := 1; i < len(calls); i++ {
		if i != 4 && !prefixKept(calls[i-1], calls[i]) {
			t.Fatalf("call %d rewrote the previous request outside the masking batch", i)
		}
	}
}
