package ai_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai"
)

func idx(i int) *int { return &i }

func TestConcatMessagesJoinsFragmentsOfOneCallByIndex(t *testing.T) {
	chunks := []*ai.Message{
		{Role: ai.RoleAssistant, Content: "a"},
		{ToolCalls: []ai.ToolCall{{Index: idx(1), ID: "b", Type: "function", Function: ai.FunctionCall{Name: "two", Arguments: `{"x"`}}}},
		{ToolCalls: []ai.ToolCall{{Index: idx(0), ID: "a", Type: "function", Function: ai.FunctionCall{Name: "one", Arguments: `{}`}}}},
		{Content: "b", ToolCalls: []ai.ToolCall{{Index: idx(1), Function: ai.FunctionCall{Arguments: `:1}`}}}},
	}

	got, err := ai.ConcatMessages(chunks)

	if err != nil || got.Content != "ab" || len(got.ToolCalls) != 2 {
		t.Fatalf("got %+v err %v", got, err)
	}
	if c := got.ToolCalls[0]; c.ID != "a" || c.Function.Name != "one" || c.Function.Arguments != "{}" {
		t.Fatalf("first call %+v", c)
	}
	if c := got.ToolCalls[1]; c.ID != "b" || c.Function.Arguments != `{"x":1}` {
		t.Fatalf("second call %+v", c)
	}
}

func TestConcatMessagesRefusesChunksThatDisagree(t *testing.T) {
	for name, chunks := range map[string][]*ai.Message{
		"role":      {{Role: ai.RoleAssistant}, {Role: ai.RoleUser}},
		"call id":   {{ToolCalls: []ai.ToolCall{{Index: idx(0), ID: "a"}}}, {ToolCalls: []ai.ToolCall{{Index: idx(0), ID: "b"}}}},
		"call name": {{ToolCalls: []ai.ToolCall{{Index: idx(0), Function: ai.FunctionCall{Name: "a"}}}}, {ToolCalls: []ai.ToolCall{{Index: idx(0), Function: ai.FunctionCall{Name: "b"}}}}},
		"nil chunk": {{Role: ai.RoleAssistant}, nil},
	} {
		if _, err := ai.ConcatMessages(chunks); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestConcatMessagesKeepsTheLargestUsageAndTheLastFinishReason(t *testing.T) {
	got, err := ai.ConcatMessages([]*ai.Message{
		{ResponseMeta: &ai.ResponseMeta{Usage: &ai.TokenUsage{PromptTokens: 10, CompletionTokens: 1}}},
		{ResponseMeta: &ai.ResponseMeta{FinishReason: "stop", Usage: &ai.TokenUsage{PromptTokens: 10, CompletionTokens: 7, PromptTokenDetails: ai.PromptTokenDetails{CachedTokens: 8}}}},
	})

	u := got.ResponseMeta.Usage
	if err != nil || got.ResponseMeta.FinishReason != "stop" || u.PromptTokens != 10 || u.CompletionTokens != 7 || u.PromptTokenDetails.CachedTokens != 8 {
		t.Fatalf("meta %+v usage %+v err %v", got.ResponseMeta, u, err)
	}
}

func TestConcatMessagesMergesExtraByKey(t *testing.T) {
	got, err := ai.ConcatMessages([]*ai.Message{
		{Extra: map[string]any{"items": "[1", "kind": "x"}},
		{Extra: map[string]any{"items": ",2]"}},
	})
	if err != nil || got.Extra["items"] != "[1,2]" || got.Extra["kind"] != "x" {
		t.Fatalf("extra %v err %v", got.Extra, err)
	}
	if _, err := ai.ConcatMessages([]*ai.Message{{Extra: map[string]any{"n": 1}}, {Extra: map[string]any{"n": 2}}}); err == nil {
		t.Fatal("two numbers under one key merged silently")
	}
}

func TestStreamEndsWithEOFAndTheReaderCanStopTheWriter(t *testing.T) {
	sr, sw := ai.Pipe[int](1)
	go func() {
		defer sw.Close()
		sw.Send(1, nil)
		sw.Send(2, nil)
	}()
	a, _ := sr.Recv()
	b, _ := sr.Recv()
	_, err := sr.Recv()
	sr.Close()
	sr.Close() // closing twice is harmless

	if a != 1 || b != 2 || !errors.Is(err, io.EOF) {
		t.Fatalf("got %d %d %v", a, b, err)
	}

	sr2, sw2 := ai.Pipe[int](0)
	sr2.Close()
	if closed := sw2.Send(1, nil); !closed {
		t.Fatal("a send after the reader closed did not report it")
	}
}

type args struct {
	Path  string `json:"path" jsonschema_description:"the file"`
	Limit int    `json:"limit,omitempty"`
}

func TestInferToolDescribesItsArgumentsAndRunsThem(t *testing.T) {
	tl, err := ai.InferTool("read", "reads a file", func(_ context.Context, in args) (map[string]string, error) {
		return map[string]string{"path": in.Path, "html": "<b>"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	info, _ := tl.Info(context.Background())
	out, runErr := tl.InvokableRun(context.Background(), `{"path":"a.go"}`)

	props, _ := info.Parameters["properties"].(map[string]any)
	if info.Name != "read" || info.Desc != "reads a file" || props["path"] == nil || props["limit"] == nil {
		t.Fatalf("info %+v", info)
	}
	if req, _ := info.Parameters["required"].([]any); len(req) != 1 || req[0] != "path" {
		t.Fatalf("required %v, want only path (limit is omitempty)", info.Parameters["required"])
	}
	if runErr != nil || out != `{"html":"<b>","path":"a.go"}` {
		t.Fatalf("out %q err %v (a result is JSON without HTML escaping)", out, runErr)
	}
}

func TestInferToolErrorsNameTheToolAndTheStage(t *testing.T) {
	exploded := errors.New("exploded")
	tl, _ := ai.InferTool("t", "d", func(_ context.Context, in args) (string, error) {
		if in.Path == "boom" {
			return "", exploded
		}
		return "ok", nil
	})

	_, badJSON := tl.InvokableRun(context.Background(), `{"path":`)
	_, failed := tl.InvokableRun(context.Background(), `{"path":"boom"}`)
	text, _ := tl.InvokableRun(context.Background(), `{"path":"x"}`)

	if badJSON == nil || !strings.Contains(badJSON.Error(), "failed to unmarshal arguments") || !strings.Contains(badJSON.Error(), "toolName=t") {
		t.Errorf("bad json: %v", badJSON)
	}
	if failed == nil || !strings.Contains(failed.Error(), "failed to invoke tool") || !errors.Is(failed, exploded) {
		t.Errorf("failed: %v", failed)
	}
	if text != "ok" {
		t.Errorf("a string result is returned as is, got %q", text)
	}
}
