package ai_test

import (
	"fmt"
	"testing"

	"github.com/7ar-chat/harness/ai"
)

// BenchmarkConcatMessages merges the chunks of a streamed answer: text deltas, then tool-call fragments.
func BenchmarkConcatMessages(b *testing.B) {
	for _, n := range []int{100, 1000} {
		chunks := make([]*ai.Message, 0, n+n/10)
		for i := 0; i < n; i++ {
			chunks = append(chunks, &ai.Message{Role: ai.RoleAssistant, Content: "some streamed text delta ", ReasoningContent: "thinking "})
		}
		for i := 0; i < n/10; i++ {
			idx := i % 3
			chunks = append(chunks, &ai.Message{Role: ai.RoleAssistant, ToolCalls: []ai.ToolCall{{Index: &idx,
				ID: fmt.Sprintf("call-%d", idx), Type: "function",
				Function: ai.FunctionCall{Name: "bash", Arguments: `{"command":"ls"}`}}}})
		}
		b.Run(fmt.Sprintf("%d-chunks", n), func(b *testing.B) {
			for b.Loop() {
				if _, err := ai.ConcatMessages(chunks); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
