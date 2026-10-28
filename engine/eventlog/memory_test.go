package eventlog_test

import (
	"context"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/engine/agui"
	"github.com/GhaziBenDahmane/harness/engine/eventlog"
	"github.com/GhaziBenDahmane/harness/store/memstore"
)

func TestMemoryLogTailsLiveAndReplaysAfterASeq(t *testing.T) {
	// Arrange
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st := memstore.New()
	log := eventlog.NewMemory(st)
	w := log.Writer("run-1", 0)
	_, _ = w.Append(ctx, agui.RunStartedEvent("t", "run-1"))
	_, _ = w.Append(ctx, agui.New(agui.StepStarted, map[string]any{"stepName": "step-1"}))

	// Act
	ch := log.Replay(ctx, "run-1", 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Append(ctx, agui.RunFinishedEvent("t", "run-1", "success", nil))
	}()
	var got []string
	for e := range ch {
		got = append(got, e.Type)
	}

	// Assert
	if len(got) != 2 || got[0] != agui.StepStarted || got[1] != agui.RunFinished {
		t.Fatalf("replayed %v", got)
	}
	if _, ok := w.Terminal(); !ok {
		t.Fatal("no terminal event")
	}
}

func TestMemoryLogPersistsOnFlushAndReplaysFromTheStoreInANewProcess(t *testing.T) {
	// Arrange
	ctx := context.Background()
	st := memstore.New()
	w := eventlog.NewMemory(st).Writer("run-1", 0)
	_, _ = w.Append(ctx, agui.RunStartedEvent("t", "run-1"))
	_, _ = w.Append(ctx, agui.RunFinishedEvent("t", "run-1", "success", nil))
	if err := w.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// Act: a fresh log over the same store
	var got []string
	for e := range eventlog.NewMemory(st).Replay(ctx, "run-1", 0) {
		got = append(got, e.Type)
	}

	// Assert
	if len(got) != 2 || got[1] != agui.RunFinished {
		t.Fatalf("replayed %v", got)
	}
}
