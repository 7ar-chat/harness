// Package clienttest is the conformance suite of client.Client: any
// implementation (in process, over HTTP, a future one) must pass Run, so that a
// front end behaves the same against all of them.
//
// The suite builds its own harness (a scripted model, a fake sandbox) and asks
// the implementation under test for clients of two users over it:
//
//	clienttest.Run(t, func(t *testing.T, h *harness.Harness, user string) client.Client {
//		return client.InProcess(h, user, client.WithModels(clienttest.Models, clienttest.DefaultModel))
//	})
package clienttest

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness"
	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/engine/agui"
	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/hosts/client"
	"github.com/GhaziBenDahmane/harness/sandbox/sandboxtest"
)

// Models and DefaultModel are what the clients under test must report from Models.
var Models = []string{"gpt-test"}

// DefaultModel is the default model of the clients under test.
const DefaultModel = "gpt-test"

// Factory returns a client acting for user over h.
type Factory func(t *testing.T, h *harness.Harness, user string) client.Client

// Run runs the suite.
func Run(t *testing.T, mk Factory) {
	t.Run("models", func(t *testing.T) { testModels(t, mk) })
	t.Run("thread lifecycle", func(t *testing.T) { testThreadLifecycle(t, mk) })
	t.Run("send and stream", func(t *testing.T) { testSendAndStream(t, mk) })
	t.Run("resume the stream from a seq", func(t *testing.T) { testResumeFromAfter(t, mk) })
	t.Run("steer a blocked run", func(t *testing.T) { testSteer(t, mk) })
	t.Run("cancel a blocked run", func(t *testing.T) { testCancel(t, mk) })
	t.Run("ask_user round trip", func(t *testing.T) { testAskUser(t, mk) })
	t.Run("validation", func(t *testing.T) { testValidation(t, mk) })
	t.Run("ownership", func(t *testing.T) { testOwnership(t, mk) })
	t.Run("sessions need a durable store", func(t *testing.T) { testSessions(t, mk) })
	t.Run("approval round trip", func(t *testing.T) { testApproval(t, mk) })
	t.Run("files", func(t *testing.T) { testFiles(t, mk) })
}

type rig struct {
	h     *harness.Harness
	ctx   context.Context
	alice client.Client
	bob   client.Client
}

type rigOpts struct {
	model *llmtest.Model
	local bool // the local sandbox backend instead of the fake one
}

func newRig(t *testing.T, mk Factory, o rigOpts) *rig {
	t.Helper()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", strings.Join(Models, ","))
	t.Setenv("LLM_STEP_RETRIES", "0")
	t.Setenv("AGENT_PERMISSIONS", "")
	t.Setenv("AGENT_PERMISSIONS_DENY", "")
	if o.local {
		t.Setenv("SANDBOX_BACKEND", "local")
		t.Setenv("LOCAL_WORKSPACE", t.TempDir())
		t.Setenv("LOCAL_STATE_DIR", t.TempDir())
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if o.model == nil {
		o.model = llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn { return answer(c, "Hi there") })
	}
	ho := harness.Options{Config: cfg, Models: llmtest.Factory{M: o.model}}
	if !o.local {
		ho.Backend = sandboxtest.NewBackend()
	}
	h, err := harness.New(ho)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	t.Cleanup(cancel)
	go func() { _ = h.Run(ctx) }()
	return &rig{h: h, ctx: ctx, alice: mk(t, h, "alice"), bob: mk(t, h, "bob")}
}

// answer plays text; calls without tools (the title) get a title.
func answer(c llmtest.Call, text string) llmtest.Turn {
	if len(c.Tools) == 0 {
		return llmtest.Turn{Chunks: llmtest.Text("A title")}
	}
	return llmtest.Turn{Chunks: llmtest.Text(text)}
}

// scripted asks for one tool call, then answers "done" once it has seen the tool result.
func scripted(tool, args string) *llmtest.Model {
	return llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if len(c.Tools) == 0 {
			return llmtest.Turn{Chunks: llmtest.Text("A title")}
		}
		if last := c.Messages[len(c.Messages)-1]; last.Role == schema.Tool {
			return llmtest.Turn{Chunks: llmtest.Text("done")}
		}
		return llmtest.Turn{Chunks: []*schema.Message{llmtest.Finish(llmtest.ToolCall(0, "c1", tool, args), "tool_calls")}}
	})
}

// blocking is a model whose first agent step waits for release; started is closed when it begins.
func blocking() (m *llmtest.Model, started chan struct{}, release func()) {
	started, gate := make(chan struct{}), make(chan struct{})
	var once, rel sync.Once
	m = llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if len(c.Tools) == 0 {
			return llmtest.Turn{Chunks: llmtest.Text("A title")}
		}
		once.Do(func() { close(started) })
		<-gate
		return llmtest.Turn{Chunks: llmtest.Text("unblocked")}
	})
	return m, started, func() { rel.Do(func() { close(gate) }) }
}

// collect reads a stream until it closes.
func collect(t *testing.T, ch <-chan client.Event) []client.Event {
	t.Helper()
	var out []client.Event
	timeout := time.After(30 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-timeout:
			t.Fatalf("the event stream did not end; got %d events", len(out))
		}
	}
}

// stream reads the whole stream of a run from after.
func stream(t *testing.T, r *rig, c client.Client, runID string, after int64) []client.Event {
	t.Helper()
	ch, err := c.Events(r.ctx, runID, after)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	return collect(t, ch)
}

// finished decodes the RUN_FINISHED event ending a stream.
func finished(t *testing.T, evs []client.Event) (outcome string, interrupts []agui.Interrupt) {
	t.Helper()
	if len(evs) == 0 || evs[len(evs)-1].Type != agui.RunFinished {
		var types []string
		for _, e := range evs {
			types = append(types, e.Type)
		}
		t.Fatalf("the stream did not end with RUN_FINISHED: %v", types)
	}
	var f struct {
		Outcome struct {
			Type       string           `json:"type"`
			Interrupts []agui.Interrupt `json:"interrupts"`
		} `json:"outcome"`
	}
	if err := json.Unmarshal(evs[len(evs)-1].Data, &f); err != nil {
		t.Fatal(err)
	}
	return f.Outcome.Type, f.Outcome.Interrupts
}

// waitIdle waits until the thread has no active run.
func waitIdle(t *testing.T, r *rig, c client.Client, threadID string) *client.Thread {
	t.Helper()
	for i := 0; i < 400; i++ {
		th, err := c.GetThread(r.ctx, threadID)
		if err != nil {
			t.Fatal(err)
		}
		if th.ActiveRun == nil {
			return th
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the thread stayed busy")
	return nil
}

func newThread(t *testing.T, r *rig, c client.Client) string {
	t.Helper()
	th, err := c.CreateThread(r.ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	return th.ID
}

func mustSend(t *testing.T, r *rig, c client.Client, threadID, text string, o client.SendOptions) *client.Run {
	t.Helper()
	run, err := c.Send(r.ctx, threadID, text, o)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if run.ID == "" || run.ThreadID != threadID {
		t.Fatalf("run %+v", run)
	}
	return run
}
