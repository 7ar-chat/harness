package responses_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/responses"
)

// recorded is one request the fake endpoint received.
type recorded struct {
	header http.Header
	body   map[string]any
}

// recorder keeps the requests of a fake endpoint.
type recorder struct {
	mu   sync.Mutex
	reqs []recorded
}

func (r *recorder) all() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recorded(nil), r.reqs...)
}

// serve answers the nth request with replies[n] (the last reply repeats) and records the requests.
func serve(t *testing.T, replies ...func(w http.ResponseWriter)) (string, *recorder) {
	t.Helper()
	rec := &recorder{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		req := recorded{header: r.Header.Clone()}
		_ = json.Unmarshal(b, &req.body)
		rec.mu.Lock()
		n := len(rec.reqs)
		rec.reqs = append(rec.reqs, req)
		rec.mu.Unlock()
		replies[min(n, len(replies)-1)](w)
	}))
	t.Cleanup(ts.Close)
	return ts.URL + "/v1/responses", rec
}

// stream replies with an SSE body.
func stream(body string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}
}

// failure replies with an HTTP error.
func failure(status int, body string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// event renders one SSE event of the given type with extra JSON fields.
func event(typ string, fields map[string]any) string {
	m := map[string]any{"type": typ}
	for k, v := range fields {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return "event: " + typ + "\ndata: " + string(b) + "\n\n"
}

// completed is the terminal event of a finished response.
func completed(response map[string]any) string {
	if response == nil {
		response = map[string]any{}
	}
	response["status"] = "completed"
	return event("response.completed", map[string]any{"response": response})
}

const okStream = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"m\",\"delta\":\"ok\"}\n\n" +
	"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"

func newModel(t *testing.T, url string, mutate ...func(*responses.Config)) *responses.Model {
	t.Helper()
	cfg := responses.Config{URL: url, Model: "gpt-x"}
	for _, f := range mutate {
		f(&cfg)
	}
	m, err := responses.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// recv drains a stream into its chunks and the error that ended it (nil: io.EOF).
func recv(sr *ai.StreamReader[*ai.Message]) ([]*ai.Message, error) {
	defer sr.Close()
	var chunks []*ai.Message
	for {
		c, err := sr.Recv()
		if errors.Is(err, io.EOF) {
			return chunks, nil
		}
		if err != nil {
			return chunks, err
		}
		chunks = append(chunks, c)
	}
}

// streamAll runs a streamed call to the end.
func streamAll(t *testing.T, m ai.ChatModel, msgs []*ai.Message, opts ...ai.Option) ([]*ai.Message, error) {
	t.Helper()
	sr, err := m.Stream(context.Background(), msgs, opts...)
	if err != nil {
		return nil, err
	}
	return recv(sr)
}

// jsonOf re-decodes a JSON literal so it compares structurally.
func jsonOf(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("%v: %s", err, s)
	}
	return v
}

func hi() []*ai.Message { return []*ai.Message{ai.UserMessage("hi")} }

func TestNewRequiresURLAndModel(t *testing.T) {
	// Without an endpoint or a model id no call could be made, so New refuses to build the client.
	// Arrange
	cases := map[string]responses.Config{
		"no URL":   {Model: "gpt-x"},
		"no model": {URL: "http://example.invalid/v1/responses"},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			m, err := responses.New(cfg)

			// Assert
			if err == nil || m != nil {
				t.Fatalf("model %v err %v, want a configuration error", m, err)
			}
		})
	}
}

