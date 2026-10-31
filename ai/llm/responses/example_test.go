package responses_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/ai/llm/responses"
)

// New returns a model over the OpenAI Responses API. Calls are stateless (store:false) and ask for the encrypted
// reasoning, which travels on the answer message (ItemsOf) and is sent back with the next request so the model keeps
// its reasoning across tool steps. The server here is a stand-in.
func ExampleNew() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"msg_1\",\"role\":\"assistant\",\"content\":[]}}\n\n"+
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"Hello!\"}\n\n"+
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":2,\"total_tokens\":12}}}\n\n")
	}))
	defer srv.Close()

	model, err := responses.New(responses.Config{URL: srv.URL + "/v1/responses", Model: "gpt-5"})
	if err != nil {
		panic(err)
	}
	reply, err := model.Generate(context.Background(), []*ai.Message{ai.UserMessage("Hi")})

	fmt.Println(reply.Content, "|", reply.ResponseMeta.Usage.TotalTokens, "tokens |", len(responses.ItemsOf(reply)), "reasoning items |", err)
	// Output:
	// Hello! | 12 tokens | 0 reasoning items | <nil>
}
