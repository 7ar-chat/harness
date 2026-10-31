//go:build unix

package harness_test

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness"
	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/ai/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/core/agui"
	"github.com/GhaziBenDahmane/harness/core/store"
)

// permRig is a harness on the local backend with a scripted model: the model
// asks for one tool call, then answers "done" once it has seen a tool result.
type permRig struct {
	t     *testing.T
	h     *harness.Harness
	ws    string
	model *llmtest.Model
	ctx   context.Context
	th    *store.Thread
}

func newPermRig(t *testing.T, env map[string]string, toolName, args string) *permRig {
	t.Helper()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ws := t.TempDir()
	t.Setenv("HOME", t.TempDir()) // no user-level permissions file
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	t.Setenv("SANDBOX_BACKEND", "local")
	t.Setenv("LOCAL_WORKSPACE", ws)
	t.Setenv("LOCAL_STATE_DIR", t.TempDir())
	t.Setenv("AGENT_PERMISSIONS", "")
	t.Setenv("AGENT_PERMISSIONS_DENY", "")
	for k, v := range env {
		t.Setenv(k, v)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if len(c.Tools) == 0 { // title and other utility calls
			return llmtest.Turn{Chunks: llmtest.Text("A title")}
		}
		if last := c.Messages[len(c.Messages)-1]; last.Role == ai.RoleTool {
			return llmtest.Turn{Chunks: llmtest.Text("done")}
		}
		return llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, "c1", toolName, args), "tool_calls")}}
	})
	h, err := harness.New(harness.Options{Config: cfg, Models: llmtest.Factory{M: model}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	go func() { _ = h.Run(ctx) }()
	th, err := h.NewThread(ctx, "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	return &permRig{t: t, h: h, ws: ws, model: model, ctx: ctx, th: th}
}

// finish waits for the terminal event of a run and returns the events.
func (r *permRig) finish(run *store.Run) []store.Event {
	r.t.Helper()
	var evs []store.Event
	for ev := range r.h.Events(r.ctx, run.ID, 0) {
		evs = append(evs, ev)
	}
	if len(evs) == 0 || evs[len(evs)-1].Type != agui.RunFinished {
		r.t.Fatalf("run did not finish: %d events", len(evs))
	}
	return evs
}

func (r *permRig) send(text string) []store.Event {
	r.t.Helper()
	run, err := r.h.Send(r.ctx, "alice", r.th.ID, text, harness.RunOptions{Model: "gpt-test"})
	if err != nil {
		r.t.Fatal(err)
	}
	return r.finish(run)
}

// toolResults are the contents of the tool messages the model was shown in its last call.
func (r *permRig) toolResults() []string {
	calls := r.model.Calls()
	for i := len(calls) - 1; i >= 0; i-- {
		var out []string
		for _, m := range calls[i].Messages {
			if m.Role == ai.RoleTool {
				out = append(out, m.Content)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

func (r *permRig) exists(name string) bool {
	_, err := os.Stat(filepath.Join(r.ws, name))
	return err == nil
}

func (r *permRig) pending() *store.Approval {
	r.t.Helper()
	th, err := r.h.Store.GetThread(r.ctx, "alice", r.th.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	return th.PendingApproval
}

func (r *permRig) decide(a *store.Approval, decision string) []store.Event {
	r.t.Helper()
	run, _, err := r.h.Store.CreateRun(r.ctx, store.NewRun{RunID: uuid.NewString(), ThreadID: r.th.ID, UserLogin: "alice", Model: "gpt-test",
		Resume: &store.Resume{InterruptID: a.ID, Decision: decision}})
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.h.Queue.Enqueue(r.ctx, run.ID); err != nil {
		r.t.Fatal(err)
	}
	return r.finish(run)
}

func eventsHave(evs []store.Event, typ string) bool {
	for _, e := range evs {
		if e.Type == typ {
			return true
		}
	}
	return false
}

func hasApprovalEvent(evs []store.Event, phase string) bool {
	for _, e := range evs {
		if e.Type != agui.Custom {
			continue
		}
		var m struct {
			Name  string         `json:"name"`
			Value map[string]any `json:"value"`
		}
		if json.Unmarshal(e.Data, &m) == nil && m.Name == "work.approval" && m.Value["phase"] == phase {
			return true
		}
	}
	return false
}

func TestLocalPermissionsDeniedCommandDoesNotRunAndModelSeesTheError(t *testing.T) {
	// Arrange: a user deny rule on a command that is otherwise allowed.
	r := newPermRig(t, map[string]string{"AGENT_PERMISSIONS_DENY": "touch forbidden*"}, "execute_command", `{"command":"touch forbidden.txt"}`)

	// Act
	evs := r.send("make the file")

	// Assert
	if r.exists("forbidden.txt") {
		t.Fatal("the denied command ran")
	}
	if hasApprovalEvent(evs, "required") || r.pending() != nil {
		t.Fatal("a denied command must not ask for approval")
	}
	res := r.toolResults()
	if len(res) != 1 || !strings.Contains(res[0], "permission denied") || !strings.Contains(res[0], "touch forbidden*") || !strings.Contains(res[0], "was not run") {
		t.Fatalf("the model saw %q", res)
	}
}

func TestLocalPermissionsProtectedPathWriteIsDenied(t *testing.T) {
	r := newPermRig(t, nil, "write_file", `{"path":"/workspace/.env","content":"SECRET=1\n"}`)

	r.send("write the env file")

	if r.exists(".env") {
		t.Fatal(".env was written")
	}
	if res := r.toolResults(); len(res) != 1 || !strings.Contains(res[0], "permission denied") || !strings.Contains(res[0], "secret") {
		t.Fatalf("the model saw %q", res)
	}
}

func TestLocalPermissionsAskPausesThenApprovalRunsTheCommand(t *testing.T) {
	// Arrange: `bash mk.sh` is not a known-safe command, so it is asked.
	r := newPermRig(t, nil, "execute_command", `{"command":"bash mk.sh"}`)
	if err := os.WriteFile(filepath.Join(r.ws, "mk.sh"), []byte("echo ran > ran.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Act 1: the run pauses with an approval interrupt and nothing ran.
	evs := r.send("run the script")
	a := r.pending()

	// Assert 1
	if a == nil || a.ToolName != "execute_command" || !strings.Contains(a.ToolInputPreview, "bash mk.sh") {
		t.Fatalf("pending approval = %+v", a)
	}
	if !hasApprovalEvent(evs, "required") {
		t.Fatal("no work.approval required event")
	}
	if r.exists("ran.txt") {
		t.Fatal("the command ran before the approval")
	}

	// Act 2: approve.
	resumed := r.decide(a, store.DecisionApprove)

	// Assert 2: it ran, and the model got its result.
	if !r.exists("ran.txt") {
		t.Fatal("the approved command did not run")
	}
	if !hasApprovalEvent(resumed, "resolved") || !eventsHave(resumed, agui.ToolCallResult) {
		t.Fatalf("resumed run: approval resolved %v, tool result %v", hasApprovalEvent(resumed, "resolved"), eventsHave(resumed, agui.ToolCallResult))
	}
	if r.pending() != nil {
		t.Fatal("the approval is still pending")
	}
}

func TestLocalPermissionsDenyingTheApprovalKeepsTheCommandFromRunning(t *testing.T) {
	r := newPermRig(t, nil, "execute_command", `{"command":"bash mk.sh"}`)
	if err := os.WriteFile(filepath.Join(r.ws, "mk.sh"), []byte("echo ran > ran.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.send("run the script")

	r.decide(r.pending(), store.DecisionDeny)

	if r.exists("ran.txt") {
		t.Fatal("the denied approval still ran the command")
	}
}

func TestLocalPermissionsSafeCommandsRunWithoutAsking(t *testing.T) {
	r := newPermRig(t, nil, "execute_command", `{"command":"echo hi > greeting.txt && ls && git status"}`)

	evs := r.send("greet")

	if hasApprovalEvent(evs, "required") || !r.exists("greeting.txt") {
		t.Fatal("a safe command was asked about or did not run")
	}
}

func TestLocalPermissionsYoloRunsAskedCommandsButKeepsDenyRules(t *testing.T) {
	// yolo: the command that would be asked runs at once.
	r := newPermRig(t, map[string]string{"AGENT_PERMISSIONS": "yolo"}, "execute_command", `{"command":"bash mk.sh"}`)
	if err := os.WriteFile(filepath.Join(r.ws, "mk.sh"), []byte("echo ran > ran.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	evs := r.send("run the script")
	if hasApprovalEvent(evs, "required") || !r.exists("ran.txt") {
		t.Fatal("yolo mode asked or did not run the command")
	}

	// yolo: deny rules still apply.
	d := newPermRig(t, map[string]string{"AGENT_PERMISSIONS": "yolo"}, "write_file", `{"path":".git/config","content":"x"}`)
	d.send("edit git internals")
	if d.exists(".git/config") {
		t.Fatal(".git/config was written in yolo mode")
	}
	if res := d.toolResults(); len(res) != 1 || !strings.Contains(res[0], "permission denied") {
		t.Fatalf("the model saw %q", res)
	}
}

func TestLocalPermissionsReadOnlyRefusesWrites(t *testing.T) {
	r := newPermRig(t, map[string]string{"AGENT_PERMISSIONS": "readonly"}, "write_file", `{"path":"a.txt","content":"x"}`)

	r.send("write")

	if r.exists("a.txt") {
		t.Fatal("a file was written in readonly mode")
	}
	if res := r.toolResults(); len(res) != 1 || !strings.Contains(res[0], "readonly mode") {
		t.Fatalf("the model saw %q", res)
	}
}

func TestLocalPermissionsRejectAnUnknownMode(t *testing.T) {
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	t.Setenv("SANDBOX_BACKEND", "local")
	t.Setenv("LOCAL_WORKSPACE", t.TempDir())
	t.Setenv("LOCAL_STATE_DIR", t.TempDir())
	t.Setenv("AGENT_PERMISSIONS", "reckless")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := harness.New(harness.Options{Config: cfg, Models: llmtest.Factory{M: llmtest.New()}}); err == nil {
		t.Fatal("harness.New accepted an unknown AGENT_PERMISSIONS")
	}
}