func TestRequestCarriesStatelessEncryptedReasoningSettings(t *testing.T) {
	// Every call is stateless and asks for the encrypted reasoning; the configured headers are sent, multi-valued ones in full.
	// Arrange
	url, rec := serve(t, stream(okStream))
	m := newModel(t, url, func(c *responses.Config) {
		c.Header = http.Header{"Authorization": {"Bearer k"}, "X-Multi": {"a", "b"}}
	})

	// Act
	_, err := streamAll(t, m, hi())

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	req := rec.all()[0]
	wantBody := jsonOf(t, `{"model":"gpt-x","stream":true,"store":false,"include":["reasoning.encrypted_content"],
		"input":[{"type":"message","role":"user","content":"hi"}]}`)
	if !reflect.DeepEqual(req.body, wantBody) {
		t.Fatalf("body %v, want %v", req.body, wantBody)
	}
	if req.header.Get("Authorization") != "Bearer k" || !reflect.DeepEqual(req.header.Values("X-Multi"), []string{"a", "b"}) ||
		req.header.Get("Content-Type") != "application/json" || req.header.Get("Accept") != "text/event-stream" {
		t.Fatalf("headers %v", req.header)
	}
}

func TestCallOptionsReachTheRequest(t *testing.T) {
	// Sampling limits, the prompt-cache key and the reasoning effort/summary are sent under the Responses names; reasoning is left out when neither is set.
	// Arrange
	cases := []struct {
		name string
		cfg  func(*responses.Config)
		opts []ai.Option
		want string
	}{
		{"all", func(c *responses.Config) { c.ReasoningSummary = "auto" },
			[]ai.Option{ai.WithTemperature(0.5), ai.WithTopP(0.25), ai.WithMaxTokens(77), ai.WithPromptCacheKey("thread-1"), ai.WithReasoningEffort("high")},
			`{"temperature":0.5,"top_p":0.25,"max_output_tokens":77,"prompt_cache_key":"thread-1","reasoning":{"effort":"high","summary":"auto"}}`},
		{"effort only", nil, []ai.Option{ai.WithReasoningEffort("low")}, `{"reasoning":{"effort":"low"}}`},
		{"summary only", func(c *responses.Config) { c.ReasoningSummary = "detailed" }, nil, `{"reasoning":{"summary":"detailed"}}`},
		{"none", nil, nil, `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url, rec := serve(t, stream(okStream))
			var mutate []func(*responses.Config)
			if tc.cfg != nil {
				mutate = append(mutate, tc.cfg)
			}
			m := newModel(t, url, mutate...)

			// Act
			_, err := streamAll(t, m, hi(), tc.opts...)

			// Assert
			if err != nil {
				t.Fatal(err)
			}
			body := rec.all()[0].body
			want := jsonOf(t, tc.want).(map[string]any)
			for _, k := range []string{"temperature", "top_p", "max_output_tokens", "prompt_cache_key", "reasoning"} {
				if !reflect.DeepEqual(body[k], want[k]) {
					t.Errorf("%s = %v, want %v", k, body[k], want[k])
				}
			}
		})
	}
}

func TestToolChoiceIsSentOnlyWithToolsAndMapsToTheResponsesForm(t *testing.T) {
	// A forced choice naming exactly one tool pins that function, several names or none require some tool, and without tools no choice is sent at all.
	// Arrange
	tools := []*ai.ToolInfo{{Name: "a"}, {Name: "b"}}
	cases := []struct {
		name  string
		tools bool
		opts  []ai.Option
		want  any
	}{
		{"forbidden", true, []ai.Option{ai.WithToolChoice(ai.ToolChoiceForbidden)}, "none"},
		{"allowed", true, []ai.Option{ai.WithToolChoice(ai.ToolChoiceAllowed)}, "auto"},
		{"forced any", true, []ai.Option{ai.WithToolChoice(ai.ToolChoiceForced)}, "required"},
		{"forced one", true, []ai.Option{ai.WithToolChoice(ai.ToolChoiceForced, "b")}, map[string]any{"type": "function", "name": "b"}},
		{"forced two", true, []ai.Option{ai.WithToolChoice(ai.ToolChoiceForced, "a", "b")}, "required"},
		{"unset", true, nil, nil},
		{"no tools", false, []ai.Option{ai.WithToolChoice(ai.ToolChoiceForced, "a")}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url, rec := serve(t, stream(okStream))
			var m ai.ChatModel = newModel(t, url)
			if tc.tools {
				var err error
				if m, err = m.(ai.Model).WithTools(tools); err != nil {
					t.Fatal(err)
				}
			}

			// Act
			_, err := streamAll(t, m, hi(), tc.opts...)

			// Assert
			if err != nil {
				t.Fatal(err)
			}
			body := rec.all()[0].body
			if !reflect.DeepEqual(body["tool_choice"], tc.want) {
				t.Fatalf("tool_choice %v, want %v", body["tool_choice"], tc.want)
			}
			if _, has := body["tools"]; has != tc.tools {
				t.Fatalf("tools present = %v, want %v", has, tc.tools)
			}
		})
	}
}

func TestWithToolsDoesNotChangeTheReceiver(t *testing.T) {
	// WithTools returns a copy: the model it was called on keeps sending no tools.
	// Arrange
	url, rec := serve(t, stream(okStream))
	base := newModel(t, url)
	withTools, err := base.WithTools([]*ai.ToolInfo{{Name: "calc"}, nil})
	if err != nil {
		t.Fatal(err)
	}

	// Act
	_, errBase := streamAll(t, base, hi())
	_, errTools := streamAll(t, withTools, hi())

	// Assert
	if errBase != nil || errTools != nil {
		t.Fatal(errBase, errTools)
	}
	reqs := rec.all()
	if _, has := reqs[0].body["tools"]; has {
		t.Errorf("receiver sent tools: %v", reqs[0].body["tools"])
	}
	tl, _ := reqs[1].body["tools"].([]any)
	if len(tl) != 1 || tl[0].(map[string]any)["name"] != "calc" {
		t.Errorf("copy sent tools %v, want just calc (nil info skipped)", reqs[1].body["tools"])
	}
}

func TestHistoryIsConvertedToResponsesInputItems(t *testing.T) {
	// System/user text, assistant text and calls, tool results: each becomes its item, empty call arguments become {}, nil messages and empty assistant text are dropped, unknown roles are ignored.
	// Arrange
	url, rec := serve(t, stream(okStream))
	m := newModel(t, url)
	history := []*ai.Message{
		ai.SystemMessage("be brief"),
		nil,
		ai.UserMessage("add"),
		ai.AssistantMessage("calling", []ai.ToolCall{
			{ID: "c1", Function: ai.FunctionCall{Name: "calc", Arguments: `{"a":1}`}},
			{ID: "c2", Function: ai.FunctionCall{Name: "now", Arguments: "  "}},
		}),
		ai.ToolMessage("2", "c1"),
		ai.ToolMessage("noon", "c2"),
		ai.AssistantMessage("", []ai.ToolCall{{ID: "c3", Function: ai.FunctionCall{Name: "now"}}}),
		{Role: "narrator", Content: "ignored"},
		ai.AssistantMessage("done", nil),
	}

	// Act
	_, err := streamAll(t, m, history)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	want := jsonOf(t, `[
		{"type":"message","role":"system","content":"be brief"},
		{"type":"message","role":"user","content":"add"},
		{"type":"message","role":"assistant","content":"calling"},
		{"type":"function_call","call_id":"c1","name":"calc","arguments":"{\"a\":1}"},
		{"type":"function_call","call_id":"c2","name":"now","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"2"},
		{"type":"function_call_output","call_id":"c2","output":"noon"},
		{"type":"function_call","call_id":"c3","name":"now","arguments":"{}"},
		{"type":"message","role":"assistant","content":"done"}]`)
	if got := rec.all()[0].body["input"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("input %v\nwant %v", got, want)
	}
}

func TestUserPartsBecomeInputTextAndImages(t *testing.T) {
	// Text parts and images (URL, or base64 with a default PNG type) are sent with an image detail defaulting to auto; unusable images are dropped and an unknown part kind is ignored.
	// Arrange
	url, rec := serve(t, stream(okStream))
	m := newModel(t, url)
	msg := &ai.Message{Role: ai.RoleUser, Content: "ignored when parts exist", Parts: []ai.Part{
		{Type: ai.PartText, Text: "look"},
		{Type: ai.PartImage, Image: &ai.Image{URL: "https://img/x.png", Detail: "low"}},
		{Type: ai.PartImage, Image: &ai.Image{Base64Data: "QUJD"}},
		{Type: ai.PartImage, Image: &ai.Image{Base64Data: "QUJD", MIMEType: "image/jpeg", Detail: "high"}},
		{Type: ai.PartImage, Image: &ai.Image{MIMEType: "image/png"}},
		{Type: ai.PartImage},
		{Type: "audio", Text: "x"},
	}}

	// Act
	_, err := streamAll(t, m, []*ai.Message{msg})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	want := jsonOf(t, `[{"type":"message","role":"user","content":[
		{"type":"input_text","text":"look"},
		{"type":"input_image","image_url":"https://img/x.png","detail":"low"},
		{"type":"input_image","image_url":"data:image/png;base64,QUJD","detail":"auto"},
		{"type":"input_image","image_url":"data:image/jpeg;base64,QUJD","detail":"high"}]}]`)
	if got := rec.all()[0].body["input"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("input %v\nwant %v", got, want)
	}
}

func TestOnlyUsableReasoningItemsOfThisModelPrecedeTheAnswer(t *testing.T) {
	// A reasoning item travels back only when it has an id and encrypted content, was produced by this model, and its message has output to follow; an empty summary is sent as [] not null.
	// Arrange
	url, rec := serve(t, stream(okStream))
	m := newModel(t, url, func(c *responses.Config) { c.Name = "alias" })
	withItems := func(content string, calls []ai.ToolCall, items ...responses.ReasoningItem) *ai.Message {
		msg := ai.AssistantMessage(content, calls)
		responses.SetItems(msg, items)
		return msg
	}
	history := []*ai.Message{
		ai.UserMessage("q"),
		withItems("a1", nil,
			responses.ReasoningItem{ID: "rs_ok", EncryptedContent: "e1", Model: "alias", Summary: []responses.SummaryPart{{Type: "summary_text", Text: "thought"}}},
			responses.ReasoningItem{ID: "rs_nosum", EncryptedContent: "e2", Model: "alias"},
			responses.ReasoningItem{ID: "", EncryptedContent: "e3", Model: "alias"},
			responses.ReasoningItem{ID: "rs_noenc", Model: "alias"},
			responses.ReasoningItem{ID: "rs_gpt", EncryptedContent: "e4", Model: "gpt-x"},
			responses.ReasoningItem{ID: "rs_other", EncryptedContent: "e5", Model: "other"}),
		ai.UserMessage("q2"),
		withItems("", nil, responses.ReasoningItem{ID: "rs_dangling", EncryptedContent: "e6", Model: "alias"}),
		ai.UserMessage("q3"),
		withItems("", []ai.ToolCall{{ID: "c1", Function: ai.FunctionCall{Name: "f", Arguments: "{}"}}},
			responses.ReasoningItem{ID: "rs_call", EncryptedContent: "e7", Model: "alias"}),
	}

	// Act
	_, err := streamAll(t, m, history)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	want := jsonOf(t, `[
		{"type":"message","role":"user","content":"q"},
		{"type":"reasoning","id":"rs_ok","summary":[{"type":"summary_text","text":"thought"}],"encrypted_content":"e1"},
		{"type":"reasoning","id":"rs_nosum","summary":[],"encrypted_content":"e2"},
		{"type":"message","role":"assistant","content":"a1"},
		{"type":"message","role":"user","content":"q2"},
		{"type":"message","role":"user","content":"q3"},
		{"type":"reasoning","id":"rs_call","summary":[],"encrypted_content":"e7"},
		{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}]`)
	if got := rec.all()[0].body["input"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("input %v\nwant %v", got, want)
	}
}

func TestReasoningItemsAreTaggedWithTheModelNameWhichDefaultsToTheModelId(t *testing.T) {
	// Without Config.Name the model id tags (and matches) its reasoning items, so items of an answer come back to the model that produced them.
	// Arrange
	reasoningDone := event("response.output_item.done", map[string]any{"item": map[string]any{"type": "reasoning", "id": "rs_1", "encrypted_content": "enc"}})
	url, rec := serve(t, stream(reasoningDone+okStream))
	m := newModel(t, url)
	first, err := m.Generate(context.Background(), hi())
	if err != nil {
		t.Fatal(err)
	}

	// Act
	_, err = m.Generate(context.Background(), []*ai.Message{ai.UserMessage("hi"), first, ai.UserMessage("again")})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	items := responses.ItemsOf(first)
	if len(items) != 1 || items[0].Model != "gpt-x" {
		t.Fatalf("items %+v, want one tagged gpt-x", items)
	}
	want := jsonOf(t, `[{"type":"message","role":"user","content":"hi"},
		{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"enc"},
		{"type":"message","role":"assistant","content":"ok"},
		{"type":"message","role":"user","content":"again"}]`)
	if got := rec.all()[1].body["input"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("second input %v\nwant %v", got, want)
	}
}

func TestReasoningItemsSurviveAJSONRoundTripOfTheMessage(t *testing.T) {
	// A stored conversation (Extra decoded back from JSON as generic maps) still sends its reasoning back.
	// Arrange
	url, rec := serve(t, stream(okStream))
	m := newModel(t, url)
	answer := ai.AssistantMessage("a", nil)
	responses.SetItems(answer, []responses.ReasoningItem{{ID: "rs_1", EncryptedContent: "enc", Model: "gpt-x", Summary: []responses.SummaryPart{{Type: "summary_text", Text: "t"}}}})
	raw, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	var stored ai.Message
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}

	// Act
	_, err = streamAll(t, m, []*ai.Message{ai.UserMessage("q"), &stored})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	want := jsonOf(t, `[{"type":"message","role":"user","content":"q"},
		{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"t"}],"encrypted_content":"enc"},
		{"type":"message","role":"assistant","content":"a"}]`)
	if got := rec.all()[0].body["input"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("input %v\nwant %v", got, want)
	}
}

// reasoningHistory is a conversation whose assistant message carries one reasoning item.
func reasoningHistory(id string) []*ai.Message {
	am := ai.AssistantMessage("answer", nil)
	responses.SetItems(am, []responses.ReasoningItem{{ID: id, EncryptedContent: "enc-" + id, Model: "gpt-x"}})
	return []*ai.Message{ai.UserMessage("q"), am, ai.UserMessage("next")}
}

// reasoningIDs lists the reasoning item ids of a recorded request.
func reasoningIDs(req recorded) []string {
	var ids []string
	input, _ := req.body["input"].([]any)
	for _, raw := range input {
		if it := raw.(map[string]any); it["type"] == "reasoning" {
			ids = append(ids, it["id"].(string))
		}
	}
	return ids
}

func TestRejectedReasoningIsResentOnceWithoutItAndNotSentAgain(t *testing.T) {
	// When the provider refuses the reasoning items (key change) the call is re-sent once without them, the answer is delivered, the refused items are never sent again, and new items still are.
	// Arrange
	rejection := failure(400, `{"error":{"code":"invalid_encrypted_content","type":"invalid_request_error","message":"The encrypted content for item rs_old could not be verified."}}`)
	url, rec := serve(t, rejection, stream(okStream))
	var logs bytes.Buffer
	m := newModel(t, url, func(c *responses.Config) { c.Logger = slog.New(slog.NewJSONHandler(&logs, nil)) })
	withTools, err := m.WithTools([]*ai.ToolInfo{{Name: "t"}})
	if err != nil {
		t.Fatal(err)
	}

	// Act
	out, err := m.Generate(context.Background(), reasoningHistory("rs_old"))
	_, errAgain := withTools.Generate(context.Background(), reasoningHistory("rs_old"))
	_, errNew := m.Generate(context.Background(), reasoningHistory("rs_new"))

	// Assert
	if err != nil || errAgain != nil || errNew != nil || out.Content != "ok" {
		t.Fatalf("out %+v errs %v %v %v", out, err, errAgain, errNew)
	}
	reqs := rec.all()
	var got [][]string
	for _, r := range reqs {
		got = append(got, reasoningIDs(r))
	}
	want := [][]string{{"rs_old"}, nil, nil, {"rs_new"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reasoning sent per request %v, want %v", got, want)
	}
	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &entry); err != nil {
		t.Fatalf("log %q: %v", logs.String(), err)
	}
	if entry["level"] != "WARN" || entry["model"] != "gpt-x" || entry["items"] != float64(1) {
		t.Fatalf("log entry %v, want one WARN for gpt-x with 1 item", entry)
	}
}

func TestReasoningRejectionIsRecognisedFromTheErrorOnly(t *testing.T) {
	// Only a 400/404 that talks about the encrypted content or a reasoning item triggers the re-send; other failures, or a request with nothing to drop, are returned after one attempt.
	// Arrange
	cases := []struct {
		name     string
		status   int
		body     string
		reasoned bool
		requests int
	}{
		{"encrypted code", 400, `{"error":{"code":"invalid_encrypted_content","message":"bad"}}`, true, 2},
		{"unknown item 404", 404, `{"error":{"message":"Item with id 'rs_9' not found."}}`, true, 2},
		{"reasoning item text", 400, `{"error":{"message":"The Reasoning ITEM is malformed"}}`, true, 2},
		{"unrelated 400", 400, `{"error":{"code":"invalid_request_error","message":"temperature out of range"}}`, true, 1},
		{"server error mentioning encrypted", 500, `{"error":{"message":"encrypted store unavailable"}}`, true, 1},
		{"unauthorized", 401, `{"error":{"message":"encrypted keys rejected"}}`, true, 1},
		{"nothing to drop", 400, `{"error":{"code":"invalid_encrypted_content","message":"bad"}}`, false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url, rec := serve(t, failure(tc.status, tc.body), stream(okStream))
			m := newModel(t, url)
			history := hi()
			if tc.reasoned {
				history = reasoningHistory("rs_1")
			}

			// Act
			_, err := m.Generate(context.Background(), history)

			// Assert
			var apiErr *responses.APIError
			if got := len(rec.all()); got != tc.requests {
				t.Fatalf("%d requests, want %d (err %v)", got, tc.requests, err)
			}
			if tc.requests == 1 && (!errors.As(err, &apiErr) || apiErr.Status != tc.status) {
				t.Fatalf("err %v, want the API error with status %d", err, tc.status)
			}
			if tc.requests == 2 && err != nil {
				t.Fatalf("err %v, want the re-sent call to succeed", err)
			}
		})
	}
}

func TestRejectedResendFailingIsReturnedWithoutLooping(t *testing.T) {
	// If the call without the reasoning fails too, that error is returned and no third attempt is made.
	// Arrange
	url, rec := serve(t,
		failure(400, `{"error":{"code":"invalid_encrypted_content","message":"bad"}}`),
		failure(429, `{"error":{"code":"rate_limit_exceeded","message":"slow"}}`))
	m := newModel(t, url)

	// Act
	_, err := m.Generate(context.Background(), reasoningHistory("rs_1"))

	// Assert
	var apiErr *responses.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 429 || len(rec.all()) != 2 {
		t.Fatalf("err %v after %d requests, want the 429 of the second attempt after 2", err, len(rec.all()))
	}
}

func TestStreamFailsWhenTheEndpointCannotBeReached(t *testing.T) {
	// A transport failure is returned from Stream/Generate as is, before any chunk.
	// Arrange
	ts := httptest.NewServer(http.NotFoundHandler())
	url := ts.URL + "/v1/responses"
	ts.Close()
	m := newModel(t, url)

	// Act
	sr, errStream := m.Stream(context.Background(), hi())
	out, errGenerate := m.Generate(context.Background(), hi())

	// Assert
	if errStream == nil || sr != nil || errGenerate == nil || out != nil {
		t.Fatalf("stream err %v generate err %v, want transport errors", errStream, errGenerate)
	}
}

func TestInvalidURLIsAnErrorNotAPanic(t *testing.T) {
	// A malformed endpoint fails the call with an error.
	// Arrange
	m := newModel(t, "http://bad host/\x7f")

	// Act
	_, err := m.Generate(context.Background(), hi())

	// Assert
	if err == nil {
		t.Fatal("want an error for an unusable URL")
	}
}

func TestGenerateMergesStreamedChunksIntoOneMessage(t *testing.T) {
	// Generate concatenates reasoning summary, text and tool-call fragments of the stream into one assistant message with usage and finish reason.
	// Arrange
	body := event("response.reasoning_summary_text.delta", map[string]any{"item_id": "rs", "delta": "think"}) +
		event("response.output_text.delta", map[string]any{"item_id": "m", "delta": "Hel"}) +
		event("response.output_text.delta", map[string]any{"item_id": "m", "delta": "lo"}) +
		event("response.output_item.added", map[string]any{"output_index": 1, "item": map[string]any{"type": "function_call", "id": "fc", "call_id": "call_1", "name": "calc"}}) +
		event("response.function_call_arguments.delta", map[string]any{"item_id": "fc", "delta": `{"a"`}) +
		event("response.function_call_arguments.delta", map[string]any{"item_id": "fc", "delta": `:1}`}) +
		completed(map[string]any{"usage": map[string]any{"input_tokens": 5, "output_tokens": 3, "total_tokens": 8}})
	url, _ := serve(t, stream(body))
	m := newModel(t, url)

	// Act
	out, err := m.Generate(context.Background(), hi())

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if out.Role != ai.RoleAssistant || out.Content != "Hello" || out.ReasoningContent != "think" {
		t.Fatalf("message %+v", out)
	}
	if len(out.ToolCalls) != 1 || out.ToolCalls[0].ID != "call_1" || out.ToolCalls[0].Function.Name != "calc" || out.ToolCalls[0].Function.Arguments != `{"a":1}` {
		t.Fatalf("tool calls %+v", out.ToolCalls)
	}
	if out.ResponseMeta.FinishReason != "tool_calls" || out.ResponseMeta.Usage.TotalTokens != 8 {
		t.Fatalf("meta %+v", out.ResponseMeta)
	}
}

func TestGenerateReturnsTheErrorOfAStreamThatFailsMidAnswer(t *testing.T) {
	// Text received before a failure event is not returned as if it were a complete answer.
	// Arrange
	body := event("response.output_text.delta", map[string]any{"item_id": "m", "delta": "partial"}) +
		event("error", map[string]any{"code": "server_error", "message": "boom"})
	url, _ := serve(t, stream(body))
	m := newModel(t, url)

	// Act
	out, err := m.Generate(context.Background(), hi())

	// Assert
	var apiErr *responses.APIError
	if out != nil || !errors.As(err, &apiErr) || apiErr.Message != "boom" {
		t.Fatalf("out %+v err %v", out, err)
	}
}

func TestClosingTheStreamReleasesTheConnection(t *testing.T) {
	// A consumer that stops reading a long stream and closes it frees the connection (the server sees the request end) instead of leaving the reader goroutine blocked.
	// Arrange
	released := make(chan struct{})
	started := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for range 200 {
			_, _ = w.Write([]byte(event("response.output_text.delta", map[string]any{"item_id": "m", "delta": "x"})))
		}
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(released)
	}))
	t.Cleanup(ts.Close)
	m := newModel(t, ts.URL+"/v1/responses")
	sr, err := m.Stream(context.Background(), hi())
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := sr.Recv(); err != nil {
		t.Fatal(err)
	}

	// Act
	sr.Close()

	// Assert
	<-released
}
