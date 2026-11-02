package anthropic_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/anthropic"
)

// capture returns a model whose server records the decoded request and the headers, and answers with an empty message.
func capture(t *testing.T, cfg anthropic.Config) (*anthropic.Model, *map[string]any, *http.Header) {
	t.Helper()
	var body map[string]any
	var hdr http.Header
	m := newModel(t, func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		body = nil
		_ = json.NewDecoder(r.Body).Decode(&body)
		io.WriteString(w, events(`{"type":"message_stop"}`))
	}, cfg)
	return m, &body, &hdr
}

func sub(t *testing.T, v any, path ...any) any {
	t.Helper()
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("at %q: %T is not an object", k, v)
			}
			v = m[k]
		case int:
			a, ok := v.([]any)
			if !ok || k >= len(a) {
				t.Fatalf("at [%d]: %v is not an array that long", k, v)
			}
			v = a[k]
		}
	}
	return v
}

func TestRequestCarriesAuthVersionAndConfiguredHeaders(t *testing.T) {
	// Arrange
	m, _, hdr := capture(t, anthropic.Config{Header: http.Header{"X-Proxy": {"a", "b"}}})

	// Act
	_, err := m.Generate(context.Background(), []*ai.Message{ai.UserMessage("hi")})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	h := *hdr
	if h.Get("x-api-key") != "k" || h.Get("anthropic-version") != "2023-06-01" ||
		h.Get("Content-Type") != "application/json" || h.Get("Accept") != "text/event-stream" {
		t.Errorf("headers %v", h)
	}
	if got := h.Values("X-Proxy"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("proxy header %v", got)
	}
}

func TestRequestOmitsTheAPIKeyHeaderWhenNoneIsConfigured(t *testing.T) {
	// Arrange
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		io.WriteString(w, events(`{"type":"message_stop"}`))
	}))
	defer srv.Close()
	m, _ := anthropic.New(anthropic.Config{BaseURL: srv.URL + "/", Model: "m"}) // trailing slash must not double up

	// Act
	_, err := m.Generate(context.Background(), []*ai.Message{ai.UserMessage("hi")})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := hdr["X-Api-Key"]; ok {
		t.Errorf("empty key sent: %v", hdr)
	}
}

func TestNewRequiresAModel(t *testing.T) {
	// Act
	_, err := anthropic.New(anthropic.Config{})

	// Assert
	if err == nil {
		t.Fatal("a model with no id was accepted")
	}
}

func TestCallOptionsReachTheBody(t *testing.T) {
	// Arrange
	m, body, _ := capture(t, anthropic.Config{})

	// Act
	_, err := m.Generate(context.Background(), []*ai.Message{ai.UserMessage("hi")})
	defaults := *body
	_, err2 := m.Generate(context.Background(), []*ai.Message{ai.UserMessage("hi")},
		ai.WithMaxTokens(77), ai.WithTemperature(0.25), ai.WithTopP(0.5))
	set := *body
	_, err3 := m.Generate(context.Background(), []*ai.Message{ai.UserMessage("hi")}, ai.WithMaxTokens(0))
	zero := *body

	// Assert
	if err != nil || err2 != nil || err3 != nil {
		t.Fatal(err, err2, err3)
	}
	if defaults["max_tokens"] != float64(anthropic.DefaultMaxTokens) || defaults["model"] != "claude-test" || defaults["stream"] != true {
		t.Errorf("defaults %v", defaults)
	}
	if _, ok := defaults["temperature"]; ok {
		t.Errorf("unset temperature sent: %v", defaults)
	}
	if set["max_tokens"] != float64(77) || set["temperature"] != 0.25 || set["top_p"] != 0.5 {
		t.Errorf("options %v", set)
	}
	if zero["max_tokens"] != float64(anthropic.DefaultMaxTokens) {
		t.Errorf("max_tokens 0 sent %v, want the default", zero["max_tokens"])
	}
}

