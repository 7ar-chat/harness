package tui_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness"
	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/core/agui"
	"github.com/GhaziBenDahmane/harness/core/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/hosts/client"
	"github.com/GhaziBenDahmane/harness/hosts/tui"
)

func newBackend(t *testing.T, interactive bool, turns ...llmtest.Turn) (*tui.ClientBackend, context.Context) {
	t.Helper()
	return newBackendModel(t, interactive, llmtest.New(turns...))
}

func newBackendModel(t *testing.T, interactive bool, model *llmtest.Model) (*tui.ClientBackend, context.Context) {
	t.Helper()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	t.Setenv("LLM_STEP_RETRIES", "0")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	h, err := harness.New(harness.Options{Config: cfg, Models: llmtest.Factory{M: model}, Backend: sandboxtest.NewBackend()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	go func() { _ = h.Run(ctx) }()
	th, err := h.NewThread(ctx, "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	return &tui.ClientBackend{C: client.InProcess(h, "alice", client.WithModels(cfg.LLMModels, "gpt-test")), ThreadID: th.ID, ModelID: "gpt-test", Interactive: interactive}, ctx
}

func TestPrintModeStreamsTheAnswerToStdoutAndToolActivityToStderr(t *testing.T) {
	// Arrange
	be, ctx := newBackend(t, false,
		llmtest.Turn{Chunks: []*schema.Message{llmtest.Finish(llmtest.ToolCall(0, "c1", "write_file", `{"path":"out.md","content":"hi"}`), "tool_calls")}},
		llmtest.Turn{Chunks: llmtest.Text("All ", "done")},
	)
	var stdout, stderr bytes.Buffer

	// Act
	code := tui.RunPrint(ctx, be, "write a file", tui.PrintOptions{Stdout: &stdout, Stderr: &stderr})

	// Assert
	if code != tui.ExitOK {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if stdout.String() != "All done\n" {
		t.Fatalf("stdout %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "> write_file out.md") || !strings.Contains(stderr.String(), "ok:") {
		t.Fatalf("stderr %q", stderr.String())
	}
}

func TestPrintModeJSONEmitsOneAGUIEventPerLine(t *testing.T) {
	// Arrange
	be, ctx := newBackend(t, false, llmtest.Turn{Chunks: llmtest.Text("hello")})
	var stdout, stderr bytes.Buffer

	// Act
	code := tui.RunPrint(ctx, be, "hi", tui.PrintOptions{Stdout: &stdout, Stderr: &stderr, JSON: true})

	// Assert
	if code != tui.ExitOK {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	var types []string
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		var e agui.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %q is not JSON: %v", line, err)
		}
		types = append(types, e.Type())
	}
	if types[0] != agui.RunStarted || types[len(types)-1] != agui.RunFinished {
		t.Fatalf("types %v", types)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr %q", stderr.String())
	}
}

func TestPrintModeExitsWithOneWhenTheRunFails(t *testing.T) {
	// Arrange
	be, ctx := newBackend(t, false, llmtest.Turn{Err: errors.New("provider exploded")})
	var stdout, stderr bytes.Buffer

	// Act
	code := tui.RunPrint(ctx, be, "hi", tui.PrintOptions{Stdout: &stdout, Stderr: &stderr})

	// Assert
	if code != tui.ExitFailed || !strings.Contains(stderr.String(), "run failed") {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
}

func TestPrintModeExitsWithOneWhenSendIsRejected(t *testing.T) {
	// Arrange
	be, ctx := newBackend(t, false)
	be.ModelID = "not-allowed"
	var stdout, stderr bytes.Buffer

	// Act
	code := tui.RunPrint(ctx, be, "hi", tui.PrintOptions{Stdout: &stdout, Stderr: &stderr})

	// Assert
	if code != tui.ExitFailed || !strings.Contains(stderr.String(), "send:") {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
}

func askUserTurn() llmtest.Turn {
	return llmtest.Turn{Chunks: []*schema.Message{llmtest.Finish(llmtest.ToolCall(0, "q1", "ask_user", `{"question":"Which database?","options":[{"label":"Postgres"},{"label":"SQLite"}]}`), "tool_calls")}}
}

func TestPrintModeExitsWithTwoWhenTheRunWaitsForTheUser(t *testing.T) {
	// Arrange
	be, ctx := newBackend(t, false, askUserTurn())
	var stdout, stderr bytes.Buffer

	// Act
	code := tui.RunPrint(ctx, be, "set up storage", tui.PrintOptions{Stdout: &stdout, Stderr: &stderr})

	// Assert
	if code != tui.ExitInterrupted || !strings.Contains(stderr.String(), "Which database?") {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
}

func TestClientBackendResumesAnAskUserInterrupt(t *testing.T) {
	// Arrange
	// The first call asks; a call that sees the user's answer in the history
	// answers; anything else (the title hook) gets a title.
	be, ctx := newBackendModel(t, false, llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		for _, m := range c.Messages {
			if m.Role == schema.Tool && strings.Contains(m.Content, "SQLite") {
				return llmtest.Turn{Chunks: llmtest.Text("Using SQLite")}
			}
		}
		if len(c.Tools) > 0 {
			return askUserTurn()
		}
		return llmtest.Turn{Chunks: llmtest.Text("A title")}
	}))
	runID, err := be.Send(ctx, "set up storage")
	if err != nil {
		t.Fatal(err)
	}
	var interrupt agui.Interrupt
	for se := range be.Events(ctx, runID) {
		if ev := tui.Decode(se); ev.Type == agui.RunFinished && len(ev.Interrupts) > 0 {
			interrupt = ev.Interrupts[0]
		}
	}
	if interrupt.ID == "" {
		t.Fatal("the run did not end with an interrupt")
	}

	// Act
	resumed, err := be.Resume(ctx, tui.ResumeRequest{InterruptID: interrupt.ID, Reason: interrupt.Reason, Text: "SQLite"})
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	var outcome string
	for se := range be.Events(ctx, resumed) {
		ev := tui.Decode(se)
		text.WriteString(ev.Delta)
		if ev.Type == agui.RunFinished {
			outcome = ev.Outcome
		}
	}

	// Assert
	if outcome != "success" || !strings.Contains(text.String(), "Using SQLite") {
		t.Fatalf("outcome %q text %q", outcome, text.String())
	}
}
