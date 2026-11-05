package harness_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
)

// callOf runs one message in a thread and returns the model call that answered it.
func callOf(t *testing.T, h *harness.Harness, m *llmtest.Model, ctx context.Context, user, thread string) llmtest.Call {
	t.Helper()
	before := len(m.Calls())
	run, err := h.Send(ctx, user, thread, "hello", harness.RunOptions{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	for range h.Events(ctx, run.ID, 0) {
	}
	calls := m.Calls()
	if len(calls) <= before {
		t.Fatal("the model was not called")
	}
	return calls[before]
}

func TestTheJobSearchConversationIsAModeOfItsOwnNextToCoding(t *testing.T) {
	// Arrange: the deployment runs the coding profile, like production
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	t.Setenv("AGENT_PROFILE", "coding")
	t.Setenv("HARNESS_JOBS_DIR", t.TempDir())
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	model := llmtest.NewFunc(func(llmtest.Call) llmtest.Turn { return llmtest.Turn{Chunks: llmtest.Text("ok")} })
	h, err := harness.New(harness.Options{Config: cfg, Models: llmtest.Factory{M: model}, Backend: sandboxtest.NewBackend(),
		Features: harness.ProfileNamed("coding")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	go func() { _ = h.Run(ctx) }()

	// Act
	jobThread, err := h.JobThread(ctx, "ann")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := h.JobThread(ctx, "ann")
	other, err := h.NewThread(ctx, "ann", "a coding task")
	if err != nil {
		t.Fatal(err)
	}
	inJobs := callOf(t, h, model, ctx, "ann", jobThread)
	inCoding := callOf(t, h, model, ctx, "ann", other.ID)

	// Assert
	system := func(c llmtest.Call) string { return c.Messages[0].Content }
	names := func(c llmtest.Call) string {
		var n []string
		for _, ti := range c.Tools {
			n = append(n, ti.Name)
		}
		return " " + strings.Join(n, " ") + " "
	}
	if again != jobThread {
		t.Fatalf("a second JobThread made another conversation: %s %s", again, jobThread)
	}
	if !strings.Contains(system(inJobs), "job-search agent") || strings.Contains(system(inJobs), "You are a coding agent") {
		t.Errorf("job thread prompt:\n%.400s", system(inJobs))
	}
	if !strings.Contains(names(inJobs), " jobs_claim ") || strings.Contains(names(inJobs), " explore ") {
		t.Errorf("job thread tools:%s", names(inJobs))
	}
	if !strings.Contains(system(inCoding), "You are a coding agent") || strings.Contains(system(inCoding), "job-search agent") {
		t.Errorf("coding thread prompt:\n%.400s", system(inCoding))
	}
	if strings.Contains(names(inCoding), " jobs_") {
		t.Errorf("coding thread tools:%s", names(inCoding))
	}
	if !strings.Contains(system(inCoding), "Jobs page") {
		t.Errorf("the coding thread is not told where the job search lives")
	}
}
