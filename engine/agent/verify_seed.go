package agent

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/core/store"
)

// seedVerification rebuilds the turn's verification state from its stored
// messages (a resumed run continues the turn of an earlier one), in the order
// the run recorded it: per tool message the checks the model ran, its file
// changes, the runtime's checks, the files left to check. A view_file counts
// when a model call followed it (its pages were sent).
func (st *runState) seedVerification(turn []*store.Message) {
	v := &st.v3.verify
	calls := map[string]ai.ToolCall{}
	for i, m := range turn {
		switch m.Role {
		case store.RoleAssistant:
			for _, c := range m.ToolCalls {
				calls[c.ID] = ai.ToolCall{ID: c.ID, Function: ai.FunctionCall{Name: c.Function.Name, Arguments: c.Function.Arguments}}
			}
		case store.RoleTool:
			items, _ := m.Metadata["verification"].([]any)
			replay := func(modelRun bool) {
				for _, it := range items {
					r, _ := it.(map[string]any)
					p, _ := r["path"].(string)
					ok, _ := r["ok"].(bool)
					failed, _ := r["failed"].(bool)
					name, _ := r["checker"].(string)
					summary, _ := r["summary"].(string)
					next, _ := r["nextStep"].(string)
					mr, _ := r["modelRun"].(bool)
					if p != "" && mr == modelRun {
						v.result(CheckResult{Path: p, Checker: name, OK: ok, Failed: failed, Summary: summary, NextStep: next}, !modelRun)
					}
				}
			}
			replay(true)
			_, all := changedOutputs(m.Metadata)
			for _, c := range all {
				v.changed(c[0], c[1])
			}
			replay(false)
			pending, _ := m.Metadata["verificationPending"].([]any)
			for _, it := range pending {
				if p, _ := it.(string); p != "" {
					if d := v.files[p]; d != nil && d.checked < d.changed {
						d.pending, d.origin = true, m.ToolCallID
					}
				}
			}
			if tc, ok := calls[m.ToolCallID]; ok && tc.Function.Name == "view_file" && m.Error != "" {
				st.noteViewFailed(tc.Function.Arguments, m.Error) // A5: replayed for a resumed run
			}
			if tc, ok := calls[m.ToolCallID]; ok && tc.Function.Name == "view_file" && m.Error == "" && answeredAfter(turn[i+1:]) {
				var args struct {
					Path string `json:"path"`
				}
				if json.Unmarshal([]byte(tc.Function.Arguments), &args) == nil {
					v.viewed(absPath(args.Path), v.next())
				}
			}
		}
	}
}

// answeredAfter reports whether a model call followed (an assistant message).
func answeredAfter(rest []*store.Message) bool {
	for _, m := range rest {
		if m.Role == store.RoleAssistant {
			return true
		}
	}
	return false
}

// TurnDeliverables are the deliverables changed in the current turn of a
// thread's messages (the verification state a run rebuilds), sorted by path,
// with their last checker result after their last change.
func TurnDeliverables(msgs []*store.Message) []Deliverable {
	return deliverablesOf(turnMessages(msgs))
}

// DeliverablesSince are the deliverables changed in the turns that started at
// or after since (a goal's turns: since its creation), like TurnDeliverables;
// the current turn when no turn started since then.
func DeliverablesSince(msgs []*store.Message, since time.Time) []Deliverable {
	for i, m := range msgs {
		if m.Role == store.RoleUser && !m.IsSteer() && !m.CreatedAt.Before(since) {
			return deliverablesOf(msgs[i:])
		}
	}
	return TurnDeliverables(msgs)
}

func deliverablesOf(turns []*store.Message) []Deliverable {
	st := &runState{}
	st.seedVerification(turns)
	v := &st.v3.verify
	names := make([]string, 0, len(v.files))
	for p, d := range v.files {
		if d.changed > 0 {
			names = append(names, p)
		}
	}
	sort.Strings(names)
	out := make([]Deliverable, 0, len(names))
	for _, p := range names {
		d := v.files[p]
		item := Deliverable{Path: p}
		if d.hasLast {
			item.Checker, item.Summary = d.last.Checker, d.last.Summary
			if checked, ok := d.verdict(); checked { // a checker that could not run gives no verdict
				item.OK = &ok
			}
		}
		out = append(out, item)
	}
	return out
}
