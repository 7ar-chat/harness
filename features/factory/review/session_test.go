package review_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/7ar-chat/harness/ai/llm"
	"github.com/7ar-chat/harness/features/factory/review"
)

// wire is an OpenAI-compatible endpoint that records the session header of each request and refuses a request
// that has none, the way providers that route by session do.
type wire struct {
	mu       sync.Mutex
	sessions []string
	refused  int
}

func (w *wire) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	id := r.Header.Get("x-test-session")
	if id == "" {
		w.refused++
		http.Error(rw, `{"error":{"message":"Request is missing x-test-session"}}`, http.StatusBadRequest)
		return
	}
	w.sessions = append(w.sessions, id)
	rw.Header().Set("Content-Type", "application/json")
	_, _ = rw.Write([]byte(`{"id":"1","object":"chat.completion","model":"reviewer-1","choices":[{"index":0,"finish_reason":"stop",` +
		`"message":{"role":"assistant","content":"{\"approve\":true,\"summary\":\"ok\",\"findings\":[]}"}}],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
}

func TestReviewCallsCarryTheSessionHeaderOfProvidersThatRouteBySession(t *testing.T) {
	// Arrange: a provider that rejects calls without a session id, and the real OpenAI client talking to it.
	w := &wire{}
	srv := httptest.NewServer(w)
	t.Cleanup(srv.Close)
	models := llm.New(llm.Options{BaseURL: srv.URL, APIKey: "k", Models: []string{"reviewer-1"}, SessionHeader: "x-test-session"})
	f := &forgeFake{diff: sourceAndTest()}
	gate := review.New(review.Options{Forge: f, Models: models, Model: "reviewer-1", RequireTests: true})

	// Act: no agent run is on the context, as when the pull request loop judges a PR.
	v, err := gate.Judge(context.Background(), task(), pr, nil)

	// Assert
	if err != nil || !v.Approve {
		t.Fatalf("verdict = %+v, err = %v; want an approval", v, err)
	}
	if w.refused != 0 || len(w.sessions) != 1 || w.sessions[0] != "review:t1" {
		t.Errorf("refused = %d, sessions = %v; want one call under the task's review session", w.refused, w.sessions)
	}
}
