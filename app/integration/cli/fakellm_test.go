//go:build unix

package cli_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// request is one recorded POST /v1/chat/completions.
type request struct {
	Stream   bool             `json:"stream"`
	Model    string           `json:"model"`
	Messages []map[string]any `json:"messages"`
	Tools    []map[string]any `json:"tools"`
}

// toolNames lists the function names offered in the request.
func (r request) toolNames() []string {
	var names []string
	for _, t := range r.Tools {
		if fn, ok := t["function"].(map[string]any); ok {
			names = append(names, fmt.Sprint(fn["name"]))
		}
	}
	return names
}

// text is the content of every message of the given role, joined.
func (r request) text(role string) string {
	var b strings.Builder
	for _, m := range r.Messages {
		if m["role"] != role {
			continue
		}
		switch c := m["content"].(type) {
		case string:
			b.WriteString(c)
		case []any:
			for _, p := range c {
				if pm, ok := p.(map[string]any); ok {
					b.WriteString(fmt.Sprint(pm["text"]))
				}
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// toolMessage returns the first role=tool message, or nil.
func (r request) toolMessage() map[string]any {
	for _, m := range r.Messages {
		if m["role"] == "tool" {
			return m
		}
	}
	return nil
}

// reply is what the fake answers: a text, a tool call, or an HTTP error.
type reply struct {
	text     string
	toolName string
	toolArgs string
	status   int // non-zero: answer this HTTP status with an error body
}

// fakeLLM is a scripted OpenAI-compatible server on loopback.
type fakeLLM struct {
	srv    *httptest.Server
	script func(request) reply

	mu   sync.Mutex
	reqs []request
}

func newFakeLLM(t *testing.T, script func(request) reply) *fakeLLM {
	t.Helper()
	f := &fakeLLM{script: script}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"gpt-test","object":"model"}]}`)
	})
	mux.HandleFunc("POST /v1/chat/completions", f.chat)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLLM) baseURL() string { return f.srv.URL + "/v1" }

// requests returns the recorded requests that offered tools (the agent's own
// calls), in arrival order; the title call offers none.
func (f *fakeLLM) agentRequests() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []request
	for _, r := range f.reqs {
		if len(r.Tools) > 0 {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeLLM) chat(w http.ResponseWriter, r *http.Request) {
	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()

	rep := reply{text: "A title"}
	if len(req.Tools) > 0 {
		rep = f.script(req)
	}
	if rep.status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rep.status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": fmt.Sprintf("scripted failure %d", rep.status), "type": "scripted_error"}})
		return
	}
	finish := "stop"
	if rep.toolName != "" {
		finish = "tool_calls"
	}
	if !req.Stream {
		msg := map[string]any{"role": "assistant", "content": rep.text}
		if rep.toolName != "" {
			msg = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
				"id": "call_1", "type": "function", "function": map[string]any{"name": rep.toolName, "arguments": rep.toolArgs}}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "object": "chat.completion", "created": 1, "model": "gpt-test",
			"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	fl, _ := w.(http.Flusher)
	send := func(delta map[string]any, finishReason any) {
		b, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-1", "object": "chat.completion.chunk", "created": 1, "model": "gpt-test",
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finishReason}},
		})
		fmt.Fprintf(w, "data: %s\n\n", b)
		if fl != nil {
			fl.Flush()
		}
	}
	send(map[string]any{"role": "assistant", "content": ""}, nil)
	if rep.toolName != "" {
		send(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_1", "type": "function",
			"function": map[string]any{"name": rep.toolName, "arguments": ""}}}}, nil)
		mid := len(rep.toolArgs) / 2
		for _, part := range []string{rep.toolArgs[:mid], rep.toolArgs[mid:]} {
			send(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": part}}}}, nil)
		}
	} else {
		mid := len(rep.text) / 2
		for _, part := range []string{rep.text[:mid], rep.text[mid:]} {
			send(map[string]any{"content": part}, nil)
		}
	}
	send(map[string]any{}, finish)
	b, _ := json.Marshal(map[string]any{"id": "chatcmpl-1", "object": "chat.completion.chunk", "created": 1, "model": "gpt-test",
		"choices": []any{}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}})
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
	if fl != nil {
		fl.Flush()
	}
}

// answer replies text to every agent call.
func answer(text string) func(request) reply {
	return func(request) reply { return reply{text: text} }
}

// toolThen asks for one tool call, then answers final once the history holds a tool result.
func toolThen(name, args, final string) func(request) reply {
	return func(r request) reply {
		if r.toolMessage() != nil {
			return reply{text: final}
		}
		return reply{toolName: name, toolArgs: args}
	}
}
