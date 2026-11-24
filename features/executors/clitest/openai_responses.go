package clitest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
)

// NewResponses starts a fake OpenAI Responses API (what Codex speaks): POST /v1/responses streamed as SSE
// (response.created, output_item.added/done, response.output_text.delta, response.function_call_arguments.delta,
// response.completed with usage) and GET /v1/models. Requests without tools are answered with a short text.
func NewResponses(script Script) *Server {
	s := &Server{Log: &Log{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.Log.seen(r.Method + " " + r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/responses") && r.Method == http.MethodGet:
			// Codex first tries a WebSocket (GET with Upgrade); this server speaks HTTP only. 426 makes it fall back at once.
			writeJSON(w, http.StatusUpgradeRequired, map[string]any{"error": map[string]any{"message": "websockets are not supported here"}})
		case strings.HasSuffix(r.URL.Path, "/responses") && r.Method == http.MethodPost:
			s.responses(w, r, body, script)
		case strings.HasSuffix(r.URL.Path, "/models"):
			writeJSON(w, 200, map[string]any{"object": "list", "data": []any{map[string]any{"id": "gpt-test", "object": "model"}}, "models": []any{}})
		default:
			writeJSON(w, 404, map[string]any{"error": map[string]any{"message": "no route " + r.URL.Path}})
		}
	})
	s.Server = httptest.NewServer(mux)
	return s
}

