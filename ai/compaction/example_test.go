package compaction_test

import (
	"context"
	"fmt"
	"strings"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/compaction"
)

// Compact leaves a conversation alone while it fits the window, and shrinks it when it does not: first old tool
// results become short previews, then everything before a recent tail is replaced by a summary. The caller supplies
// the model call that writes the summary.
func ExampleCompact() {
	history := []*ai.Message{ai.UserMessage("Find the bug in main.go")}
	for i := range 6 {
		id := fmt.Sprintf("call_%d", i)
		history = append(history,
			ai.AssistantMessage("", []ai.ToolCall{{ID: id, Function: ai.FunctionCall{Name: "read_file", Arguments: `{"path":"main.go"}`}}}),
			ai.ToolMessage(strings.Repeat("line of code\n", 400), id, ai.WithToolName("read_file")))
	}
	history = append(history, ai.AssistantMessage("The bug is in the loop bound.", nil))

	res, err := compaction.Compact(context.Background(), history, compaction.Options{
		Window:    8000, // the model's context window, in tokens
		Threshold: 0.75, // compact above this share of it
	})

	fmt.Println(res.Action, "| err:", err)
	fmt.Println("smaller:", res.TokensAfter < res.TokensBefore, "| messages:", len(history), "->", len(res.Messages))
	// Output:
	// shrunk | err: <nil>
	// smaller: true | messages: 14 -> 14
}

// When shrinking is not enough, or the caller asks (Manual), the older part of the conversation is handed to the
// Summarizer as a transcript and replaced by what it returns. The summary is for the caller to put back into the
// prompt (typically in the system message). Automatic compaction keeps a recent tail verbatim; Manual folds every
// message, so Result.Messages is empty here.
func ExampleCompact_summary() {
	history := []*ai.Message{ai.UserMessage("Find the bug in main.go")}
	for i := range 6 {
		id := fmt.Sprintf("call_%d", i)
		history = append(history,
			ai.AssistantMessage("", []ai.ToolCall{{ID: id, Function: ai.FunctionCall{Name: "read_file", Arguments: `{"path":"main.go"}`}}}),
			ai.ToolMessage(strings.Repeat("line of code\n", 400), id, ai.WithToolName("read_file")))
	}
	history = append(history, ai.AssistantMessage("The bug is in the loop bound.", nil))

	res, _ := compaction.Compact(context.Background(), history, compaction.Options{
		Window: 8000,
		Manual: true, // summarize now, whatever the size
		Summarize: func(_ context.Context, previous, transcript string) (string, error) {
			fmt.Println("summarizer sees a transcript with USER and TOOL RESULT turns:",
				strings.Contains(transcript, "USER:") && strings.Contains(transcript, "TOOL RESULT"))
			return "Goal: find the bug in main.go. Read main.go six times; the loop bound is wrong.", nil
		},
	})

	fmt.Println(res.Action, "|", len(history), "->", len(res.Messages), "messages")
	fmt.Println(res.Summary)
	// Output:
	// summarizer sees a transcript with USER and TOOL RESULT turns: true
	// summarized | 14 -> 0 messages
	// Goal: find the bug in main.go. Read main.go six times; the loop bound is wrong.
}

// A prompt that fits is returned unchanged.
func ExampleCompact_fits() {
	history := []*ai.Message{ai.UserMessage("hi"), ai.AssistantMessage("hello", nil)}

	res, _ := compaction.Compact(context.Background(), history, compaction.Options{Window: 100_000})

	fmt.Println(res.Action, len(res.Messages))
	// Output:
	// none 2
}

// EstimateText predicts the token count of a text without calling a provider.
func ExampleEstimateText() {
	n := compaction.EstimateText("The quick brown fox jumps over the lazy dog.")
	fmt.Println(n > 5 && n < 20)
	// Output:
	// true
}
