package agui

import (
	"encoding/json"
	"fmt"
)

// pairs maps the START/END event types of AG-UI spans to their id field.
var pairs = map[string]struct{ end, id string }{
	StepStarted:           {StepFinished, "stepName"},
	TextMessageStart:      {TextMessageEnd, "messageId"},
	ToolCallStart:         {ToolCallEnd, "toolCallId"},
	ReasoningStart:        {ReasoningEnd, "messageId"},
	ReasoningMessageStart: {ReasoningMessageEnd, "messageId"},
}

// inside maps the events that belong to an open span to its START type.
var inside = map[string]string{
	TextMessageContent:      TextMessageStart,
	ToolCallArgs:            ToolCallStart,
	ReasoningMessageContent: ReasoningMessageStart,
}

// CheckOrder verifies the AG-UI order of one run's event log, as strict
// clients (AG-UI HttpAgent's verifier) do: RUN_STARTED first and only once;
// at most one terminal event (RUN_FINISHED/RUN_ERROR), and nothing after it;
// every *_START closed by its *_END with the same id (steps, text and
// reasoning messages, tool calls), content/args only inside an open span, and
// nothing open at RUN_FINISHED. A log without a terminal event (run in
// progress) is valid when what it holds is ordered.
func CheckOrder(evs []Event) error {
	if len(evs) == 0 {
		return fmt.Errorf("agui order: empty log")
	}
	open := map[string]string{} // START type + "/" + id -> START type
	terminal := -1
	for i, e := range evs {
		typ := e.Type()
		if terminal >= 0 {
			return fmt.Errorf("agui order: event %d (%s) after the terminal event %d", i, typ, terminal)
		}
		if i == 0 && typ != RunStarted {
			return fmt.Errorf("agui order: first event is %s, want %s", typ, RunStarted)
		}
		id := func(field string) string { s, _ := e[field].(string); return s }
		switch {
		case typ == RunStarted && i > 0:
			return fmt.Errorf("agui order: event %d: second %s", i, RunStarted)
		case IsTerminal(typ):
			if typ == RunFinished && len(open) > 0 {
				return fmt.Errorf("agui order: event %d: %s while spans are still open: %v", i, typ, keys(open))
			}
			terminal = i
		}
		if p, ok := pairs[typ]; ok {
			k := typ + "/" + id(p.id)
			if _, dup := open[k]; dup {
				return fmt.Errorf("agui order: event %d: %s %q already started", i, typ, id(p.id))
			}
			open[k] = typ
			continue
		}
		for start, p := range pairs {
			if p.end == typ {
				k := start + "/" + id(p.id)
				if _, ok := open[k]; !ok {
					return fmt.Errorf("agui order: event %d: %s %q not started", i, typ, id(p.id))
				}
				delete(open, k)
			}
		}
		if start, ok := inside[typ]; ok {
			if _, ok := open[start+"/"+id(pairs[start].id)]; !ok {
				return fmt.Errorf("agui order: event %d: %s for %q not started", i, typ, id(pairs[start].id))
			}
		}
	}
	return nil
}

// CheckOrderJSON is CheckOrder over raw event JSON (store.Event.Data, SSE data).
func CheckOrderJSON(raw []json.RawMessage) error {
	evs := make([]Event, 0, len(raw))
	for i, b := range raw {
		var e Event
		if err := json.Unmarshal(b, &e); err != nil {
			return fmt.Errorf("agui order: event %d is not JSON: %w", i, err)
		}
		evs = append(evs, e)
	}
	return CheckOrder(evs)
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