func parseResponses(path string, body []byte) (*Request, bool) {
	var in struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
		Tools  []struct {
			Type       string          `json:"type"`
			Name       string          `json:"name"`
			Parameters json.RawMessage `json:"parameters"`
		} `json:"tools"`
		Input json.RawMessage `json:"input"`
	}
	_ = json.Unmarshal(body, &in)
	rq := &Request{Path: path, Model: in.Model, Raw: body, ToolSchemas: map[string]json.RawMessage{}}
	for _, t := range in.Tools {
		name := t.Name
		if name == "" {
			name = t.Type // local_shell, web_search...
		}
		rq.Tools = append(rq.Tools, name)
		rq.ToolSchemas[name] = t.Parameters
	}
	var items []struct {
		Tools []struct {
			Type  string `json:"type"`
			Name  string `json:"name"`
			Tools []struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"tools"`
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		Output  json.RawMessage `json:"output"`
	}
	if json.Unmarshal(in.Input, &items) == nil {
		for _, it := range items {
			switch {
			case it.Type == "additional_tools":
				// Codex's default model gets its tools here, not in "tools": a namespace holding one freeform
				// "exec" tool (JavaScript) whose runtime exposes the others as tools.<name>(...).
				for _, ns := range it.Tools {
					if ns.Type != "namespace" {
						rq.Tools = append(rq.Tools, ns.Name)
					}
					for _, t := range ns.Tools {
						rq.Tools = append(rq.Tools, t.Name)
					}
				}
			case strings.HasSuffix(it.Type, "_output"): // function_call_output, custom_tool_call_output, local_shell_call_output
				rq.ToolResults = append(rq.ToolResults, textOfContent(it.Output))
			case it.Type == "message" && it.Role == "user":
				rq.UserTexts = append(rq.UserTexts, textOfContent(it.Content))
			}
		}
	}
	return rq, in.Stream
}

func (s *Server) responses(w http.ResponseWriter, r *http.Request, body []byte, script Script) {
	rq, stream := parseResponses(r.URL.Path, body)
	if d := os.Getenv("CLITEST_DUMP"); d != "" { // debugging aid: the request bodies the CLI really sends
		_ = os.WriteFile(fmt.Sprintf("%s/responses-%03d.json", d, len(s.Log.Requests())), body, 0o644)
	}
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
	in, out := or(step.In, 1800), or(step.Out, 90)
	usage := map[string]any{"input_tokens": in, "input_tokens_details": map[string]any{"cached_tokens": step.CacheRead}, "output_tokens": out,
		"output_tokens_details": map[string]any{"reasoning_tokens": 0}, "total_tokens": in + out}
	respID := fmt.Sprintf("resp_%03d", n)
	var items []map[string]any
	if step.Text != "" {
		items = append(items, map[string]any{"type": "message", "id": fmt.Sprintf("msg_%03d", n), "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": step.Text, "annotations": []any{}}}})
	}
	for i, t := range step.Tools {
		if t.Custom {
			items = append(items, map[string]any{"type": "custom_tool_call", "id": fmt.Sprintf("ctc_%03d_%d", n, i), "call_id": fmt.Sprintf("call_%03d_%d", n, i),
				"name": t.Name, "input": t.Input, "status": "completed"})
			continue
		}
		args, _ := json.Marshal(t.Args)
		items = append(items, map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%03d_%d", n, i), "call_id": fmt.Sprintf("call_%03d_%d", n, i),
			"name": t.Name, "arguments": string(args), "status": "completed"})
	}
	if !stream {
		writeJSON(w, 200, map[string]any{"id": respID, "object": "response", "status": "completed", "model": rq.Model, "output": items, "usage": usage})
		return
	}
	w.Header().Set("content-type", "text/event-stream")
	w.WriteHeader(200)
	fl := w.(http.Flusher)
	seq := 0
	ev := func(typ string, v map[string]any) {
		v["type"] = typ
		v["sequence_number"] = seq
		seq++
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, b)
		fl.Flush()
	}
	base := func(status string, output []map[string]any) map[string]any {
		return map[string]any{"id": respID, "object": "response", "created_at": 1, "status": status, "model": rq.Model, "output": output, "usage": nil}
	}
	ev("response.created", map[string]any{"response": base("in_progress", []map[string]any{})})
	ev("response.in_progress", map[string]any{"response": base("in_progress", []map[string]any{})})
	for i, it := range items {
		switch it["type"] {
		case "message":
			empty := map[string]any{"type": "message", "id": it["id"], "role": "assistant", "status": "in_progress", "content": []any{}}
			ev("response.output_item.added", map[string]any{"output_index": i, "item": empty})
			ev("response.content_part.added", map[string]any{"item_id": it["id"], "output_index": i, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
			text := step.Text
			for _, piece := range chunks(text, 24) {
				ev("response.output_text.delta", map[string]any{"item_id": it["id"], "output_index": i, "content_index": 0, "delta": piece})
			}
			ev("response.output_text.done", map[string]any{"item_id": it["id"], "output_index": i, "content_index": 0, "text": text})
			ev("response.content_part.done", map[string]any{"item_id": it["id"], "output_index": i, "content_index": 0, "part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}}})
			ev("response.output_item.done", map[string]any{"output_index": i, "item": it})
		case "custom_tool_call":
			added := map[string]any{"type": "custom_tool_call", "id": it["id"], "call_id": it["call_id"], "name": it["name"], "input": "", "status": "in_progress"}
			ev("response.output_item.added", map[string]any{"output_index": i, "item": added})
			input := it["input"].(string)
			for _, piece := range chunks(input, 17) {
				ev("response.custom_tool_call_input.delta", map[string]any{"item_id": it["id"], "output_index": i, "delta": piece})
			}
			ev("response.custom_tool_call_input.done", map[string]any{"item_id": it["id"], "output_index": i, "input": input})
			ev("response.output_item.done", map[string]any{"output_index": i, "item": it})
		case "function_call":
			added := map[string]any{"type": "function_call", "id": it["id"], "call_id": it["call_id"], "name": it["name"], "arguments": "", "status": "in_progress"}
			ev("response.output_item.added", map[string]any{"output_index": i, "item": added})
			args := it["arguments"].(string)
			for _, piece := range chunks(args, 17) {
				ev("response.function_call_arguments.delta", map[string]any{"item_id": it["id"], "output_index": i, "delta": piece})
			}
			ev("response.function_call_arguments.done", map[string]any{"item_id": it["id"], "output_index": i, "arguments": args})
			ev("response.output_item.done", map[string]any{"output_index": i, "item": it})
		}
	}
	done := base("completed", items)
	done["usage"] = usage
	ev("response.completed", map[string]any{"response": done})
}
