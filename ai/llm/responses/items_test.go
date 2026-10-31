package responses_test

import (
	"reflect"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/responses"
)

func TestItemsOfReadsEveryShapeAMessageCanBeStoredIn(t *testing.T) {
	// Items come back from typed Extra, from Extra decoded from JSON, and are nil for messages without usable items.
	// Arrange
	typed := []responses.ReasoningItem{{ID: "rs_1", EncryptedContent: "e", Model: "m"}}
	cases := []struct {
		name string
		msg  *ai.Message
		want []responses.ReasoningItem
	}{
		{"nil message", nil, nil},
		{"no extra", &ai.Message{}, nil},
		{"no key", &ai.Message{Extra: map[string]any{"other": 1}}, nil},
		{"typed", &ai.Message{Extra: map[string]any{responses.ExtraReasoningItems: typed}}, typed},
		{"decoded from JSON", &ai.Message{Extra: map[string]any{responses.ExtraReasoningItems: []any{
			map[string]any{"id": "rs_2", "encrypted_content": "e2", "model": "m", "summary": []any{map[string]any{"type": "summary_text", "text": "t"}}}}}},
			[]responses.ReasoningItem{{ID: "rs_2", EncryptedContent: "e2", Model: "m", Summary: []responses.SummaryPart{{Type: "summary_text", Text: "t"}}}}},
		{"wrong shape", &ai.Message{Extra: map[string]any{responses.ExtraReasoningItems: "not items"}}, nil},
		{"unencodable", &ai.Message{Extra: map[string]any{responses.ExtraReasoningItems: make(chan int)}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Act
			got := responses.ItemsOf(tc.msg)

			// Assert
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ItemsOf = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestSetItemsAttachesOnlyRealItemsAndKeepsOtherExtras(t *testing.T) {
	// No items (or no message) leaves the message as it was; items are added next to existing Extra values.
	// Arrange
	untouched := &ai.Message{}
	withExtra := &ai.Message{Extra: map[string]any{"keep": "me"}}

	// Act
	responses.SetItems(nil, []responses.ReasoningItem{{ID: "x"}})
	responses.SetItems(untouched, nil)
	responses.SetItems(withExtra, []responses.ReasoningItem{{ID: "rs_1", EncryptedContent: "e"}})

	// Assert
	if untouched.Extra != nil {
		t.Errorf("Extra = %v, want nil", untouched.Extra)
	}
	if withExtra.Extra["keep"] != "me" || len(responses.ItemsOf(withExtra)) != 1 {
		t.Errorf("Extra = %v", withExtra.Extra)
	}
}

func TestStoredItemsRoundTrip(t *testing.T) {
	// Items persisted with MarshalItems come back identical from UnmarshalItems; empty or corrupt storage yields no items instead of an error.
	// Arrange
	items := []responses.ReasoningItem{
		{ID: "rs_1", EncryptedContent: "e1", Model: "m", Summary: []responses.SummaryPart{{Type: "summary_text", Text: "t"}}},
		{ID: "rs_2", EncryptedContent: "e2"},
	}

	// Act
	raw := responses.MarshalItems(items)
	back := responses.UnmarshalItems(raw)

	// Assert
	if !reflect.DeepEqual(back, items) {
		t.Fatalf("round trip %+v, want %+v", back, items)
	}
	if responses.MarshalItems(nil) != nil || responses.MarshalItems([]responses.ReasoningItem{}) != nil {
		t.Error("no items must marshal to nil")
	}
	for _, bad := range [][]byte{nil, {}, []byte("not json"), []byte(`{"id":"x"}`)} {
		if got := responses.UnmarshalItems(bad); got != nil {
			t.Errorf("UnmarshalItems(%q) = %+v, want nil", bad, got)
		}
	}
}
