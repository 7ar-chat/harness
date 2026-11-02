package pgstore_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/7ar-chat/harness/core/store"
)

func TestPGKeepsTheReasoningItemsOfAnAssistantMessage(t *testing.T) {
	// The opaque reasoning items of an assistant message (Responses API) are stored with it and listed back unchanged; other messages have none.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "")
	run, _, err := s.CreateRun(ctx, newRun(th.ID, "q"))
	if err != nil {
		t.Fatal(err)
	}
	items := json.RawMessage(`[{"id":"rs_1","encrypted_content":"gAAAA-x","model":"gpt-5.6-terra"}]`)
	answer := &store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleAssistant, Content: "a", ReasoningItems: items}

	// Act
	err = s.AppendMessages(ctx, answer)

	// Assert
	msgs, lerr := s.ListMessages(ctx, th.ID)
	if err != nil || lerr != nil || len(msgs) != 2 || msgs[0].ReasoningItems != nil {
		t.Fatalf("err %v %v msgs %+v", err, lerr, msgs)
	}
	var got, want any
	_ = json.Unmarshal(msgs[1].ReasoningItems, &got)
	_ = json.Unmarshal(items, &want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reasoning items %s", msgs[1].ReasoningItems)
	}
}
