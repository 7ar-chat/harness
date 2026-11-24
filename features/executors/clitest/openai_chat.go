package clitest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
)

// NewOpenAIChat starts a fake OpenAI chat-completions API (what omp speaks for a custom openai-completions
// provider): POST /v1/chat/completions streamed as chat.completion.chunk SSE frames ending with a usage chunk and
// [DONE], and GET /v1/models. Requests without tools get a short text.
func NewOpenAIChat(script Script) *Server {
	s := &Server{Log: &Log{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.Log.seen(r.Method + " " + r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat/completions") && r.Method == http.MethodPost:
			s.chat(w, body, script)
		case strings.HasSuffix(r.URL.Path, "/models"):
			writeJSON(w, 200, map[string]any{"object": "list", "data": []any{map[string]any{"id": "fake-model", "object": "model"}}})
		default:
			writeJSON(w, 404, map[string]any{"error": map[string]any{"message": "no route " + r.URL.Path}})
		}
	})
	s.Server = httptest.NewServer(mux)
	return s
}

func parseChat(path string, body []byte) (*Request, bool) {
	var in struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
		Tools  []struct {
			Function struct {
				Name       string          `json:"name"`
				Parameters json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &in)
	rq := &Request{Path: path, Model: in.Model, Raw: body, ToolSchemas: map[string]json.RawMessage{}}
	for _, t := range in.Tools {
		rq.Tools = append(rq.Tools, t.Function.Name)
		rq.ToolSchemas[t.Function.Name] = t.Function.Parameters
	}
	for _, m := range in.Messages {
		switch m.Role {
		case "tool":
			rq.ToolResults = append(rq.ToolResults, textOfContent(m.Content))
		case "user":
			rq.UserTexts = append(rq.UserTexts, textOfContent(m.Content))
		}
	}
	return rq, in.Stream
}

func (s *Server) chat(w http.ResponseWriter, body []byte, script Script) {
	rq, stream := parseChat("/v1/chat/completions", body)
	s.Log.add(rq)
	step := Step{Text: "ok"}
	if len(rq.Tools) > 0 {
		step = script(rq)
	}
	if step.Status != 0 {
		writeJSON(w, step.Status, map[string]any{"error": map[string]any{"message": "Incorrect API key provided: test. You can find your API key at https://platform.openai.com/account/api-keys.",
			"type": "invalid_request_error", "param": nil, "code": "invalid_api_key"}})
		return
	}
	n := len(s.Log.Requests())
	in, out := or(step.In, 1700), or(step.Out, 70)
	usage := map[string]any{"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out,
		"prompt_tokens_details": map[string]any{"cached_tokens": step.CacheRead}}
	finish := "stop"
	var calls []map[string]any
	for i, t := range step.Tools {
		args, _ := json.Marshal(t.Args)
		calls = append(calls, map[string]any{"id": fmt.Sprintf("call_%03d_%d", n, i), "type": "function",
			"function": map[string]any{"name": t.Name, "arguments": string(args)}})
		finish = "tool_calls"
	}
	id := fmt.Sprintf("chatcmpl-%03d", n)
	if !stream {
		msg := map[string]any{"role": "assistant", "content": step.Text}
		if calls != nil {
			msg["tool_calls"] = calls
		}
		writeJSON(w, 200, map[string]any{"id": id, "object": "chat.completion", "created": 1, "model": rq.Model, "usage": usage,
			"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}}})
		return
	}
	w.Header().Set("content-type", "text/event-stream")
	w.WriteHeader(200)
	fl := w.(http.Flusher)
	frame := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		fl.Flush()
	}
	chunk := func(delta map[string]any, fin any) map[string]any {
		return map[string]any{"id": id, "object": "chat.completion.chunk", "created": 1, "model": rq.Model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": fin}}}
	}
	frame(chunk(map[string]any{"role": "assistant", "content": ""}, nil))
	if step.Text != "" {
		for _, piece := range chunks(step.Text, 24) {
			frame(chunk(map[string]any{"content": piece}, nil))
		}
	}
	for i, c := range calls {
		fn := c["function"].(map[string]any)
		frame(chunk(map[string]any{"tool_calls": []any{map[string]any{"index": i, "id": c["id"], "type": "function",
			"function": map[string]any{"name": fn["name"], "arguments": ""}}}}, nil))
		for _, piece := range chunks(fn["arguments"].(string), 17) {
			frame(chunk(map[string]any{"tool_calls": []any{map[string]any{"index": i, "function": map[string]any{"arguments": piece}}}}, nil))
		}
	}
	frame(chunk(map[string]any{}, finish))
	frame(map[string]any{"id": id, "object": "chat.completion.chunk", "created": 1, "model": rq.Model, "choices": []any{}, "usage": usage})
	fmt.Fprint(w, "data: [DONE]\n\n")
	fl.Flush()
}
