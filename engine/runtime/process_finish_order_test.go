package runtime_test

import (
	"context"
	"testing"

	"github.com/7ar-chat/harness/core/store"
)

func TestTerminalEventIsPublishedAfterRunIsFinished(t *testing.T) {
	// When a client sees RUN_FINISHED live, the run is already terminal in the database (thread reloads show the answer).
	// Arrange
	r := newRig(t)
	_, run := r.queued("q")
	statusAtTerminal := make(chan string, 1)
	go func() {
		for ev := range r.log.Replay(context.Background(), run.ID, 0) {
			if ev.Type == "RUN_FINISHED" {
				got, _ := r.st.GetRun(context.Background(), run.ID)
				statusAtTerminal <- got.Status
				return
			}
		}
	}()

	// Act
	r.process(run)

	// Assert
	if s := <-statusAtTerminal; s != store.StatusCompleted {
		t.Fatalf("status when RUN_FINISHED was visible: %s", s)
	}
}
