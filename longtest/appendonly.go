package longtest

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/GhaziBenDahmane/harness/ai"
)

// Fingerprint is the byte-exact identity of a message as the provider sees it.
func Fingerprint(m *ai.Message) string {
	b, _ := json.Marshal(struct {
		Role       ai.Role
		Content    string
		Reasoning  string
		ToolCalls  []ai.ToolCall
		ToolCallID string
		ToolName   string
	}{m.Role, m.Content, m.ReasoningContent, m.ToolCalls, m.ToolCallID, m.ToolName})
	return string(b)
}

// AssertAppendOnly reports an error unless next starts with prev, message for
// message and byte for byte: what a provider's prompt cache needs between two
// consecutive steps of a run. It is the strict form; use AppendOnlyExceptMasks
// for runs where tool-output masking is on.
func AssertAppendOnly(prev, next []*ai.Message) error {
	if len(next) < len(prev) {
		return fmt.Errorf("the request shrank from %d to %d messages", len(prev), len(next))
	}
	for i, m := range prev {
		if Fingerprint(m) != Fingerprint(next[i]) {
			return fmt.Errorf("message %d changed between consecutive requests (%s)", i, next[i].Role)
		}
	}
	return nil
}

// IsMaskStub reports whether a tool message is the stub masking leaves behind.
func IsMaskStub(m *ai.Message) bool {
	return m.Role == ai.RoleTool && strings.HasPrefix(m.Content, "[") && strings.Contains(m.Content, " result, ") &&
		strings.Contains(m.Content, "hidden to save context")
}

// AppendOnlyExceptMasks is AssertAppendOnly that tolerates one change: a tool
// result of prev replaced by its mask stub in next (masking moves the boundary
// as the prompt grows). It returns the index of the first message that
// differs (len(prev) when none does), so callers can measure how much of the
// previous request a cache keeps, and how many results were newly masked.
func AppendOnlyExceptMasks(prev, next []*ai.Message) (firstDiff, newlyMasked int, err error) {
	if len(next) < len(prev) {
		return 0, 0, fmt.Errorf("the request shrank from %d to %d messages", len(prev), len(next))
	}
	firstDiff = len(prev)
	for i, m := range prev {
		if Fingerprint(m) == Fingerprint(next[i]) {
			continue
		}
		if !(m.Role == ai.RoleTool && !IsMaskStub(m) && IsMaskStub(next[i]) && next[i].ToolCallID == m.ToolCallID) {
			return i, newlyMasked, fmt.Errorf("message %d (%s) was rewritten between consecutive requests, not by masking", i, m.Role)
		}
		if firstDiff == len(prev) {
			firstDiff = i
		}
		newlyMasked++
	}
	return firstDiff, newlyMasked, nil
}
