package workflows_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/7ar-chat/harness/core/sandbox"
	core "github.com/7ar-chat/harness/core/workflows"
	wf "github.com/7ar-chat/harness/features/workflows"
)

func TestOnFinishedIsCalledOnceWhenARunEnds(t *testing.T) {
	// Arrange
	x := newExecRig(t)
	var finished atomic.Int32
	var runID atomic.Value
	x.eng = wf.New(wf.Options{Store: x.store, Tasks: x.tasks,
		Sandbox:    func(context.Context, core.Run, string, string) (sandbox.Sandbox, error) { return x.sb, nil },
		OnFinished: func(r core.Run) { finished.Add(1); runID.Store(r.ID) }})
	w := x.publish("one", script("s", "true"))

	// Act
	run := x.start(w)
	run = x.settle(run, "s", core.NodeDone)
	x.eng.Wait()

	// Assert
	if run.State != core.RunDone || finished.Load() != 1 || runID.Load() != run.ID {
		t.Fatalf("state %s, finished %d", run.State, finished.Load())
	}
	if _, err := x.eng.Advance(x.ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	x.eng.Wait()
	if finished.Load() != 1 {
		t.Fatalf("a settled run was reported again: %d", finished.Load())
	}
}
