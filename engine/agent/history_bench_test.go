package agent_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/agent"
	"github.com/GhaziBenDahmane/harness/engine/compaction"
)

// thread builds n steps of a coding session: an assistant tool call and a 6 kB tool result each.
func thread(n int) []*store.Message {
	out := []*store.Message{{ID: "u0", Role: store.RoleUser, Content: "fix the bug", Position: 1}}
	result := strings.Repeat("func Handle(ctx context.Context) error { return nil }\n", 110)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("c%d", i)
		out = append(out,
			&store.Message{ID: "a" + id, Role: store.RoleAssistant, Content: "reading", Position: int64(2 + 2*i),
				ToolCalls: []store.ToolCall{{ID: id, Type: "function", Function: store.ToolCallFunction{Name: "read_file", Arguments: `{"path":"/workspace/a.go"}`}}}},
			&store.Message{ID: "t" + id, Role: store.RoleTool, ToolCallID: id, Content: result, Position: int64(3 + 2*i)})
	}
	return out
}

// BenchmarkRunStartHistory is what every run does before its first model call: rebuild the
// provider history from the stored messages and estimate its tokens.
func BenchmarkRunStartHistory(b *testing.B) {
	for _, n := range []int{50, 500, 2000} {
		msgs := thread(n)
		b.Run(fmt.Sprintf("%d-steps", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				h := agent.BuildHistory(msgs)
				_ = compaction.EstimateMessages(h)
			}
		})
	}
}
