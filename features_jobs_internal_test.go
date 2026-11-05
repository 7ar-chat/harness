package harness

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/features/jobhunt"
)

func jobsWorker(t *testing.T, turns int) (*jobWorker, context.Context) {
	t.Helper()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	var ts []llmtest.Turn
	for range turns {
		ts = append(ts, llmtest.Turn{Chunks: llmtest.Text("done")}, llmtest.Turn{Chunks: llmtest.Text("A title")})
	}
	h, err := New(Options{Config: cfg, Models: llmtest.Factory{M: llmtest.New(ts...)}, Backend: sandboxtest.NewBackend()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	go func() { _ = h.Run(ctx) }()

	board := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jobs":[{"id":1,"title":"Go Engineer","absolute_url":"https://boards.greenhouse.io/acme/jobs/1","location":{"name":"Paris"}}]}`))
	}))
	t.Cleanup(board.Close)
	svc := jobhunt.New(t.TempDir())
	svc.Bases = map[string]string{jobhunt.Greenhouse: board.URL}
	return &jobWorker{svc: svc, h: h, model: "gpt-test", now: time.Now}, ctx
}

func TestTheJobWorkerScansAndWakesTheAgentOncePerRoundInTheDaysConversation(t *testing.T) {
	// Arrange
	w, ctx := jobsWorker(t, 3)
	if _, err := w.svc.SetProfile("ann", jobhunt.Profile{Enabled: true, Titles: []string{"go"},
		Companies: []jobhunt.Company{{Name: "Acme", ATS: jobhunt.Greenhouse, Slug: "acme"}}}); err != nil {
		t.Fatal(err)
	}
	// Act
	if err := w.run(ctx); err != nil {
		t.Fatal(err)
	}
	st1, _ := w.svc.RunState("ann")
	// the first run is still active: this round starts nothing and is not an error
	if err := w.one(ctx, "ann"); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if th, _ := w.h.Store.GetThread(ctx, "ann", st1.ThreadID); th != nil && th.ActiveRun == nil {
			break
		}
	}
	if err := w.one(ctx, "ann"); err != nil { // idle again, the new job is still unjudged: another round
		t.Fatal(err)
	}
	st2, _ := w.svc.RunState("ann")
	// Assert
	if st1.Runs != 1 || st2.Runs < 2 || st1.ThreadID == "" || st1.ThreadID != st2.ThreadID {
		t.Fatalf("%+v %+v", st1, st2)
	}
	th, err := w.h.Store.GetThread(ctx, "ann", st1.ThreadID)
	if err != nil || !strings.HasPrefix(th.Title, "Job search ") {
		t.Fatalf("%+v %v", th, err)
	}
	if c, _ := w.svc.Counts("ann"); c[jobhunt.StatusNew] != 1 {
		t.Fatalf("%v", c)
	}
}

func TestTheJobWorkerLeavesAlonePeopleWhoDidNotTurnItOnAndRoundsWithNothingToDo(t *testing.T) {
	w, ctx := jobsWorker(t, 1)
	if _, err := w.svc.SetProfile("bob", jobhunt.Profile{Titles: []string{"go"},
		Companies: []jobhunt.Company{{Name: "Acme", ATS: jobhunt.Greenhouse, Slug: "acme"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.SetProfile("cy", jobhunt.Profile{Enabled: true, Titles: []string{"plumber"},
		Companies: []jobhunt.Company{{Name: "Acme", ATS: jobhunt.Greenhouse, Slug: "acme"}}}); err != nil {
		t.Fatal(err)
	}
	if err := w.run(ctx); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"bob", "cy"} {
		if st, _ := w.svc.RunState(u); st.Runs != 0 {
			t.Errorf("%s: %+v", u, st)
		}
	}
}

func TestTheJobSectionAsksForTheProfileUntilThereIsOne(t *testing.T) {
	svc := jobhunt.New(t.TempDir())
	if s := jobSection(svc, "ann"); !strings.Contains(s, "tell you the roles") {
		t.Fatal(s)
	}
	if _, err := svc.SetProfile("ann", jobhunt.Profile{Titles: []string{"go dev"}, Mode: jobhunt.ModeAuto}); err != nil {
		t.Fatal(err)
	}
	if s := jobSection(svc, "ann"); !strings.Contains(s, "Mode: auto") || !strings.Contains(s, "go dev") {
		t.Fatal(s)
	}
}
