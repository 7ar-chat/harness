package store

import (
	"encoding/json"
	"strings"
)

// LatestPlan is the plan of the last successful update_plan call among msgs
// (in position order): the thread's plan once an edit hid later messages (nil
// when no visible call wrote one).
func LatestPlan(msgs []*Message) []PlanItem {
	failed := map[string]bool{}
	answered := map[string]bool{}
	for _, m := range msgs {
		if m.Role == RoleTool {
			answered[m.ToolCallID] = true
			if m.Error != "" {
				failed[m.ToolCallID] = true
			}
		}
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role != RoleAssistant {
			continue
		}
		for j := len(m.ToolCalls) - 1; j >= 0; j-- {
			tc := m.ToolCalls[j]
			if tc.Function.Name != "update_plan" || !answered[tc.ID] || failed[tc.ID] {
				continue
			}
			if items, ok := parsePlan(tc.Function.Arguments); ok {
				return items
			}
		}
	}
	return nil
}

func parsePlan(args string) ([]PlanItem, bool) {
	var in struct {
		Items []PlanItem `json:"items"`
	}
	if json.Unmarshal([]byte(args), &in) != nil {
		return nil, false
	}
	out := make([]PlanItem, 0, len(in.Items))
	for _, it := range in.Items {
		id, title := strings.TrimSpace(it.ID), strings.TrimSpace(it.Title)
		switch it.Status {
		case PlanPending, PlanInProgress, PlanCompleted:
		default:
			return nil, false
		}
		if id == "" || title == "" {
			return nil, false
		}
		out = append(out, PlanItem{ID: CleanText(id), Title: CleanText(title), Status: it.Status})
	}
	return out, true
}
