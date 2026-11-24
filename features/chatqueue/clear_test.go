package chatqueue_test

import (
	"context"
	"testing"

	"github.com/7ar-chat/harness/adapters/store/threadstate"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/features/chatqueue"
)

func TestClearEmptiesTheQueueAndReturnsWhatWaited(t *testing.T) {
	// Arrange
	ctx := context.Background()
	st := threadstate.NewMem()
	q := &chatqueue.Queue{St: st}
	att := []store.Attachment{{Name: "a.png", Path: "/workspace/a.png", MimeType: "image/png"}}
	_, _ = q.Add(ctx, "t", "one", chatqueue.Options{Attachments: att})
	_, _ = q.Add(ctx, "t", "two", chatqueue.Options{})
	_, _ = q.Add(ctx, "other", "keep", chatqueue.Options{})

	// Act
	got, err := q.Clear(ctx, "t")

	// Assert
	if err != nil || len(got) != 2 || got[0].Options.Attachments[0].Name != "a.png" {
		t.Fatalf("cleared %+v, %v", got, err)
	}
	if items, _ := q.List(ctx, "t"); len(items) != 0 {
		t.Fatalf("queue = %+v", items)
	}
	if raw, _ := st.GetThreadState(ctx, "t", chatqueue.Key); raw != nil {
		t.Fatalf("the empty queue left a document: %s", raw)
	}
	if items, _ := q.List(ctx, "other"); len(items) != 1 {
		t.Fatalf("another thread's queue was touched: %+v", items)
	}
}