func TestToolChoiceMapsToTheAnthropicForms(t *testing.T) {
	m, body, _ := capture(t, anthropic.Config{})
	tm, err := m.WithTools([]*ai.ToolInfo{{Name: "a"}, {Name: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		opts []ai.Option
		want string
	}{
		{"none given", nil, "<absent>"},
		{"forbidden", []ai.Option{ai.WithToolChoice(ai.ToolChoiceForbidden)}, `{"type":"none"}`},
		{"allowed", []ai.Option{ai.WithToolChoice(ai.ToolChoiceAllowed)}, `{"type":"auto"}`},
		{"forced any", []ai.Option{ai.WithToolChoice(ai.ToolChoiceForced)}, `{"type":"any"}`},
		{"forced one", []ai.Option{ai.WithToolChoice(ai.ToolChoiceForced, "b")}, `{"name":"b","type":"tool"}`},
		{"forced two", []ai.Option{ai.WithToolChoice(ai.ToolChoiceForced, "a", "b")}, `{"type":"any"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Act
			if _, err := tm.Generate(context.Background(), []*ai.Message{ai.UserMessage("hi")}, tc.opts...); err != nil {
				t.Fatal(err)
			}

			// Assert
			got := "<absent>"
			if v, ok := (*body)["tool_choice"]; ok {
				b, _ := json.Marshal(v)
				got = string(b)
			}
			if got != tc.want {
				t.Errorf("tool_choice %s, want %s", got, tc.want)
			}
		})
	}
}

func TestToolChoiceIsNotSentWithoutTools(t *testing.T) {
	// Arrange
	m, body, _ := capture(t, anthropic.Config{})

	// Act
	_, err := m.Generate(context.Background(), []*ai.Message{ai.UserMessage("hi")}, ai.WithToolChoice(ai.ToolChoiceForced))

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := (*body)["tool_choice"]; ok {
		t.Errorf("tool_choice sent with no tools: %v", *body)
	}
	if _, ok := (*body)["tools"]; ok {
		t.Errorf("tools sent: %v", *body)
	}
}

func TestToolSchemasAreSentAsObjectSchemasWithoutMutatingTheCallersInfo(t *testing.T) {
	// Arrange
	m, body, _ := capture(t, anthropic.Config{NoCache: true})
	params := map[string]any{"$schema": "http://json-schema.org/draft-07/schema#", "type": "object"}
	withProps := map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}, "required": []string{"q"}}
	tm, err := m.WithTools([]*ai.ToolInfo{
		nil,
		{Name: "bare", Desc: "no params"},
		{Name: "noprops", Parameters: params},
		{Name: "full", Desc: "F", Parameters: withProps},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Act
	_, err = tm.Generate(context.Background(), []*ai.Message{ai.UserMessage("hi")})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	tools := sub(t, *body, "tools").([]any)
	if len(tools) != 3 {
		t.Fatalf("tools %v: a nil info must be skipped", tools)
	}
	b, _ := json.Marshal(tools)
	want := `[{"description":"no params","input_schema":{"properties":{},"type":"object"},"name":"bare"},` +
		`{"input_schema":{"properties":{},"type":"object"},"name":"noprops"},` +
		`{"description":"F","input_schema":{"properties":{"q":{"type":"string"}},"required":["q"],"type":"object"},"name":"full"}]`
	if string(b) != want {
		t.Errorf("tools\n got %s\nwant %s", b, want)
	}
	if _, ok := params["$schema"]; !ok || len(params) != 2 {
		t.Errorf("caller's schema was edited: %v", params)
	}
}

func TestCacheBreakpointsUseTheConfiguredTTL(t *testing.T) {
	for ttl, want := range map[string]string{"": "<none>", "5m": "<none>", "1h": "1h"} {
		t.Run("ttl "+ttl, func(t *testing.T) {
			// Arrange
			m, body, _ := capture(t, anthropic.Config{CacheTTL: ttl})
			tm, _ := m.WithTools([]*ai.ToolInfo{{Name: "a"}})

			// Act
			_, err := tm.Generate(context.Background(), []*ai.Message{ai.SystemMessage("s"), ai.UserMessage("u")})

			// Assert
			if err != nil {
				t.Fatal(err)
			}
			for _, cc := range []any{
				sub(t, *body, "tools", 0, "cache_control"),
				sub(t, *body, "system", 0, "cache_control"),
				sub(t, *body, "messages", 0, "content", 0, "cache_control"),
			} {
				obj, ok := cc.(map[string]any)
				if !ok || obj["type"] != "ephemeral" {
					t.Fatalf("breakpoint %v", cc)
				}
				got, has := obj["ttl"].(string)
				if !has {
					got = "<none>"
				}
				if got != want {
					t.Errorf("ttl %q, want %q", got, want)
				}
			}
		})
	}
}

func TestMessagesMergeIntoAlternatingRolesAndDropEmptyOnes(t *testing.T) {
	// Arrange
	m, body, _ := capture(t, anthropic.Config{NoCache: true})
	msgs := []*ai.Message{
		nil,
		ai.SystemMessage("   "), // blank: no system prompt at all
		ai.UserMessage("one"),
		ai.UserMessage("two"), // same role: merged
		ai.AssistantMessage("", nil),
		ai.AssistantMessage("a1", nil),
		ai.AssistantMessage("a2", nil),
		ai.UserMessage(""),
	}

	// Act
	_, err := m.Generate(context.Background(), msgs)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := (*body)["system"]; ok {
		t.Errorf("blank system prompt sent: %v", (*body)["system"])
	}
	b, _ := json.Marshal((*body)["messages"])
	want := `[{"content":[{"text":"one","type":"text"},{"text":"two","type":"text"}],"role":"user"},` +
		`{"content":[{"text":"a1","type":"text"},{"text":"a2","type":"text"}],"role":"assistant"},` +
		`{"content":[{"text":"(empty message)","type":"text"}],"role":"user"}]`
	if string(b) != want {
		t.Errorf("messages\n got %s\nwant %s", b, want)
	}
}

func TestAssistantToolCallsBecomeToolUseBlocksWithObjectInput(t *testing.T) {
	// Arrange
	m, body, _ := capture(t, anthropic.Config{NoCache: true})
	call := func(id, name, args string) ai.ToolCall {
		return ai.ToolCall{ID: id, Type: "function", Function: ai.FunctionCall{Name: name, Arguments: args}}
	}

	// Act
	_, err := m.Generate(context.Background(), []*ai.Message{
		ai.UserMessage("go"),
		{Role: ai.RoleAssistant, ToolCalls: []ai.ToolCall{
			call("t1", "ok", `{"a":1}`),
			call("t2", "empty", "  "),
			call("t3", "broken", `{"a":`),
			call("t4", "null", `null`),
			call("t5", "array", `[1]`),
		}},
	})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	blocks := sub(t, *body, "messages", 1, "content").([]any)
	if len(blocks) != 5 {
		t.Fatalf("blocks %v", blocks)
	}
	for i, wantInput := range []string{`{"a":1}`, `{}`, `{}`, `{}`, `{}`} {
		blk := blocks[i].(map[string]any)
		in, hasInput := blk["input"]
		if !hasInput {
			t.Errorf("block %d has no input: the API requires one on every tool_use", i)
		}
		if blk["type"] != "tool_use" {
			t.Errorf("block %d type %v", i, blk["type"])
		}
		b, _ := json.Marshal(in)
		if got := string(b); got != wantInput {
			t.Errorf("block %d input %s, want %s", i, got, wantInput)
		}
	}
}

func TestToolResultsGoFirstInTheUserMessageThatFollowsTheCalls(t *testing.T) {
	// Arrange
	m, body, _ := capture(t, anthropic.Config{NoCache: true})

	// Act: a result with no user message after it opens its own.
	_, err := m.Generate(context.Background(), []*ai.Message{
		{Role: ai.RoleAssistant, ToolCalls: []ai.ToolCall{{ID: "t1", Function: ai.FunctionCall{Name: "a", Arguments: "{}"}}}},
		ai.ToolMessage("r1", "t1"),
	})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(sub(t, *body, "messages", 1))
	want := `{"content":[{"content":"r1","tool_use_id":"t1","type":"tool_result"}],"role":"user"}`
	if string(b) != want {
		t.Errorf("tool result message\n got %s\nwant %s", b, want)
	}
}

func TestUserImagesAreSentAsBase64OrURLSources(t *testing.T) {
	// Arrange
	m, body, _ := capture(t, anthropic.Config{NoCache: true})
	parts := []ai.Part{
		{Type: ai.PartText, Text: "look"},
		{Type: ai.PartText}, // empty text: dropped
		{Type: ai.PartImage, Image: &ai.Image{Base64Data: "QUJD", MIMEType: "image/jpeg"}},
		{Type: ai.PartImage, Image: &ai.Image{Base64Data: "REVG"}}, // media type defaults to png
		{Type: ai.PartImage, Image: &ai.Image{URL: "data:image/webp;base64,R0hJ"}},
		{Type: ai.PartImage, Image: &ai.Image{URL: "data:broken-no-comma"}},
		{Type: ai.PartImage, Image: &ai.Image{URL: "https://example.com/a.png"}},
		{Type: ai.PartImage, Image: &ai.Image{}}, // nothing to send
		{Type: ai.PartImage},                     // no image at all
		{Type: "audio"},                          // unknown kinds are ignored
	}

	// Act
	_, err := m.Generate(context.Background(), []*ai.Message{{Role: ai.RoleUser, Content: "ignored when parts exist", Parts: parts}})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(sub(t, *body, "messages", 0, "content"))
	want := `[{"text":"look","type":"text"},` +
		`{"source":{"data":"QUJD","media_type":"image/jpeg","type":"base64"},"type":"image"},` +
		`{"source":{"data":"REVG","media_type":"image/png","type":"base64"},"type":"image"},` +
		`{"source":{"data":"R0hJ","media_type":"image/webp","type":"base64"},"type":"image"},` +
		`{"source":{"type":"url","url":"data:broken-no-comma"},"type":"image"},` +
		`{"source":{"type":"url","url":"https://example.com/a.png"},"type":"image"}]`
	if string(b) != want {
		t.Errorf("content\n got %s\nwant %s", b, want)
	}
}

func TestAUserMessageWithOnlyUnusableImagePartsIsStillSent(t *testing.T) {
	// Arrange
	m, body, _ := capture(t, anthropic.Config{NoCache: true})

	// Act
	_, err := m.Generate(context.Background(), []*ai.Message{{Role: ai.RoleUser, Parts: []ai.Part{{Type: ai.PartImage}}}})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if got := sub(t, *body, "messages", 0, "content", 0, "text"); got != "(empty message)" {
		t.Errorf("placeholder %v", got)
	}
}
