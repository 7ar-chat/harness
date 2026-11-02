package eventlog_test

import (
	"context"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/eventlog"
)

func TestMemoryLogReplayAfterTheTerminalEventEndsAtOnce(t *testing.T) {
	// A client reconnecting with the last seq of a finished run gets an empty stream that closes, not a tail that never ends.
	// Arrange
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st := memstore.New()
	live := eventlog.NewMemory(st)
	w := live.Writer("run", 0)
	_, _ = w.Append(ctx, step(1))
	_, _ = w.Append(ctx, agui.RunFinishedEvent("t", "run", "success", nil))
	_ = w.Close(ctx)

	for name, log := range map[string]*eventlog.MemoryLog{"same process": live, "new process": eventlog.NewMemory(st)} {
		// Act
		ch := log.Replay(ctx, "run", 2)
		var got []store.Event
	drain:
		for {
			select {
			case e, ok := <-ch:
				if !ok {
					break drain
				}
				got = append(got, e)
			case <-ctx.Done():
				t.Fatalf("%s: replay after the terminal event did not end", name)
			}
		}

		// Assert
		if len(got) != 0 {
			t.Errorf("%s: replayed %v after the terminal event, want nothing", name, got)
		}

		// Control: one seq earlier still delivers the terminal event.
		var seqs []int64
		for e := range log.Replay(ctx, "run", 1) {
			seqs = append(seqs, e.Seq)
		}
		if len(seqs) != 1 || seqs[0] != 2 {
			t.Errorf("%s: replay after seq 1 = %v, want [2]", name, seqs)
		}
	}
}
