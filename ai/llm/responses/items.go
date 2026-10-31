package responses

import (
	"encoding/json"

	"github.com/GhaziBenDahmane/harness/ai"
)

// ExtraReasoningItems is the ai.Message.Extra key of the reasoning items
// an assistant message was generated with ([]ReasoningItem). The model sets it
// on the last chunk of a stream (so ai.ConcatMessages keeps it as is), and
// sends the items back, in order, before the message's own items.
const ExtraReasoningItems = "responses_reasoning_items"

// ReasoningItem is an opaque reasoning item of the Responses API (store:false,
// include reasoning.encrypted_content): the model's reasoning, encrypted by the
// provider, that the next requests carry back so it is not lost between tool
// steps and runs.
type ReasoningItem struct {
	ID               string        `json:"id"`
	EncryptedContent string        `json:"encrypted_content"`
	Summary          []SummaryPart `json:"summary,omitempty"`
	// Model is the id of the model that produced the item: it is only sent back to that model.
	Model string `json:"model,omitempty"`
}

// SummaryPart is a reasoning summary part ({"type":"summary_text","text":…}).
type SummaryPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ItemsOf returns the reasoning items an assistant message carries (nil: none).
func ItemsOf(m *ai.Message) []ReasoningItem {
	if m == nil || m.Extra == nil {
		return nil
	}
	switch v := m.Extra[ExtraReasoningItems].(type) {
	case nil:
		return nil
	case []ReasoningItem:
		return v
	default: // a message that went through JSON (map/[]any)
		b, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		var items []ReasoningItem
		if json.Unmarshal(b, &items) != nil {
			return nil
		}
		return items
	}
}

// SetItems attaches reasoning items to a message (no-op without items).
func SetItems(m *ai.Message, items []ReasoningItem) {
	if m == nil || len(items) == 0 {
		return
	}
	if m.Extra == nil {
		m.Extra = map[string]any{}
	}
	m.Extra[ExtraReasoningItems] = items
}

// MarshalItems encodes reasoning items for storage (nil: none).
func MarshalItems(items []ReasoningItem) json.RawMessage {
	if len(items) == 0 {
		return nil
	}
	b, err := json.Marshal(items)
	if err != nil {
		return nil
	}
	return b
}

// UnmarshalItems decodes stored reasoning items (nil when empty or invalid).
func UnmarshalItems(raw []byte) []ReasoningItem {
	if len(raw) == 0 {
		return nil
	}
	var items []ReasoningItem
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	return items
}
