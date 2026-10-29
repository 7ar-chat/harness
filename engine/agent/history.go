package agent

import (
	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/engine/compaction"
	"github.com/GhaziBenDahmane/harness/engine/llm"
	"github.com/GhaziBenDahmane/harness/store"
)

// BuildHistory turns persisted messages into a provider-valid model history:
// reasoning rows are skipped, steers are prefixed, every tool call is followed
// by its result (found by tool call id, wherever it was stored: an ask_user
// answer arrives after the steers of its run) or a synthetic "interrupted"
// result, orphan results are dropped, and tool outputs older than the last two
// assistant turns are cut to their head and tail. Each message carries its store position.
func BuildHistory(msgs []*store.Message) []*schema.Message {
	return BuildHistoryWith(msgs, HistoryOptions{})
}

// HistoryOptions tune BuildHistoryWith.
type HistoryOptions struct {
	// Reasoning gives every assistant message the reasoning stored before it in
	// its run (reasoning passback, harness v3 §5) instead of dropping it.
	Reasoning bool
}

// BuildHistoryWith is BuildHistory with options.
func BuildHistoryWith(msgs []*store.Message, opts HistoryOptions) []*schema.Message {
	var filtered []*store.Message
	results := map[string]*store.Message{}
	reasoningOf := map[string]string{} // assistant message id → reasoning of its step
	pending := map[string]string{}     // run id → reasoning not attached yet
	for _, m := range msgs {
		switch m.Role {
		case store.RoleReasoning:
			pending[m.RunID] += m.Content
		case store.RoleAssistant:
			if r := pending[m.RunID]; r != "" {
				reasoningOf[m.ID] = r
				delete(pending, m.RunID)
			}
			filtered = append(filtered, m)
		case store.RoleTool:
			if _, dup := results[m.ToolCallID]; !dup {
				results[m.ToolCallID] = m
			}
		default:
			filtered = append(filtered, m)
		}
	}
	cutoff := -1
	seen := 0
	for i := len(filtered) - 1; i >= 0; i-- {
		if filtered[i].Role == store.RoleAssistant {
			seen++
			if seen == 2 {
				cutoff = i
				break
			}
		}
	}

	multi := multiAuthor(filtered)
	var out []*schema.Message
	add := func(m *schema.Message, pos int64) {
		compaction.SetPosition(m, pos)
		out = append(out, m)
	}
	answered := map[string]bool{}
	for i, m := range filtered {
		switch m.Role {
		case store.RoleUser:
			content := m.Content
			if multi {
				content = AuthorTag(m.AuthorLogin, m.AuthorKind, m.OnBehalfOf) + content
			}
			if m.IsSteer() {
				content = SteerPrefix + content
			}
			um := userMessage(m, content)
			if m.IsSteer() {
				compaction.MarkSteer(um)
			} else {
				compaction.MarkRequest(um)
			}
			add(um, m.Position)
		case store.RoleAssistant:
			if m.Content == "" && len(m.ToolCalls) == 0 {
				continue
			}
			calls := make([]schema.ToolCall, 0, len(m.ToolCalls))
			for _, c := range m.ToolCalls {
				if answered[c.ID] {
					continue // a reused id would make the history invalid
				}
				calls = append(calls, schema.ToolCall{ID: c.ID, Type: "function", Function: schema.FunctionCall{Name: c.Function.Name, Arguments: c.Function.Arguments}})
			}
			if len(calls) == 0 {
				if m.Content == "" {
					continue
				}
				calls = nil
			}
			am := schema.AssistantMessage(m.Content, calls)
			if opts.Reasoning {
				am.ReasoningContent = reasoningOf[m.ID]
			}
			llm.SetReasoningItems(am, m.ReasoningItems)              // Responses API reasoning items, dropped with the message
			compaction.SetFiles(am, store.ArtifactPaths(m.Metadata)) // end-of-run changes
			add(am, m.Position)
			for _, c := range calls {
				answered[c.ID] = true
				r := results[c.ID]
				if r == nil {
					out = append(out, schema.ToolMessage(interruptedResult, c.ID, schema.WithToolName(c.Function.Name)))
					continue
				}
				content := r.Content
				if i < cutoff {
					content = truncateOld(content)
				}
				tm := schema.ToolMessage(content, c.ID, schema.WithToolName(c.Function.Name))
				compaction.SetFiles(tm, store.ArtifactPaths(r.Metadata))
				add(tm, r.Position)
			}
		}
	}
	return out
}

// truncateOld keeps the head and tail of an old tool output (its stderr and
// exitCode come last, A-24).
func truncateOld(s string) string { return compaction.Preview(s, oldToolOutputChars) }

// multiAuthor reports whether the user messages come from more than one
// speaker (several users, or a supervisor next to a user): only then is each
// message tagged with its author in the model's history, so single-user
// threads keep the prompts they always had.
func multiAuthor(msgs []*store.Message) bool {
	seen := map[string]bool{}
	for _, m := range msgs {
		if m.Role != store.RoleUser {
			continue
		}
		who := m.AuthorLogin
		if m.AuthorKind == store.AuthorSupervisor {
			who = "supervisor:" + m.OnBehalfOf
		}
		if who == "" {
			continue
		}
		seen[who] = true
		if len(seen) > 1 {
			return true
		}
	}
	return false
}

// AuthorTag is the "[alice] " prefix that tells the model who wrote a user
// message ("" when the author is unknown).
func AuthorTag(login, kind, onBehalfOf string) string {
	switch {
	case kind == store.AuthorSupervisor && onBehalfOf != "":
		return "[supervisor, for " + onBehalfOf + "] "
	case kind == store.AuthorSupervisor:
		return "[supervisor] "
	case login != "":
		return "[" + login + "] "
	}
	return ""
}
