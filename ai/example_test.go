package ai_test

import (
	"context"
	"fmt"
	"io"

	"github.com/GhaziBenDahmane/harness/ai"
)

type weatherArgs struct {
	City string `json:"city" jsonschema_description:"the city to look up"`
}

// A tool is a Go function: InferTool derives the arguments schema the model sees from the input struct, decodes the
// model's JSON arguments into it and renders the result as the text the model reads.
func ExampleInferTool() {
	weather, _ := ai.InferTool("weather", "Current weather of a city.",
		func(_ context.Context, in weatherArgs) (map[string]any, error) {
			return map[string]any{"city": in.City, "temp_c": 18}, nil
		})

	info, _ := weather.Info(context.Background())
	out, _ := weather.InvokableRun(context.Background(), `{"city":"Lyon"}`)

	fmt.Println(info.Name, "-", info.Desc)
	fmt.Println(info.Parameters["required"])
	fmt.Println(out)
	// Output:
	// weather - Current weather of a city.
	// [city]
	// {"city":"Lyon","temp_c":18}
}

// A streamed answer arrives as chunks; ConcatMessages merges them into the message a non-streamed call returns. Tool
// call fragments that share an Index are joined, their arguments concatenated.
func ExampleConcatMessages() {
	zero := 0
	msg, _ := ai.ConcatMessages([]*ai.Message{
		{Role: ai.RoleAssistant, Content: "Let me "},
		{Content: "look."},
		{ToolCalls: []ai.ToolCall{{Index: &zero, ID: "call_1", Type: "function", Function: ai.FunctionCall{Name: "weather", Arguments: `{"city":`}}}},
		{ToolCalls: []ai.ToolCall{{Index: &zero, Function: ai.FunctionCall{Arguments: `"Lyon"}`}}}},
	})

	fmt.Println(msg.Content)
	fmt.Println(msg.ToolCalls[0].Function.Name, msg.ToolCalls[0].Function.Arguments)
	// Output:
	// Let me look.
	// weather {"city":"Lyon"}
}

// Pipe connects a producer to a consumer. The consumer reads until io.EOF; closing the reader tells the producer to stop.
func ExamplePipe() {
	sr, sw := ai.Pipe[string](2)
	go func() {
		defer sw.Close()
		sw.Send("hello", nil)
		sw.Send("world", nil)
	}()
	defer sr.Close()

	for {
		chunk, err := sr.Recv()
		if err == io.EOF {
			break
		}
		fmt.Println(chunk)
	}
	// Output:
	// hello
	// world
}
