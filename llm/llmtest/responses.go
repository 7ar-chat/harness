package llmtest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	goopenai "github.com/meguminnnnnnnnn/go-openai"

	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/llm"
	"github.com/GhaziBenDahmane/harness/llm/responses"
)

// The Responses backend plays the scripted turns through a fake Responses API
// server (the event shapes of docs/spike-responses-api.md) and the real
// Responses client (llm.Provider with LLM_RESPONSES_MODELS): the calls are
// recorded as usual, every turn starts with an encrypted reasoning item (as
// gpt-5.6 answers do), and the server checks each request like the provider
// would (store:false, every function_call answered by a later
// function_call_output and every output after its call, unknown encrypted
// content refused with invalid_encrypted_content) plus Work's own invariant:
// a reasoning item is sent right before the output it produced.

var responsesBackend atomic.Bool

// UseResponses makes every Factory serve its model through the fake Responses
// server (models with a "provider/" prefix stay on the scripted Chat Completions fake).
func UseResponses(on bool) { responsesBackend.Store(on) }

// UsingResponses reports whether the Responses backend is on.
func UsingResponses() bool { return responsesBackend.Load() }

// Violations are the requests the fake Responses server refused as invalid
// (a history the real provider would reject).
func Violations() []string {
	fake().mu.Lock()
	defer fake().mu.Unlock()
	return append([]string(nil), fake().violations...)
}

// Requests are the Responses request bodies the model's calls sent (Responses backend only).
func (m *Model) Requests() []map[string]any {
	r := m.r()
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]any(nil), r.requests...)
}

// TurnHeader carries the scripted turn of a request to the fake server.
const TurnHeader = "X-Llmtest-Turn"

type turnKey struct{}

type pendingTurn struct {
	turn Turn
	root *Model
	name string
}

type fakeResponses struct {
	once       sync.Once
	srv        *httptest.Server
	transport  http.RoundTripper
	mu         sync.Mutex
	pending    map[string]*pendingTurn
	issued     map[string]string // reasoning item id → encrypted_content
	violations []string
	seq        atomic.Int64
}

var fakeSrv fakeResponses

func fake() *fakeResponses {
	f := &fakeSrv
	f.once.Do(func() {
		f.pending, f.issued = map[string]*pendingTurn{}, map[string]string{}
		f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.MaxIdleConnsPerHost = 64
		f.transport = &turnTransport{base: tr}
	})
	return f
}

func (f *fakeResponses) id(prefix string) string {
	return fmt.Sprintf("%s_%06d", prefix, f.seq.Add(1))
}

// turnTransport copies the turn token of the request context into TurnHeader.
type turnTransport struct{ base http.RoundTripper }

func (t *turnTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if tok, _ := req.Context().Value(turnKey{}).(string); tok != "" {
		req = req.Clone(req.Context())
		req.Header.Set(TurnHeader, tok)
	}
	return t.base.RoundTrip(req)
}

// responsesClient returns the real Responses client of name against the fake server.
func (m *Model) responsesClient(ctx context.Context, name string) (model.ToolCallingChatModel, error) {
	r := m.r()
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.clients[name]; c != nil {
		return c, nil
	}
	f := fake()
	cfg := &config.Config{LLMProvider: "openai", LLMBaseURL: f.srv.URL + "/v1", LLMAPIKey: "test-key",
		LLMModels: []string{name}, Responses: config.Responses{LLMResponsesModels: []string{name}}}
	inner, err := llm.NewWithTransport(cfg, f.transport).Model(ctx, name)
	if err != nil {
		return nil, err
	}
	c := &responsesModel{view: r, name: name, inner: inner}
	if r.clients == nil {
		r.clients = map[string]model.ToolCallingChatModel{}
	}
	r.clients[name] = c
	return c, nil
}

// responsesModel records the call and picks its turn like the scripted model,
// then lets the real Responses client play it through the fake server.
type responsesModel struct {
	view  *Model
	name  string
	inner model.ToolCallingChatModel
}

func (m *responsesModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	inner, err := m.inner.WithTools(tools)
	if err != nil {
		return nil, err
	}
	return &responsesModel{view: &Model{root: m.view.r(), tools: tools}, name: m.name, inner: inner}, nil
}

func (m *responsesModel) prepare(ctx context.Context, msgs []*schema.Message, opts []model.Option) context.Context {
	t := m.view.next(ctx, msgs, opts)
	if t.OnCall != nil {
		t.OnCall()
	}
	f := fake()
	tok := f.id("turn")
	f.mu.Lock()
	f.pending[tok] = &pendingTurn{turn: t, root: m.view.r(), name: m.name}
	f.mu.Unlock()
	return context.WithValue(ctx, turnKey{}, tok)
}

