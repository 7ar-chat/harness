package schedstore_test

import (
	"testing"

	"github.com/7ar-chat/harness/core/schedule"
)

func TestClearThreadForgetsTheReusedConversationOnly(t *testing.T) {
	eachStore(t, func(t *testing.T, s schedule.Store) {
		// Arrange: two schedules reusing a conversation each.
		var ids [2]string
		for i, thread := range []string{"gone", "kept"} {
			in := sched(thread, t0)
			in.ReuseSession = true
			sc, err := s.Create(ctx, in)
			must(t, err)
			ids[i] = sc.ID
			cl, err := s.StartManual(ctx, sc.ID, "ann", t0)
			must(t, err)
			must(t, s.SetThread(ctx, cl.Run.ID, thread, ""))
		}

		// Act
		must(t, s.ClearThread(ctx, "gone"))

		// Assert
		gone, _ := s.Get(ctx, ids[0])
		kept, _ := s.Get(ctx, ids[1])
		if gone.ThreadID != "" || kept.ThreadID != "kept" {
			t.Fatalf("gone %q kept %q", gone.ThreadID, kept.ThreadID)
		}
		runs, _ := s.Runs(ctx, ids[0], 5)
		if len(runs) != 1 || runs[0].ThreadID != "gone" {
			t.Fatalf("the run history must be kept: %+v", runs)
		}
		must(t, s.ClearThread(ctx, "never-existed"))
	})
}
