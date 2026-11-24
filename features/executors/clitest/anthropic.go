package clitest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
)

// NewAnthropic starts a fake Anthropic Messages API: POST /v1/messages (streaming SSE or one JSON message),
// /v1/messages/count_tokens and /v1/models. Requests without tools (titles, probes) get a short text; the agent
// loop's requests are answered by script.
func NewAnthropic(script Script) *Server {
	s := &Server{Log: &Log{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.Log.seen(r.Method + " " + r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.URL.Path == "/v1/messages/count_tokens":
			writeJSON(w, 200, map[string]any{"input_tokens": 42})
		case r.URL.Path == "/v1/messages" && r.Method == http.MethodPost:
			s.messages(w, body, script)
		case strings.HasPrefix(r.URL.Path, "/v1/models"):
			writeJSON(w, 200, map[string]any{"data": []any{}, "has_more": false, "first_id": nil, "last_id": nil})
		default:
			writeJSON(w, 404, map[string]any{"type": "error", "error": map[string]any{"type": "not_found_error", "message": "no route " + r.URL.Path}})
		}
	})
	s.Server = httptest.NewServer(mux)
	return s
}

func parseAnthropic(path string, body []byte) (*Request, bool) {
	var in struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
		Tools  []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &in)
	rq := &Request{Path: path, Model: in.Model, Raw: body, ToolSchemas: map[string]json.RawMessage{}}
	for _, t := range in.Tools {
		rq.Tools = append(rq.Tools, t.Name)
		rq.ToolSchemas[t.Name] = t.InputSchema
	}
	for _, m := range in.Messages {
		var blocks []struct {
			Type    string          `json:"type"`
			Text    string          `json:"text"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(m.Content, &blocks) != nil {
			if m.Role == "user" {
				rq.UserTexts = append(rq.UserTexts, textOfContent(m.Content))
			}
			continue
		}
		for _, b := range blocks {
			switch {
			case b.Type == "tool_result":
				rq.ToolResults = append(rq.ToolResults, textOfContent(b.Content))
			case b.Type == "text" && m.Role == "user":
				rq.UserTexts = append(rq.UserTexts, b.Text)
			}
		}
	}
	return rq, in.Stream
}

func (s *Server) messages(w http.ResponseWriter, body []byte, script Script) {
	rq, stream := parseAnthropic("/v1/messages", body)
	s.Log.add(rq)
	step := Step{Text: "ok"}
	if len(rq.Tools) > 0 {
		step = script(rq)
	}
	if step.Status != 0 {
		writeJSON(w, step.Status, map[string]any{"type": "error", "error": map[string]any{"type": "authentication_error", "message": "invalid x-api-key"}})
		return
	}
	in, out := or(step.In, 1500), or(step.Out, 120)
	usage := map[string]any{"input_tokens": in, "output_tokens": out, "cache_read_input_tokens": step.CacheRead, "cache_creation_input_tokens": 0}
	stop := "end_turn"
	var content []map[string]any
	if step.Text != "" {
		content = append(content, map[string]any{"type": "text", "text": step.Text})
	}
	for i, t := range step.Tools {
		content = append(content, map[string]any{"type": "tool_use", "id": fmt.Sprintf("toolu_%03d_%d", len(s.Log.Requests()), i), "name": t.Name, "input": t.Args})
		stop = "tool_use"
	}
	msg := map[string]any{"id": fmt.Sprintf("msg_%03d", len(s.Log.Requests())), "type": "message", "role": "assistant", "model": rq.Model,
		"content": content, "stop_reason": stop, "stop_sequence": nil, "usage": usage}
	if !stream {
		writeJSON(w, 200, msg)
		return
	}
	w.Header().Set("content-type", "text/event-stream")
	w.WriteHeader(200)
	fl := w.(http.Flusher)
	ev := func(name string, v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
		fl.Flush()
	}
	start := map[string]any{"id": msg["id"], "type": "message", "role": "assistant", "model": rq.Model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": in, "output_tokens": 1, "cache_read_input_tokens": step.CacheRead, "cache_creation_input_tokens": 0}}
	ev("message_start", map[string]any{"type": "message_start", "message": start})
	ev("ping", map[string]any{"type": "ping"})
	for i, c := range content {
		switch c["type"] {
		case "text":
			ev("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "text", "text": ""}})
			text := c["text"].(string)
			for _, piece := range chunks(text, 24) {
				ev("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "text_delta", "text": piece}})
			}
		case "tool_use":
			ev("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "tool_use", "id": c["id"], "name": c["name"], "input": map[string]any{}}})
			args, _ := json.Marshal(c["input"])
			for _, piece := range chunks(string(args), 17) {
				ev("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": piece}})
			}
		}
		ev("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
	}
	ev("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": map[string]any{"input_tokens": in, "output_tokens": out, "cache_read_input_tokens": step.CacheRead, "cache_creation_input_tokens": 0}})
	ev("message_stop", map[string]any{"type": "message_stop"})
}

func chunks(s string, n int) []string {
	r := []rune(s)
	var out []string
	for len(r) > n {
		out = append(out, string(r[:n]))
		r = r[n:]
	}
	return append(out, string(r))
}
