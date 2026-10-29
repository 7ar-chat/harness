package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/core/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
)

func newSite(t *testing.T, model *llmtest.Model, backend *sandboxtest.Backend) *httptest.Server {
	t.Helper()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	t.Setenv("LLM_DEFAULT_MODEL", "gpt-test")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	h, err := newHarness(cfg, llmtest.Factory{M: model}, backend, nil, demoOrders{"alice/A-1042": "shipped 2026-10-20"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	go func() { _ = h.Run(ctx) }()
	srv := httptest.NewServer((&site{h: h, model: cfg.LLMDefaultModel}).handler())
	t.Cleanup(func() { srv.Close(); cancel() })
	return srv
}

// call performs a request as user ("" sends no identity).
func call(t *testing.T, srv *httptest.Server, method, path, user, body string) (int, map[string]any, string) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if user != "" {
		req.Header.Set("X-Site-User", user)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, m, string(raw)
}

func lookupModel() *llmtest.Model {
	return llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if len(c.Tools) == 0 {
			return llmtest.Turn{Chunks: llmtest.Text("A title")}
		}
		if c.Messages[len(c.Messages)-1].Role == schema.Tool {
			return llmtest.Turn{Chunks: llmtest.Text("Your order is on its way.")}
		}
		return llmtest.Turn{Chunks: []*schema.Message{llmtest.Finish(llmtest.ToolCall(0, "c1", "lookup_order", `{"orderId":"A-1042"}`), "tool_calls")}}
	})
}

func TestAnAnswerRunNeedsNoSandboxAndUsesTheSitesTool(t *testing.T) {
	// Arrange
	backend := sandboxtest.NewBackend()
	model := lookupModel()
	srv := newSite(t, model, backend)

	// Act
	status, ask, raw := call(t, srv, "POST", "/ask", "alice", `{"message":"where is order A-1042?"}`)
	events, _ := ask["events"].(string)
	_, _, stream := call(t, srv, "GET", events, "alice", "")

	// Assert
	if status != http.StatusAccepted || events == "" {
		t.Fatalf("ask: %d %s", status, raw)
	}
	if !strings.Contains(stream, "shipped 2026-10-20") || !strings.Contains(stream, "RUN_FINISHED") {
		t.Fatalf("the tool result or the end of the run is missing from the stream:\n%s", stream)
	}
	if n := len(backend.Created()); n != 0 {
		t.Fatalf("a question created %d sandboxes", n)
	}
	for _, ti := range model.Calls()[0].Tools {
		if ti.Name == "execute_command" {
			t.Fatal("an off run offered execute_command")
		}
	}
	_, run, _ := call(t, srv, "GET", "/agent/api/runs/"+ask["runId"].(string), "alice", "")
	if run["sandbox"] != "off" {
		t.Fatalf("run %v", run)
	}
}

func TestACodeRequestGetsTheSandboxToolsButNoSandboxUntilACommandRuns(t *testing.T) {
	// Arrange
	backend := sandboxtest.NewBackend()
	model := lookupModel()
	srv := newSite(t, model, backend)

	// Act
	_, ask, _ := call(t, srv, "POST", "/ask", "alice", `{"message":"order A-1042?","code":true}`)
	call(t, srv, "GET", ask["events"].(string), "alice", "")

	// Assert
	var names []string
	for _, ti := range model.Calls()[0].Tools {
		names = append(names, ti.Name)
	}
	if !strings.Contains(strings.Join(names, " "), "execute_command") {
		t.Fatalf("tools %v", names)
	}
	if n := len(backend.Created()); n != 0 {
		t.Fatalf("created %d sandboxes without a command", n)
	}
}

func TestUsersSeeOnlyTheirOwnThreadsAndOrders(t *testing.T) {
	// Arrange
	srv := newSite(t, lookupModel(), nil)
	_, ask, _ := call(t, srv, "POST", "/ask", "alice", `{"message":"hello"}`)
	threadID := ask["threadId"].(string)

	// Act
	aliceStatus, _, aliceList := call(t, srv, "GET", "/my/threads", "alice", "")
	_, _, bobList := call(t, srv, "GET", "/my/threads", "bob", "")
	bobThread, _, _ := call(t, srv, "GET", "/agent/api/threads/"+threadID, "bob", "")
	bobAsk, _, _ := call(t, srv, "POST", "/ask", "bob", `{"message":"hi","threadId":"`+threadID+`"}`)
	anon, _, _ := call(t, srv, "GET", "/my/threads", "", "")
	anonAgent, _, _ := call(t, srv, "GET", "/agent/api/threads", "", "")
	_, _, bobStream := call(t, srv, "GET", ask["events"].(string), "bob", "")

	// Assert
	if aliceStatus != 200 || !strings.Contains(aliceList, threadID) {
		t.Fatalf("alice's threads: %s", aliceList)
	}
	if strings.Contains(bobList, threadID) {
		t.Fatalf("bob lists alice's thread: %s", bobList)
	}
	if bobThread != 404 || bobAsk != 404 {
		t.Fatalf("bob reaches alice's thread: GET %d, POST %d", bobThread, bobAsk)
	}
	if anon != 401 || anonAgent != 401 {
		t.Fatalf("anonymous: %d %d", anon, anonAgent)
	}
	if strings.Contains(bobStream, "RUN_") {
		t.Fatalf("bob reads alice's events: %s", bobStream)
	}
}

func TestTheOrderToolDoesNotLeakAnotherCustomersOrder(t *testing.T) {
	// Arrange: bob asks for A-1042, which is alice's
	srv := newSite(t, lookupModel(), nil)

	// Act
	_, ask, _ := call(t, srv, "POST", "/ask", "bob", `{"message":"order A-1042?"}`)
	_, _, stream := call(t, srv, "GET", ask["events"].(string), "bob", "")

	// Assert
	if strings.Contains(stream, "shipped") || !strings.Contains(stream, "no such order") {
		t.Fatalf("stream:\n%s", stream)
	}
}
