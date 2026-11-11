package compaction_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/compaction"
)

// session builds n steps of a coding session: an assistant tool call and a 6 kB tool result each.
func session(n int) []*ai.Message {
	out := []*ai.Message{ai.UserMessage("fix the failing test in the handler package")}
	compaction.MarkRequest(out[0])
	result := strings.Repeat("func Handle(ctx context.Context) error { return nil }\n", 110)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("call-%d", i)
		out = append(out,
			ai.AssistantMessage("reading the file", []ai.ToolCall{{ID: id, Type: "function",
				Function: ai.FunctionCall{Name: "read_file", Arguments: `{"path":"/workspace/handler.go"}`}}}),
			ai.ToolMessage(result, id, ai.WithToolName("read_file")))
	}
	for i, m := range out {
		compaction.SetPosition(m, int64(i+1))
	}
	return out
}

// BenchmarkEstimateText is the token estimate of a text, run on every message before each model call.
func BenchmarkEstimateText(b *testing.B) {
	for _, name := range []string{"english", "json", "python", "chinese"} {
		raw, err := os.ReadFile(filepath.Join("testdata", "tokens", name+".txt"))
		if err != nil {
			b.Fatal(err)
		}
		text := string(raw)
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(text)))
			for b.Loop() {
				_ = compaction.EstimateText(text)
			}
		})
	}
}

// BenchmarkEstimateMessages is the token estimate of a whole prompt.
func BenchmarkEstimateMessages(b *testing.B) {
	for _, n := range []int{50, 500} {
		msgs := session(n)
		b.Run(fmt.Sprintf("%d-steps", n), func(b *testing.B) {
			for b.Loop() {
				_ = compaction.EstimateMessages(msgs)
			}
		})
	}
}

// BenchmarkCompactShrink is the first compaction pass: old tool results over the threshold become previews.
func BenchmarkCompactShrink(b *testing.B) {
	msgs := session(200)
	window := compaction.EstimateMessages(msgs) * 2
	ctx := context.Background()
	for b.Loop() {
		res, err := compaction.Compact(ctx, msgs, compaction.Options{Window: window, Threshold: 0.4})
		if err != nil {
			b.Fatal(err)
		}
		_ = res
	}
}

// BenchmarkCompactDropHead is a forced compaction without a summarizer: the head of the thread is dropped.
func BenchmarkCompactDropHead(b *testing.B) {
	msgs := session(200)
	ctx := context.Background()
	for b.Loop() {
		res, err := compaction.Compact(ctx, msgs, compaction.Options{Window: 60000, Force: true})
		if err != nil {
			b.Fatal(err)
		}
		_ = res
	}
}

// BenchmarkTranscript renders a thread as text for the summarizer.
func BenchmarkTranscript(b *testing.B) {
	msgs := session(200)
	for b.Loop() {
		_ = compaction.Transcript(msgs)
	}
}
