package anthropic_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/anthropic"
)

// New returns a model over the Anthropic Messages API. It sets prompt-cache breakpoints on the tools, the system
// prompt and the last message, so a long agent history is billed at the cache rate. Finish reasons are mapped to the
// ones the other clients use (end_turn is "stop", tool_use is "tool_calls", max_tokens is "length"). The server here is
// a stand-in.
func ExampleNew() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":12,\"output_tokens\":1}}}\n\n"+
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello!\"}}\n\n"+
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":4}}\n\n"+
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	model, err := anthropic.New(anthropic.Config{BaseURL: srv.URL, APIKey: "sk-ant-test", Model: "claude-sonnet"})
	if err != nil {
		panic(err)
	}
	reply, err := model.Generate(context.Background(), []*ai.Message{ai.SystemMessage("Be brief."), ai.UserMessage("Hi")})

	fmt.Println(reply.Content, "|", reply.ResponseMeta.FinishReason, "|", err)
	// Output:
	// Hello! | stop | <nil>
}
