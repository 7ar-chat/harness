package split_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeLLM is a scripted, OpenAI-compatible chat-completions server (the Go twin of web/e2e/fakellm.mjs).
//
// What it answers is decided from the request alone:
//   - no tools offered (title generation)         -> "A title"
//   - the last message is a tool result           -> "Wrote out.md"
//   - the last user text contains "write"         -> a write_file{out.md, "hello"} tool call
//   - anything else                               -> "Reply to: <user text>" streamed in three pieces
//
// A request that opens a model turn (tools offered, last message from the user) first passes the gates the test
// installed with Hold and Barrier; a gate waits for its release or for the request to be abandoned.
type fakeLLM struct {
	srv  *httptest.Server
	done chan struct{}

	mu    sync.Mutex
	gates []func(ctx context.Context, user string)
}

func newFakeLLM(t *testing.T) *fakeLLM {
	t.Helper()
	f := &fakeLLM{done: make(chan struct{})}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(func() { close(f.done); f.srv.Close() })
	return f
}

// BaseURL is the LLM_BASE_URL of the processes that call the fake.
func (f *fakeLLM) BaseURL() string { return f.srv.URL + "/v1" }

// hold is a gate on the model turns whose user text contains a substring.
type hold struct {
	arrived chan struct{} // closed when the first matching request is held
	release chan struct{}
	once    sync.Once
}

// Hold keeps every model turn whose user text contains match until the returned hold is released.
func (f *fakeLLM) Hold(match string) *hold {
	h := &hold{arrived: make(chan struct{}), release: make(chan struct{})}
	f.addGate(func(ctx context.Context, user string) {
		if !strings.Contains(user, match) {
			return
		}
		h.once.Do(func() { close(h.arrived) })
		select {
		case <-h.release:
		case <-ctx.Done():
		case <-f.done:
		}
	})
	return h
}

// Release lets the held replies continue.
func (h *hold) Release() { close(h.release) }

// Barrier holds every model turn until n of them are waiting at the same time, then lets them all go.
func (f *fakeLLM) Barrier(n int) {
	var mu sync.Mutex
	waiting := 0
	ready := make(chan struct{})
	f.addGate(func(ctx context.Context, _ string) {
		mu.Lock()
		waiting++
		if waiting == n {
			close(ready)
		}
		mu.Unlock()
		select {
		case <-ready:
		case <-ctx.Done():
		case <-f.done:
		}
	})
}

func (f *fakeLLM) addGate(g func(ctx context.Context, user string)) {
	f.mu.Lock()
	f.gates = append(f.gates, g)
	f.mu.Unlock()
}

type chatRequest struct {
	Stream   bool              `json:"stream"`
	Tools    []json.RawMessage `json:"tools"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

// contentText flattens a message content that is a string or a list of parts.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &parts)
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// scripted is the reply to one request: text pieces or one tool call.
type scripted struct {
	text []string
	call *struct{ name, args string }
}

func (f *fakeLLM) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	var reply scripted
	last := ""
	user := ""
	for _, m := range req.Messages {
		last = m.Role
		if m.Role == "user" {
			user = contentText(m.Content)
		}
	}
	switch {
	case len(req.Tools) == 0:
		reply.text = []string{"A title"}
	case last == "tool":
		reply.text = []string{"Wrote ", "out.md"}
	default:
		f.mu.Lock()
		gates := append([]func(context.Context, string){}, f.gates...)
		f.mu.Unlock()
		for _, g := range gates {
			g(r.Context(), user)
		}
		if r.Context().Err() != nil {
			return
		}
		if strings.Contains(user, "write") {
			reply.call = &struct{ name, args string }{"write_file", `{"path":"out.md","content":"hello"}`}
		} else {
			reply.text = []string{"Reply ", "to: ", user}
		}
	}
	if !req.Stream {
		writeCompletion(w, reply)
		return
	}
	writeStream(w, reply)
}

func chunk(delta map[string]any, finish any) map[string]any {
	return map[string]any{
		"id": "c1", "object": "chat.completion.chunk", "created": 1, "model": "gpt-test",
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
}

func writeStream(w http.ResponseWriter, reply scripted) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fl := w.(http.Flusher)
	send := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		fl.Flush()
	}
	send(chunk(map[string]any{"role": "assistant", "content": ""}, nil))
	if reply.call != nil {
		send(chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": 0, "id": "call_1", "type": "function",
			"function": map[string]any{"name": reply.call.name, "arguments": reply.call.args},
		}}}, nil))
		send(chunk(map[string]any{}, "tool_calls"))
	} else {
		for _, piece := range reply.text {
			send(chunk(map[string]any{"content": piece}, nil))
		}
		send(chunk(map[string]any{}, "stop"))
	}
	send(map[string]any{
		"id": "c1", "object": "chat.completion.chunk", "created": 1, "model": "gpt-test", "choices": []any{},
		"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	})
	fmt.Fprint(w, "data: [DONE]\n\n")
	fl.Flush()
}

func writeCompletion(w http.ResponseWriter, reply scripted) {
	msg := map[string]any{"role": "assistant", "content": strings.Join(reply.text, "")}
	finish := "stop"
	if reply.call != nil {
		msg = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
			"id": "call_1", "type": "function",
			"function": map[string]any{"name": reply.call.name, "arguments": reply.call.args},
		}}}
		finish = "tool_calls"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "c1", "object": "chat.completion", "created": 1, "model": "gpt-test",
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
		"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	})
}
