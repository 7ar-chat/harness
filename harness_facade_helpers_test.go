package harness_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/compaction"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/core/store"
)

// buildFacade builds a harness on the minimal profile and the in-memory sandbox backend; mod adjusts the options.
// The runtime is not started: see startFacade.
func buildFacade(t *testing.T, env map[string]string, model *llmtest.Model, mod func(*harness.Options)) *harness.Harness {
	t.Helper()
	o := harness.Options{
		Config: featureConfig(t, env), Models: llmtest.Factory{M: model},
		Backend: sandboxtest.NewBackend(), Features: harness.ProfileMinimal(),
	}
	if mod != nil {
		mod(&o)
	}
	h, err := harness.New(o)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// startFacade runs the harness' runtime until the test ends and returns the context it lives in.
func startFacade(t *testing.T, h *harness.Harness) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	done := make(chan struct{})
	go func() { _ = h.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return ctx
}

// facade is buildFacade followed by startFacade.
func facade(t *testing.T, env map[string]string, model *llmtest.Model, mod func(*harness.Options)) (*harness.Harness, context.Context) {
	t.Helper()
	h := buildFacade(t, env, model, mod)
	return h, startFacade(t, h)
}

// lastUserText is the content of the last user message of a model call.
func lastUserText(c llmtest.Call) string {
	for i := len(c.Messages) - 1; i >= 0; i-- {
		if c.Messages[i].Role == ai.RoleUser {
			return c.Messages[i].Content
		}
	}
	return ""
}

// echoModel answers "re: <last user text>" and summarises with "SUMMARY" when asked to compact.
func echoModel() *llmtest.Model {
	return llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if len(c.Messages) > 0 && c.Messages[0].Content == compaction.Instructions {
			return llmtest.Turn{Chunks: llmtest.Text("SUMMARY of the conversation")}
		}
		return llmtest.Turn{Chunks: llmtest.Text("re: " + lastUserText(c))}
	})
}

// exchange sends text as user and waits for the run's terminal event.
func exchange(ctx context.Context, t *testing.T, h *harness.Harness, user, threadID, text string, o harness.RunOptions) *store.Run {
	t.Helper()
	if o.Model == "" {
		o.Model = "gpt-test"
	}
	run, err := h.Send(ctx, user, threadID, text, o)
	if err != nil {
		t.Fatal(err)
	}
	waitRun(ctx, t, h, run)
	return run
}

// waitRun drains the run's events: the channel closes after the terminal one.
func waitRun(ctx context.Context, t *testing.T, h *harness.Harness, run *store.Run) []store.Event {
	t.Helper()
	var evs []store.Event
	for ev := range h.Events(ctx, run.ID, 0) {
		evs = append(evs, ev)
	}
	if len(evs) == 0 {
		t.Fatalf("run %s produced no events (context: %v)", run.ID, ctx.Err())
	}
	return evs
}

func contents(msgs []*store.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Content
	}
	return out
}

func messagesOf(ctx context.Context, t *testing.T, h *harness.Harness, threadID string) []*store.Message {
	t.Helper()
	msgs, err := h.Store.ListMessages(ctx, threadID)
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

// eventually polls cond until it holds or the context ends.
func eventually(ctx context.Context, t *testing.T, what string, cond func() bool) {
	t.Helper()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", what)
		case <-tick.C:
		}
	}
}

// gitRepo makes a repository with one commit (a.txt = "v1") and returns its real path.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	writeFile(t, dir, "a.txt", "v1\n")
	git("add", "-A")
	git("commit", "-q", "-m", "init")
	return dir
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(dir, name string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	return strings.TrimSpace(string(b)), err == nil
}
