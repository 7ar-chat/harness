package httpserver_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness"
	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/hosts/web"
	"github.com/GhaziBenDahmane/harness/sandbox/sandboxtest"
)

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	h, err := harness.New(harness.Options{
		Config:  cfg,
		Models:  llmtest.Factory{M: llmtest.New(llmtest.Turn{Chunks: llmtest.Text("Hi there")}, llmtest.Turn{Chunks: llmtest.Text("Title")})},
		Backend: sandboxtest.NewBackend(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = h.Run(ctx) }()
	srv := httptest.NewServer(httpserver.New(h, httpserver.Options{
		DefaultModel: "gpt-test",
		User: func(r *http.Request) (httpserver.Caller, error) {
			return httpserver.Caller{Login: r.Header.Get("X-User")}, nil
		},
	}))
	t.Cleanup(func() { srv.Close(); cancel() })
	return srv
}

func call(t *testing.T, srv *httptest.Server, method, path, user, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	req.Header.Set("X-User", user)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestRunsAreCreatedAndStreamedAsSSE(t *testing.T) {
	// Arrange
	srv := newServer(t)
	var th struct{ ID string }
	_ = json.NewDecoder(call(t, srv, "POST", "/threads", "alice", `{}`).Body).Decode(&th)

	// Act
	resp := call(t, srv, "POST", "/threads/"+th.ID+"/runs", "alice", `{"message":"hello"}`)
	var run struct{ ID string }
	_ = json.NewDecoder(resp.Body).Decode(&run)
	stream := call(t, srv, "GET", "/runs/"+run.ID+"/events", "alice", "")
	defer stream.Body.Close()
	var ids []string
	var data strings.Builder
	sc := bufio.NewScanner(stream.Body)
	deadline := time.AfterFunc(10*time.Second, func() { stream.Body.Close() })
	defer deadline.Stop()
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "id: "); ok {
			ids = append(ids, v)
		}
		if v, ok := strings.CutPrefix(line, "data: "); ok {
			data.WriteString(v + "\n")
		}
	}

	// Assert
	if resp.StatusCode != 202 || stream.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d type %q", resp.StatusCode, stream.Header.Get("Content-Type"))
	}
	if len(ids) == 0 || ids[0] != "1" || !strings.Contains(data.String(), "RUN_FINISHED") || !strings.Contains(data.String(), "Hi there") {
		t.Fatalf("ids %v data %s", ids, data.String())
	}
}

func TestAnotherUserCannotSeeTheRun(t *testing.T) {
	// Arrange
	srv := newServer(t)
	var th struct{ ID string }
	_ = json.NewDecoder(call(t, srv, "POST", "/threads", "alice", `{}`).Body).Decode(&th)
	var run struct{ ID string }
	_ = json.NewDecoder(call(t, srv, "POST", "/threads/"+th.ID+"/runs", "alice", `{"message":"hello"}`).Body).Decode(&run)

	// Act
	resp := call(t, srv, "GET", "/runs/"+run.ID+"/events", "mallory", "")

	// Assert
	if resp.StatusCode != 404 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestUnauthenticatedRequestsAreRefused(t *testing.T) {
	// Arrange
	srv := newServer(t)

	// Act
	resp := call(t, srv, "GET", "/threads", "", "")

	// Assert
	if resp.StatusCode != 401 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}
