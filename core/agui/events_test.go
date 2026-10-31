package agui_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/agui"
)

func TestEventTypeAndTerminals(t *testing.T) {
	// Arrange / Act / Assert
	if got := agui.New(agui.Custom, nil).Type(); got != agui.Custom {
		t.Errorf("Type = %q", got)
	}
	if got := (agui.Event{"type": 5}).Type(); got != "" {
		t.Errorf("a non-string type reads as %q", got)
	}
	for typ, want := range map[string]bool{agui.RunFinished: true, agui.RunError: true, agui.RunStarted: false, agui.Custom: false, "": false} {
		if agui.IsTerminal(typ) != want {
			t.Errorf("IsTerminal(%q) = %v", typ, !want)
		}
	}
}

func TestNewBuildsAnEventOfTheGivenTypeWithItsFields(t *testing.T) {
	// Arrange / Act
	e := agui.New(agui.RunStarted, map[string]any{"threadId": "t", "extra": 1})

	// Assert
	if e.Type() != agui.RunStarted || e["threadId"] != "t" || e["extra"] != 1 {
		t.Fatalf("event = %v", e)
	}
}

func TestRunEventsCarryTheFieldsClientsRead(t *testing.T) {
	// Act
	started := agui.RunStartedEvent("t1", "r1")
	finished := agui.RunFinishedEvent("t1", "r1", "success", nil)
	withUsage := agui.RunFinishedEvent("t1", "r1", "cancelled", []agui.Usage{{Model: "m", InputTokens: 3, CachedInputTokens: 1}})
	failed := agui.RunErrorEvent(agui.CodeModelError, "boom")

	// Assert
	if started.Type() != agui.RunStarted || started["threadId"] != "t1" || started["runId"] != "r1" || started["protocolVersion"] != "1.0" {
		t.Errorf("started = %v", started)
	}
	if finished.Type() != agui.RunFinished || finished["outcome"].(map[string]any)["type"] != "success" {
		t.Errorf("finished = %v", finished)
	}
	b, _ := json.Marshal(finished)
	if !json.Valid(b) || !strings.Contains(string(b), `"usage":[]`) {
		t.Errorf("a run without usage must serialize an empty list, not null: %s", b)
	}
	b, _ = json.Marshal(withUsage)
	if !strings.Contains(string(b), `"cachedInputTokens":1`) || !strings.Contains(string(b), `"inputTokens":3`) {
		t.Errorf("usage json = %s", b)
	}
	if failed.Type() != agui.RunError || failed["code"] != agui.CodeModelError || failed["message"] != "boom" {
		t.Errorf("failed = %v", failed)
	}
}

func TestSteerEventsSayWhetherTheModelGotTheSteer(t *testing.T) {
	// Act
	plain := agui.SteerEvent("s1", "use tabs", true)
	kept := agui.SteerEvent("s2", "later", false)
	by := agui.SteerEventBy("s3", "hi", true, "alice", "supervisor", "bob")
	anonymous := agui.SteerEventBy("s4", "hi", false, "", "", "")

	// Assert
	v := func(e agui.Event) map[string]any { return e["value"].(map[string]any) }
	if plain.Type() != agui.Custom || plain["name"] != agui.CustomSteer || v(plain)["delivered"] != true || v(plain)["text"] != "use tabs" || v(plain)["id"] != "s1" {
		t.Errorf("plain = %v", plain)
	}
	if v(kept)["delivered"] != false {
		t.Errorf("kept = %v", kept)
	}
	if v(by)["author"] != "alice" || v(by)["authorKind"] != "supervisor" || v(by)["onBehalfOf"] != "bob" {
		t.Errorf("by = %v", by)
	}
	for _, k := range []string{"author", "authorKind", "onBehalfOf"} {
		if _, ok := v(anonymous)[k]; ok {
			t.Errorf("empty %s must be omitted, got %v", k, v(anonymous))
		}
	}
}

func TestCustomAndSubagentEvents(t *testing.T) {
	// Arrange
	payload := map[string]any{"toolCallId": "c1", "phase": agui.SubagentStart, "agent": "explore"}

	// Act
	sub := agui.SubagentEvent(payload)
	custom := agui.CustomEvent("work.thing", 42)

	// Assert
	if sub.Type() != agui.Custom || sub["name"] != agui.CustomSubagent || !reflect.DeepEqual(sub["value"], payload) {
		t.Errorf("subagent = %v", sub)
	}
	if custom["name"] != "work.thing" || custom["value"] != 42 {
		t.Errorf("custom = %v", custom)
	}
}

func TestInterruptedRunFinishesWithTheInterruptsAndPassesTheOrderCheck(t *testing.T) {
	// Arrange
	ints := []agui.Interrupt{{ID: "i1", Reason: "tool_approval", Message: "ok?", ToolCallID: "c1", Approval: map[string]any{"id": "a1"}}}

	// Act
	ev := agui.RunInterruptedEvent("t1", "r1", ints, nil)
	snap := agui.StateSnapshotEvent(map[string]any{"plan": []any{}})

	// Assert
	outcome := ev["outcome"].(map[string]any)
	if ev.Type() != agui.RunFinished || outcome["type"] != "interrupt" || !reflect.DeepEqual(outcome["interrupts"], ints) {
		t.Errorf("interrupted = %v", ev)
	}
	if ev["usage"] == nil {
		t.Error("usage missing")
	}
	if snap.Type() != agui.StateSnapshot || snap["snapshot"] == nil {
		t.Errorf("snapshot = %v", snap)
	}
	if err := agui.CheckOrder([]agui.Event{agui.RunStartedEvent("t1", "r1"), ev}); err != nil {
		t.Errorf("a run that ends interrupted is a well-ordered run: %v", err)
	}
}

func TestCheckOrderJSONRejectsAnEventThatIsNotJSON(t *testing.T) {
	// Arrange
	raw := []json.RawMessage{json.RawMessage(`{"type":"RUN_STARTED"}`), json.RawMessage(`{not json`)}

	// Act
	err := agui.CheckOrderJSON(raw)

	// Assert
	if err == nil || !strings.Contains(err.Error(), "event 1 is not JSON") {
		t.Fatalf("err = %v", err)
	}
}
