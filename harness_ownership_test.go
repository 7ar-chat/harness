package harness_test

import (
	"errors"
	"testing"

	"github.com/GhaziBenDahmane/harness"
	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/store"
)

func TestSendAndResumeRefuseAThreadOfAnotherUser(t *testing.T) {
	// Arrange: alice's thread has a run in flight (a blocked model call), so a careless check would answer
	// "run active" instead of "not found" and reveal that the thread exists.
	h, ctx := newTestHarness(t, llmtest.Turn{Chunks: llmtest.Text("working"), Block: true})
	th, err := h.NewThread(ctx, "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Send(ctx, "alice", th.ID, "start", harness.RunOptions{Model: "gpt-test"}); err != nil {
		t.Fatal(err)
	}

	// Act
	_, sendBusy := h.Send(ctx, "bob", th.ID, "hijack", harness.RunOptions{Model: "gpt-test"})
	_, resumeBusy := h.Resume(ctx, "bob", th.ID, harness.ResumeRequest{Model: "gpt-test", Reason: "ask_user", Text: "x"})
	_, sendMissing := h.Send(ctx, "bob", "no-such-thread", "hi", harness.RunOptions{Model: "gpt-test"})
	_, sendOwner := h.Send(ctx, "alice", th.ID, "second", harness.RunOptions{Model: "gpt-test"})

	// Assert
	for name, err := range map[string]error{"send to a busy foreign thread": sendBusy, "resume on a busy foreign thread": resumeBusy, "send to a missing thread": sendMissing} {
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s: want store.ErrNotFound, got %v", name, err)
		}
	}
	if !errors.Is(sendOwner, store.ErrRunActive) {
		t.Errorf("the owner sending while a run is active: want store.ErrRunActive, got %v", sendOwner)
	}
}

func TestSendToAnIdleForeignThreadIsRefused(t *testing.T) {
	// Arrange
	h, ctx := newTestHarness(t, llmtest.Turn{Chunks: llmtest.Text("hi")}, llmtest.Turn{Chunks: llmtest.Text("title")})
	th, _ := h.NewThread(ctx, "alice", "")

	// Act
	run, err := h.Send(ctx, "bob", th.ID, "hello", harness.RunOptions{Model: "gpt-test"})

	// Assert
	if run != nil || !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("run %v err %v", run, err)
	}
	if msgs, _ := h.Store.ListMessages(ctx, th.ID); len(msgs) != 0 {
		t.Fatalf("a refused send left %d messages in alice's thread", len(msgs))
	}
}