func (m *responsesModel) Stream(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return m.inner.Stream(m.prepare(ctx, msgs, opts), msgs, opts...)
}

func (m *responsesModel) Generate(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	return m.inner.Generate(m.prepare(ctx, msgs, opts), msgs, opts...)
}

// --- the fake server ---

func (f *fakeResponses) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil || r.Context().Err() != nil {
		return // the client went away while sending (a cancelled call)
	}
	f.mu.Lock()
	p := f.pending[r.Header.Get(TurnHeader)]
	f.mu.Unlock()
	if p == nil || !strings.HasSuffix(r.URL.Path, "/responses") {
		writeError(w, http.StatusNotFound, "not_found", "llmtest: unknown turn or path "+r.URL.Path, 0)
		return
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		f.violation(w, p, "the body is not JSON: "+err.Error())
		return
	}
	p.root.mu.Lock()
	p.root.requests = append(p.root.requests, req)
	p.root.mu.Unlock()
	if msg := f.validate(r, p.name, req); msg != "" {
		f.violation(w, p, msg)
		return
	}
	if id := f.unknownReasoning(req); id != "" {
		writeError(w, http.StatusBadRequest, "invalid_encrypted_content",
			"The encrypted content for item "+id+" could not be verified. Reason: Encrypted content could not be decrypted or parsed.", 0)
		return
	}
	f.play(w, r, p)
}

func (f *fakeResponses) violation(w http.ResponseWriter, p *pendingTurn, msg string) {
	f.mu.Lock()
	f.violations = append(f.violations, p.name+": "+msg)
	f.mu.Unlock()
	writeError(w, http.StatusBadRequest, "invalid_request_error", "llmtest: "+msg, 0)
}

