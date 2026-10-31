package agui_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/agui"
)

func ev(typ string, kv ...string) agui.Event {
	e := agui.Event{"type": typ}
	for i := 0; i+1 < len(kv); i += 2 {
		e[kv[i]] = kv[i+1]
	}
	return e
}

func TestCheckOrderAcceptsAWellFormedRun(t *testing.T) {
	// A run with paired steps, text, reasoning and tool calls ending in RUN_FINISHED is valid.
	// Arrange
	evs := []agui.Event{
		ev(agui.RunStarted), ev(agui.StepStarted, "stepName", "s1"),
		ev(agui.ReasoningStart, "messageId", "r"), ev(agui.ReasoningMessageStart, "messageId", "r"),
		ev(agui.ReasoningMessageContent, "messageId", "r"), ev(agui.ReasoningMessageEnd, "messageId", "r"), ev(agui.ReasoningEnd, "messageId", "r"),
		ev(agui.TextMessageStart, "messageId", "m"), ev(agui.TextMessageContent, "messageId", "m"),
		ev(agui.ToolCallStart, "toolCallId", "c"), ev(agui.ToolCallArgs, "toolCallId", "c"), ev(agui.ToolCallEnd, "toolCallId", "c"),
		ev(agui.TextMessageEnd, "messageId", "m"), ev(agui.ToolCallResult, "toolCallId", "c"),
		ev(agui.StepFinished, "stepName", "s1"), ev(agui.Custom, "name", "work.usage"), ev(agui.RunFinished),
	}

	// Act
	err := agui.CheckOrder(evs)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
}

func TestCheckOrderRejectsViolations(t *testing.T) {
	// Each START/END or terminal-order violation is reported with the offending index.
	// Arrange
	cases := map[string]struct {
		evs  []agui.Event
		want string
	}{
		"first not RUN_STARTED":  {[]agui.Event{ev(agui.RunError)}, "first event"},
		"empty log":              {nil, "empty"},
		"second RUN_STARTED":     {[]agui.Event{ev(agui.RunStarted), ev(agui.RunStarted)}, "RUN_STARTED"},
		"event after terminal":   {[]agui.Event{ev(agui.RunStarted), ev(agui.RunFinished), ev(agui.Custom, "name", "x")}, "after the terminal"},
		"two terminals":          {[]agui.Event{ev(agui.RunStarted), ev(agui.RunFinished), ev(agui.RunError)}, "after the terminal"},
		"content without start":  {[]agui.Event{ev(agui.RunStarted), ev(agui.TextMessageContent, "messageId", "m")}, "not started"},
		"end without start":      {[]agui.Event{ev(agui.RunStarted), ev(agui.ToolCallEnd, "toolCallId", "c")}, "not started"},
		"double start":           {[]agui.Event{ev(agui.RunStarted), ev(agui.ToolCallStart, "toolCallId", "c"), ev(agui.ToolCallStart, "toolCallId", "c")}, "already started"},
		"finished while open":    {[]agui.Event{ev(agui.RunStarted), ev(agui.TextMessageStart, "messageId", "m"), ev(agui.RunFinished)}, "still open"},
		"step finished not open": {[]agui.Event{ev(agui.RunStarted), ev(agui.StepFinished, "stepName", "s")}, "not started"},
		"args after end":         {[]agui.Event{ev(agui.RunStarted), ev(agui.ToolCallStart, "toolCallId", "c"), ev(agui.ToolCallEnd, "toolCallId", "c"), ev(agui.ToolCallArgs, "toolCallId", "c")}, "not started"},
	}

	for name, tc := range cases {
		// Act
		err := agui.CheckOrder(tc.evs)

		// Assert
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error containing %q", name, err, tc.want)
		}
	}
}

func TestCheckOrderJSONAcceptsAnInProgressLogAndRawEvents(t *testing.T) {
	// A log without its terminal event yet (run in progress) is valid when what it has is ordered.
	// Arrange
	var raw []json.RawMessage
	for _, e := range []agui.Event{ev(agui.RunStarted), ev(agui.TextMessageStart, "messageId", "m")} {
		b, _ := json.Marshal(e)
		raw = append(raw, b)
	}

	// Act
	err := agui.CheckOrderJSON(raw)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
}

func TestCheckOrderJSONReportsTheOrderViolationOfValidJSON(t *testing.T) {
	// Arrange
	var raw []json.RawMessage
	for _, e := range []agui.Event{ev(agui.RunStarted), ev(agui.RunFinished), ev(agui.Custom, "name", "late")} {
		b, _ := json.Marshal(e)
		raw = append(raw, b)
	}

	// Act
	err := agui.CheckOrderJSON(raw)

	// Assert
	if err == nil || !strings.Contains(err.Error(), "after the terminal") {
		t.Fatalf("err = %v", err)
	}
}
