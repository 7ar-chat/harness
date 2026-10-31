package agent_test

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/ai/agent"
	"github.com/GhaziBenDahmane/harness/ai/llm"
	"github.com/GhaziBenDahmane/harness/ai/llm/llmtest"
)

type cityArgs struct {
	City string `json:"city"`
}

// Run asks the model, runs the tools it calls, feeds the results back, and stops when the model answers in plain text.
// The model here is scripted; with a real one, pass the result of openai.New(...).WithTools(...).
func ExampleRun() {
	weather, _ := ai.InferTool("weather", "Current weather of a city.",
		func(_ context.Context, in cityArgs) (string, error) { return in.City + ": 18C, clear", nil })
	info, _ := weather.Info(context.Background())

	script := llmtest.New(
		llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, "call_1", "weather", `{"city":"Lyon"}`), "tool_calls")}},
		llmtest.Turn{Chunks: llmtest.Text("It is 18C and clear in Lyon.")},
	)
	model, _ := script.WithTools([]*ai.ToolInfo{info})

	res, err := agent.Run(context.Background(), agent.Config{
		Model: model,
		Tools: []ai.InvokableTool{weather},
		OnTool: func(c ai.ToolCall, result string) {
			fmt.Printf("%s(%s) -> %s\n", c.Function.Name, c.Function.Arguments, result)
		},
	}, []*ai.Message{ai.UserMessage("Weather in Lyon?")})

	fmt.Println(res.Reply.Content, "| steps:", res.Steps, "| err:", err)
	// Output:
	// weather({"city":"Lyon"}) -> Lyon: 18C, clear
	// It is 18C and clear in Lyon. | steps: 2 | err: <nil>
}

// A Caller streams each model call under a watchdog and retries the ones worth retrying. Give it to Run as Config.Caller,
// or use it directly in your own loop. Here the first attempt fails with a transient error and the second succeeds.
func ExampleCaller() {
	script := llmtest.New(
		llmtest.Turn{Err: fmt.Errorf("status code: 503, overloaded")},
		llmtest.Turn{Chunks: llmtest.Text("hello")},
	)
	caller := &agent.Caller{
		Retries: 2,
		Sleep:   func(context.Context, time.Duration) error { return nil }, // no real waiting in an example
		OnRetry: func(_ context.Context, attempt int, class llm.ErrorClass, _ error) error {
			fmt.Printf("attempt %d failed (%s), retrying\n", attempt, class)
			return nil
		},
	}

	stream, err := caller.Call(context.Background(), script, []*ai.Message{ai.UserMessage("hi")}, nil,
		func() agent.Attempt { return observer{} })
	reply, _ := ai.ConcatMessages(stream.Chunks)

	fmt.Println(reply.Content, "| err:", err)
	// Output:
	// attempt 1 failed (transient), retrying
	// hello | err: <nil>
}

// observer is an Attempt that has nothing to do per chunk. A real one turns chunks into events and undoes them in Abort.
type observer struct{}

func (observer) Chunk(context.Context, *ai.Message) error { return nil }
func (observer) Settle(context.Context, bool) error       { return nil }
func (observer) Abort(context.Context, bool) error        { return nil }

// Schedule runs a step's tool calls in the model's order and hands consecutive parallel-safe calls to a batch.
func ExampleSchedule() {
	calls := []ai.ToolCall{{ID: "read_a"}, {ID: "read_b"}, {ID: "write_c"}, {ID: "read_d"}}
	readOnly := func(_ context.Context, c ai.ToolCall) bool { return c.ID[:4] == "read" }

	_ = agent.Schedule(context.Background(), calls, readOnly,
		func(_ context.Context, c ai.ToolCall) error { fmt.Println("alone:", c.ID); return nil },
		func(_ context.Context, b []ai.ToolCall) error {
			ids := make([]string, len(b))
			for i, c := range b {
				ids[i] = c.ID
			}
			fmt.Println("together:", ids)
			return nil
		})
	// Output:
	// together: [read_a read_b]
	// alone: write_c
	// alone: read_d
}

// CapOutput bounds a tool result before the model reads it, keeping the head and the tail and saying how much was cut.
func ExampleCapOutput() {
	long := strings.Repeat("a", 500) + strings.Repeat("z", 500)
	out := agent.CapOutput(long, 200)

	fmt.Println(len([]rune(out)), strings.HasPrefix(out, "aaa"), strings.HasSuffix(out, "zzz"), strings.Contains(out, "800 characters truncated"))
	fmt.Println(agent.CapOutput("short", 200))
	// Output:
	// 200 true true true
	// short
}