func writeError(w http.ResponseWriter, status int, code, msg string, retryAfter time.Duration) {
	if retryAfter > 0 {
		w.Header().Set("retry-after-ms", strconv.FormatInt(retryAfter.Milliseconds(), 10))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": "invalid_request_error", "param": nil, "code": code}})
	_, _ = w.Write(b)
}

// validate checks a request like the provider: "" when valid.
func (f *fakeResponses) validate(r *http.Request, name string, req map[string]any) string {
	if r.URL.Path != "/v1/responses" {
		return "path " + r.URL.String()
	}
	if r.Header.Get("Authorization") != "Bearer test-key" {
		return "missing or wrong Authorization"
	}
	if req["model"] != name {
		return "model must be sent verbatim"
	}
	if req["stream"] != true || req["store"] != false {
		return "stream must be true and store false"
	}
	if inc, _ := req["include"].([]any); len(inc) != 1 || inc[0] != "reasoning.encrypted_content" {
		return "include must be [reasoning.encrypted_content]"
	}
	if tools, _ := req["tools"].([]any); len(tools) > 0 {
		if req["parallel_tool_calls"] != true {
			return "parallel_tool_calls must be true with tools"
		}
		for _, t := range tools {
			tm, _ := t.(map[string]any)
			params, _ := tm["parameters"].(map[string]any)
			if tm["type"] != "function" || tm["name"] == "" || params["type"] != "object" {
				return fmt.Sprintf("invalid tool %v", tm["name"])
			}
			if tm["strict"] == true && !responses.Strict(params) {
				return fmt.Sprintf("Invalid schema for function '%v' in strict mode", tm["name"])
			}
		}
	}
	input, _ := req["input"].([]any)
	calls, outputs := map[string]bool{}, map[string]bool{}
	for i, raw := range input {
		it, _ := raw.(map[string]any)
		switch it["type"] {
		case "message":
			switch it["role"] {
			case "system", "developer", "user", "assistant":
			default:
				return fmt.Sprintf("input[%d]: role %v", i, it["role"])
			}
		case "reasoning":
			if s, _ := it["id"].(string); s == "" {
				return fmt.Sprintf("input[%d]: reasoning item without id", i)
			}
			if s, _ := it["encrypted_content"].(string); s == "" {
				return fmt.Sprintf("input[%d]: reasoning item without encrypted_content (store:false)", i)
			}
			if _, ok := it["summary"].([]any); !ok {
				return fmt.Sprintf("input[%d]: reasoning item without summary", i)
			}
			next := map[string]any{}
			if i+1 < len(input) {
				next, _ = input[i+1].(map[string]any)
			}
			if !(next["type"] == "reasoning" || next["type"] == "function_call" || (next["type"] == "message" && next["role"] == "assistant")) {
				// Stricter than the provider (which accepts it): an orphaned item is reasoning lost or misplaced.
				return fmt.Sprintf("reasoning item %v is not right before the output it produced", it["id"])
			}
		case "function_call":
			id, _ := it["call_id"].(string)
			if id == "" || it["name"] == "" {
				return fmt.Sprintf("input[%d]: function_call without call_id or name", i)
			}
			if calls[id] {
				return "Duplicate function_call call_id " + id
			}
			calls[id] = true
		case "function_call_output":
			id, _ := it["call_id"].(string)
			if !calls[id] {
				return "No tool call found for function call output with call_id " + id + "."
			}
			outputs[id] = true
		default:
			return fmt.Sprintf("input[%d]: unknown item type %v", i, it["type"])
		}
	}
	for id := range calls {
		if !outputs[id] {
			return "No tool output found for function call " + id + "."
		}
	}
	return ""
}

// unknownReasoning is the id of a reasoning item this server did not issue ("" when none).
func (f *fakeResponses) unknownReasoning(req map[string]any) string {
	input, _ := req["input"].([]any)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, raw := range input {
		it, _ := raw.(map[string]any)
		if it["type"] != "reasoning" {
			continue
		}
		id, _ := it["id"].(string)
		if enc, _ := it["encrypted_content"].(string); f.issued[id] != enc {
			return id
		}
	}
	return ""
}

func (f *fakeResponses) issue(name string) (id, enc string) {
	id = f.id("rs")
	enc = "llmtest:" + base64.StdEncoding.EncodeToString([]byte(name+"/"+id))
	f.mu.Lock()
	f.issued[id] = enc
	f.mu.Unlock()
	return id, enc
}

// errorStatus maps a scripted error to the HTTP status and code the provider would send.
func errorStatus(err error) (int, string, string) {
	msg := err.Error()
	var apiErr *goopenai.APIError
	status := 0
	if e, ok := err.(*goopenai.APIError); ok {
		apiErr = e
	}
	if apiErr != nil {
		status = apiErr.HTTPStatusCode
		if apiErr.Message != "" {
			msg = apiErr.Message
		}
	}
	if e, ok := err.(*goopenai.RequestError); ok {
		status = e.HTTPStatusCode
	}
	code := ""
	if apiErr != nil {
		code, _ = apiErr.Code.(string)
	}
	switch llm.Classify(err) {
	case llm.ClassOverflow:
		status, code = http.StatusBadRequest, "context_length_exceeded"
	case llm.ClassRateLimited:
		status, code = http.StatusTooManyRequests, "rate_limit_exceeded"
	case llm.ClassTransient, llm.ClassTimeout:
		if status < 500 {
			status = http.StatusServiceUnavailable
		}
		code = "server_error"
	default:
		if status == 0 || status == http.StatusTooManyRequests || status >= 500 {
			status = http.StatusBadRequest
		}
		if code == "" {
			code = "invalid_request_error"
		}
	}
	return status, code, msg
}

// sse writes the events of one response.
type sse struct {
	w      http.ResponseWriter
	fl     http.Flusher
	f      *fakeResponses
	name   string
	index  int
	output []map[string]any
	open   map[string]any // the item being streamed
	texts  map[string]*strings.Builder
}

func (s *sse) event(typ string, fields map[string]any) {
	fields["type"] = typ
	b, _ := json.Marshal(fields)
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", typ, b)
}

func (s *sse) flush() {
	if s.fl != nil {
		s.fl.Flush()
	}
}

func (s *sse) add(item map[string]any) {
	s.closeOpen()
	s.open = item
	added := map[string]any{}
	for k, v := range item {
		added[k] = v
	}
	if item["type"] == "reasoning" {
		added["encrypted_content"] = nil
	}
	s.event("response.output_item.added", map[string]any{"output_index": s.index, "item": added})
}

func (s *sse) closeOpen() {
	it := s.open
	if it == nil {
		return
	}
	s.open = nil
	id, _ := it["id"].(string)
	switch it["type"] {
	case "message":
		if b := s.texts[id]; b != nil {
			s.event("response.output_text.done", map[string]any{"item_id": id, "output_index": s.index, "content_index": 0, "text": b.String()})
			it["content"] = []any{map[string]any{"type": "output_text", "annotations": []any{}, "text": b.String()}}
		}
	case "function_call":
		s.event("response.function_call_arguments.done", map[string]any{"item_id": id, "output_index": s.index, "arguments": it["arguments"]})
		it["status"] = "completed"
	case "reasoning":
		if b := s.texts[id]; b != nil {
			it["summary"] = []any{map[string]any{"type": "summary_text", "text": b.String()}}
		}
	}
	s.event("response.output_item.done", map[string]any{"output_index": s.index, "item": it})
	s.output = append(s.output, it)
	s.index++
}

func (s *sse) reasoning() map[string]any {
	id, enc := s.f.issue(s.name)
	return map[string]any{"id": id, "type": "reasoning", "summary": []any{}, "content": []any{}, "encrypted_content": enc}
}

func (s *sse) text(kind string, delta string) {
	if s.open == nil || s.open["type"] != kind {
		var item map[string]any
		if kind == "reasoning" {
			item = s.reasoning()
		} else {
			item = map[string]any{"id": s.f.id("msg"), "type": "message", "status": "in_progress", "content": []any{}, "role": "assistant"}
		}
		s.add(item)
		if kind == "reasoning" {
			s.event("response.reasoning_summary_part.added", map[string]any{"item_id": item["id"], "output_index": s.index, "summary_index": 0,
				"part": map[string]any{"type": "summary_text", "text": ""}})
		} else {
			s.event("response.content_part.added", map[string]any{"item_id": item["id"], "output_index": s.index, "content_index": 0,
				"part": map[string]any{"type": "output_text", "annotations": []any{}, "text": ""}})
		}
	}
	id, _ := s.open["id"].(string)
	if s.texts[id] == nil {
		s.texts[id] = &strings.Builder{}
	}
	s.texts[id].WriteString(delta)
	if kind == "reasoning" {
		s.event("response.reasoning_summary_text.delta", map[string]any{"item_id": id, "output_index": s.index, "summary_index": 0, "delta": delta})
	} else {
		s.event("response.output_text.delta", map[string]any{"item_id": id, "output_index": s.index, "content_index": 0, "delta": delta, "obfuscation": "x"})
	}
}

// fakeCall is a tool call merged from the scripted fragments.
type fakeCall struct {
	id, name string
	item     map[string]any
}

// mergeCalls groups the scripted tool-call fragments into calls (like the
// agent does for Chat Completions streams): the key of every fragment, by chunk.
func mergeCalls(chunks []*schema.Message) ([][]string, map[string]*fakeCall) {
	keys := make([][]string, len(chunks))
	calls := map[string]*fakeCall{}
	byIndex := map[int]string{}
	last := ""
	for ci, c := range chunks {
		if c == nil {
			continue
		}
		for pos, tc := range c.ToolCalls {
			var key string
			switch {
			case tc.Index != nil:
				k, ok := byIndex[*tc.Index]
				if !ok {
					k = fmt.Sprintf("i:%d", *tc.Index)
				}
				if fc := calls[k]; fc != nil && tc.ID != "" && fc.id != "" && tc.ID != fc.id {
					k = fmt.Sprintf("i:%d:%s", *tc.Index, tc.ID)
				}
				byIndex[*tc.Index] = k
				key = k
			case tc.ID != "":
				key = "id:" + tc.ID
			case pos == 0 && last != "" && (tc.Function.Name == "" || calls[last] == nil || calls[last].name == ""):
				key = last
			default:
				key = fmt.Sprintf("p:%d:%d", ci, pos)
			}
			last = key
			fc := calls[key]
			if fc == nil {
				fc = &fakeCall{}
				calls[key] = fc
			}
			if fc.id == "" {
				fc.id = tc.ID
			}
			if fc.name == "" {
				fc.name = tc.Function.Name
			}
			keys[ci] = append(keys[ci], key)
		}
	}
	return keys, calls
}

func (f *fakeResponses) play(w http.ResponseWriter, r *http.Request, p *pendingTurn) {
	t := p.turn
	if t.Err != nil {
		status, code, msg := errorStatus(t.Err)
		writeError(w, status, code, msg, t.RetryAfter)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	s := &sse{w: w, fl: fl, f: f, name: p.name, texts: map[string]*strings.Builder{}}
	respID := f.id("resp")
	s.event("response.created", map[string]any{"response": map[string]any{"id": respID, "object": "response", "status": "in_progress", "model": p.name, "output": []any{}}})
	s.event("response.in_progress", map[string]any{"response": map[string]any{"id": respID, "object": "response", "status": "in_progress", "model": p.name, "output": []any{}}})
	hasReasoning := false
	for _, c := range t.Chunks {
		if c != nil && c.ReasoningContent != "" {
			hasReasoning = true
		}
	}
	if !hasReasoning { // gpt-5.6 answers start with an (encrypted) reasoning item
		s.add(s.reasoning())
		s.closeOpen()
	}
	if t.Refusal != "" {
		item := map[string]any{"id": f.id("msg"), "type": "message", "status": "in_progress", "content": []any{}, "role": "assistant"}
		s.add(item)
		s.event("response.refusal.delta", map[string]any{"item_id": item["id"], "output_index": s.index, "content_index": 0, "delta": t.Refusal})
		item["content"] = []any{map[string]any{"type": "refusal", "refusal": t.Refusal}}
		s.closeOpen()
	}
	s.flush()
	if t.KeepAlive > 0 && !keepAlive(r.Context(), t.KeepAlive, func() {
		s.event("response.in_progress", map[string]any{"response": map[string]any{"id": respID, "object": "response", "status": "in_progress", "model": p.name, "output": []any{}}})
		s.flush()
	}) {
		return
	}
	if t.Pings > 0 && !keepAlive(r.Context(), t.Pings, func() { fmt.Fprint(w, ": keep-alive\n\n"); s.flush() }) {
		return
	}
	keys, calls := mergeCalls(t.Chunks)
	finish := ""
	var u schema.TokenUsage
	hasUsage := false
	for ci, c := range t.Chunks {
		if t.ChunkDelay > 0 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(t.ChunkDelay):
			}
		}
		if c == nil {
			continue
		}
		if c.ReasoningContent != "" {
			s.text("reasoning", c.ReasoningContent)
		}
		if c.Content != "" {
			s.text("message", c.Content)
		}
		for pos, tc := range c.ToolCalls {
			fc := calls[keys[ci][pos]]
			if fc.item == nil {
				if fc.id == "" {
					fc.id = "call_" + strings.TrimPrefix(f.id("llmtest"), "llmtest_")
				}
				fc.item = map[string]any{"id": f.id("fc"), "type": "function_call", "status": "in_progress", "arguments": "", "call_id": fc.id, "name": fc.name}
				s.add(fc.item)
			}
			if tc.Function.Arguments == "" {
				continue
			}
			// Interleaved fragments of an earlier call keep its item id (the client merges them by id).
			fc.item["arguments"] = fc.item["arguments"].(string) + tc.Function.Arguments
			s.event("response.function_call_arguments.delta", map[string]any{"item_id": fc.item["id"], "output_index": s.index, "delta": tc.Function.Arguments, "obfuscation": "x"})
		}
		if rm := c.ResponseMeta; rm != nil {
			if rm.FinishReason != "" {
				finish = rm.FinishReason
			}
			if x := rm.Usage; x != nil {
				hasUsage = true
				u.PromptTokens = max(u.PromptTokens, x.PromptTokens)
				u.CompletionTokens = max(u.CompletionTokens, x.CompletionTokens)
				u.TotalTokens = max(u.TotalTokens, x.TotalTokens)
				u.PromptTokenDetails.CachedTokens = max(u.PromptTokenDetails.CachedTokens, x.PromptTokenDetails.CachedTokens)
				u.CompletionTokensDetails.ReasoningTokens = max(u.CompletionTokensDetails.ReasoningTokens, x.CompletionTokensDetails.ReasoningTokens)
			}
		}
		s.flush()
	}
	if t.Block {
		<-r.Context().Done()
		return
	}
	if t.FailAfter != nil {
		_, code, msg := errorStatus(t.FailAfter)
		s.event("error", map[string]any{"code": code, "message": msg, "param": nil})
		s.flush()
		return
	}
	s.closeOpen()
	resp := map[string]any{"id": respID, "object": "response", "status": "completed", "model": p.name, "output": s.output}
	if hasUsage {
		resp["usage"] = map[string]any{
			"input_tokens": u.PromptTokens, "input_tokens_details": map[string]any{"cached_tokens": u.PromptTokenDetails.CachedTokens, "cache_write_tokens": 0},
			"output_tokens": u.CompletionTokens, "output_tokens_details": map[string]any{"reasoning_tokens": u.CompletionTokensDetails.ReasoningTokens},
			"total_tokens": u.TotalTokens,
		}
	}
	typ := "response.completed"
	switch finish {
	case "length":
		typ, resp["status"], resp["incomplete_details"] = "response.incomplete", "incomplete", map[string]any{"reason": "max_output_tokens"}
	case "content_filter":
		typ, resp["status"], resp["incomplete_details"] = "response.incomplete", "incomplete", map[string]any{"reason": "content_filter"}
	}
	s.event(typ, map[string]any{"response": resp})
	s.flush()
	f.mu.Lock()
	delete(f.pending, r.Header.Get(TurnHeader))
	f.mu.Unlock()
}
