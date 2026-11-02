package runtime_test

import (
	"encoding/json"
	"fmt"
	"testing"
)

// checkStrictOrder fails the test when the events break the strict AG-UI order
// (stricter than agui.CheckOrder, which rig_test.go applies to every run log):
// RUN_STARTED first, the terminal event last; every STEP/TEXT/TOOL_CALL/REASONING
// START closed by its END; content/args only inside their open message or call;
// no tool call starts while a text message or another tool call is open; every
// TOOL_CALL_RESULT of a call started in this log follows its TOOL_CALL_END (a
// resumed run answers calls started by the run it resumes).
func checkStrictOrder(t *testing.T, evs []event) {
	t.Helper()
	if err := orderError(evs); err != nil {
		t.Fatalf("AG-UI order: %v\n%v", err, types(evs))
	}
}

func orderError(evs []event) error {
	if len(evs) == 0 || evs[0]["type"] != "RUN_STARTED" {
		return fmt.Errorf("the first event is not RUN_STARTED")
	}
	str := func(e event, k string) string { s, _ := e[k].(string); return s }
	openText, openCall, openStep, openReasoning := "", "", "", ""
	started, ended := map[string]bool{}, map[string]bool{}
	for i, e := range evs {
		typ := str(e, "type")
		switch typ {
		case "STEP_STARTED":
			if openStep != "" {
				return fmt.Errorf("event %d: step %s starts inside %s", i, str(e, "stepName"), openStep)
			}
			openStep = str(e, "stepName")
		case "STEP_FINISHED":
			if openStep != str(e, "stepName") {
				return fmt.Errorf("event %d: step %s finishes but %q is open", i, str(e, "stepName"), openStep)
			}
			if openText != "" || openCall != "" || openReasoning != "" {
				return fmt.Errorf("event %d: step finishes with open text %q call %q reasoning %q", i, openText, openCall, openReasoning)
			}
			openStep = ""
		case "REASONING_START":
			openReasoning = str(e, "messageId")
		case "REASONING_END":
			openReasoning = ""
		case "TEXT_MESSAGE_START":
			if openText != "" || openCall != "" {
				return fmt.Errorf("event %d: text starts while text %q / call %q is open", i, openText, openCall)
			}
			openText = str(e, "messageId")
		case "TEXT_MESSAGE_CONTENT":
			if openText == "" || openText != str(e, "messageId") {
				return fmt.Errorf("event %d: text content outside its message", i)
			}
		case "TEXT_MESSAGE_END":
			if openText != str(e, "messageId") {
				return fmt.Errorf("event %d: text end without start", i)
			}
			openText = ""
		case "TOOL_CALL_START":
			if openText != "" || openCall != "" {
				return fmt.Errorf("event %d: tool call %s starts while text %q / call %q is open", i, str(e, "toolCallId"), openText, openCall)
			}
			openCall = str(e, "toolCallId")
			started[openCall] = true
		case "TOOL_CALL_ARGS":
			if openCall != str(e, "toolCallId") {
				return fmt.Errorf("event %d: args of %s outside the call", i, str(e, "toolCallId"))
			}
		case "TOOL_CALL_END":
			if openCall != str(e, "toolCallId") {
				return fmt.Errorf("event %d: end of %s without start", i, str(e, "toolCallId"))
			}
			ended[openCall] = true
			openCall = ""
		case "TOOL_CALL_RESULT":
			if openCall != "" || openText != "" {
				return fmt.Errorf("event %d: tool result while call %q / text %q is open", i, openCall, openText)
			}
			if id := str(e, "toolCallId"); started[id] && !ended[id] {
				return fmt.Errorf("event %d: result of %s before its end", i, id)
			}
		case "RUN_FINISHED", "RUN_ERROR":
			if i != len(evs)-1 {
				return fmt.Errorf("event %d: terminal event is not last", i)
			}
			if openText != "" || openCall != "" || openStep != "" || openReasoning != "" {
				return fmt.Errorf("terminal event with open text %q call %q step %q reasoning %q", openText, openCall, openStep, openReasoning)
			}
			return nil
		}
	}
	return fmt.Errorf("no terminal event")
}

// strictOrder checks every run log the test produced against the strict AG-UI
// order when the test ends (rigs of the verification, approval and Responses
// tests).
func (r *rig) strictOrder() {
	r.t.Helper()
	t := r.t
	t.Cleanup(func() {
		for runID, logged := range r.st.EventLogs() {
			if len(logged) == 0 {
				continue
			}
			evs := make([]event, 0, len(logged))
			for _, e := range logged {
				var m event
				_ = json.Unmarshal(e.Data, &m)
				evs = append(evs, m)
			}
			if err := orderError(evs); err != nil {
				t.Errorf("run %s: strict AG-UI order: %v\n%v", runID, err, types(evs))
			}
		}
	})
}
