package tui

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/hosts/client"
)

func runPrint(t *testing.T, be Backend, o PrintOptions) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	o.Stdout, o.Stderr = &out, &errb
	code = RunPrint(context.Background(), be, "go", o)
	return code, out.String(), errb.String()
}

func msgDelta(id, d string) agui.Event {
	return agui.New(agui.TextMessageContent, map[string]any{"messageId": id, "delta": d})
}

func TestPrintStreamsAnswersToStdoutAndToolsToStderr(t *testing.T) {
	// Arrange
	evs := []agui.Event{
		agui.New(agui.TextMessageStart, map[string]any{"messageId": "m1"}),
		msgDelta("m1", "Looking "), msgDelta("m1", "now."),
		agui.New(agui.TextMessageEnd, map[string]any{"messageId": "m1"}),
	}
	evs = append(evs, toolEvents("c1", "read_file", `{"path":"a.go"}`, "package a\nfunc A()", false)...)
	evs = append(evs, toolEvents("c2", "run_command", `{"command":"false"}`, "exit 1", true)...)
	evs = append(evs,
		agui.SteerEvent("s1", "also check b.go", true),
		agui.New(agui.TextMessageStart, map[string]any{"messageId": "m2"}),
		msgDelta("m2", "Done."),
		agui.New(agui.TextMessageEnd, map[string]any{"messageId": "m2"}),
		agui.RunFinishedEvent("t", "r", "success", nil))

	// Act
	code, stdout, stderr := runPrint(t, &replayBackend{events: evs}, PrintOptions{})

	// Assert
	if code != ExitOK {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if stdout != "Looking now.\n\nDone.\n" {
		t.Fatalf("stdout %q", stdout)
	}
	for _, want := range []string{"> read_file a.go\n", "  ok: package a func A()\n", "> run_command false\n", "  error: exit 1\n", "  steer: also check b.go\n"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
}

func TestPrintExitStatusFollowsTheTerminalEvent(t *testing.T) {
	interrupt := agui.RunInterruptedEvent("t", "r", []agui.Interrupt{
		{ID: "i1", Reason: client.ReasonToolApproval, Message: "run_command"},
		{ID: "i2", Reason: "ask_user", Message: "Which database?"},
	}, nil)
	cases := map[string]struct {
		events []agui.Event
		code   int
		stderr []string
	}{
		"success":   {[]agui.Event{agui.RunFinishedEvent("t", "r", "success", nil)}, ExitOK, nil},
		"cancelled": {[]agui.Event{agui.RunFinishedEvent("t", "r", "cancelled", nil)}, ExitFailed, []string{"harness: run cancelled"}},
		"run error": {[]agui.Event{agui.RunErrorEvent("boom", "the model failed")}, ExitFailed, []string{"harness: run failed: the model failed"}},
		"interrupt": {[]agui.Event{interrupt}, ExitInterrupted, []string{"waiting for input (tool_approval): run_command", "waiting for input (ask_user): Which database?"}},
		"no result": {[]agui.Event{msgDelta("m", "hi")}, ExitFailed, []string{"the event stream ended without a result"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			code, _, stderr := runPrint(t, &replayBackend{events: tc.events}, PrintOptions{})

			if code != tc.code {
				t.Fatalf("exit %d, want %d (stderr %q)", code, tc.code, stderr)
			}
			for _, want := range tc.stderr {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr lacks %q:\n%s", want, stderr)
				}
			}
		})
	}
}

func TestPrintStopsReadingAtTheTerminalEvent(t *testing.T) {
	evs := []agui.Event{agui.RunFinishedEvent("t", "r", "success", nil), msgDelta("m", "after the end")}

	code, stdout, _ := runPrint(t, &replayBackend{events: evs}, PrintOptions{})

	if code != ExitOK || stdout != "" {
		t.Fatalf("exit %d stdout %q", code, stdout)
	}
}

type failingSend struct{ fakeBackend }

func (*failingSend) Send(context.Context, string) (string, error) {
	return "", errors.New("queue full")
}

func TestPrintReportsARunThatCannotStart(t *testing.T) {
	code, stdout, stderr := runPrint(t, &failingSend{}, PrintOptions{})

	if code != ExitFailed || stdout != "" || !strings.Contains(stderr, "harness: send: queue full") {
		t.Fatalf("exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestPrintJSONWritesOnlyTheEventsToStdout(t *testing.T) {
	evs := []agui.Event{msgDelta("m", "hi"), agui.RunFinishedEvent("t", "r", "success", nil)}

	code, stdout, stderr := runPrint(t, &replayBackend{events: evs}, PrintOptions{JSON: true})

	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if code != ExitOK || len(lines) != 2 || !strings.Contains(lines[0], `"delta":"hi"`) || !strings.Contains(lines[1], agui.RunFinished) {
		t.Fatalf("exit %d stdout %q", code, stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr %q: JSON mode must stay quiet on success", stderr)
	}
}

func TestPrintJSONKeepsTheFailureStatus(t *testing.T) {
	code, stdout, stderr := runPrint(t, &replayBackend{events: []agui.Event{agui.RunErrorEvent("c", "bad key")}}, PrintOptions{JSON: true})

	if code != ExitFailed || !strings.Contains(stdout, agui.RunError) || !strings.Contains(stderr, "bad key") {
		t.Fatalf("exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

type cancelBackend struct {
	fakeBackend
	cancel context.CancelFunc
}

func (c *cancelBackend) Events(context.Context, string) <-chan client.Event {
	c.cancel()
	ch := make(chan client.Event)
	close(ch)
	return ch
}

func TestPrintAbortedByTheContextIsAFailureWithoutAStreamMessage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var out, errb bytes.Buffer

	code := RunPrint(ctx, &cancelBackend{cancel: cancel}, "go", PrintOptions{Stdout: &out, Stderr: &errb})

	if code != ExitFailed || strings.Contains(errb.String(), "ended without a result") {
		t.Fatalf("exit %d stderr %q", code, errb.String())
	}
}
