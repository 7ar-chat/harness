package llmtest_test

import (
	"context"
	"fmt"

	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/ai/llm/llmtest"
)

// A scripted model answers each call with the next Turn and records the calls, so a test can check what the code under
// test asked for as well as what it did with the answer.
func ExampleModel() {
	model := llmtest.New(
		llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, "call_1", "weather", `{"city":"Lyon"}`), "tool_calls")}},
		llmtest.Turn{Chunks: llmtest.Text("18C and clear.")},
	)

	first, _ := model.Generate(context.Background(), []*ai.Message{ai.UserMessage("Weather in Lyon?")})
	second, _ := model.Generate(context.Background(), []*ai.Message{ai.UserMessage("Weather in Lyon?"), first,
		ai.ToolMessage(`{"temp_c":18}`, "call_1")}, ai.WithReasoningEffort("low"))

	fmt.Println(first.ToolCalls[0].Function.Name, "->", second.Content)
	calls := model.Calls()
	fmt.Println(len(calls), "calls; second had", len(calls[1].Messages), "messages and effort", calls[1].ReasoningEffort)
	// Output:
	// weather -> 18C and clear.
	// 2 calls; second had 3 messages and effort low
}

// A Turn can fail instead of answering, to test how code reacts to a provider error.
func ExampleTurn_failure() {
	model := llmtest.New(llmtest.Turn{Err: fmt.Errorf("status code: 503, overloaded")})

	_, err := model.Generate(context.Background(), []*ai.Message{ai.UserMessage("hi")})

	fmt.Println(err)
	// Output:
	// status code: 503, overloaded
}
