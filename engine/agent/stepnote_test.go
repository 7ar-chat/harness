package agent

import (
	"context"
	"testing"

	"github.com/GhaziBenDahmane/harness/core/store"
)

func TestStepNotesAppendOneSystemReminderPerStep(t *testing.T) {
	// Arrange
	st := &runState{in: Input{Run: &store.Run{ID: "run-1"}}, a: &Agent{Harness: Harness{StepNotes: []StepNote{
		func(context.Context, Input) string { return "first" },
		nil,
		func(context.Context, Input) string { return "  " },
		func(context.Context, Input) string { panic("boom") },
		func(context.Context, Input) string { return "second" },
	}}}}

	// Act
	st.stepNotes(context.Background())

	// Assert
	if len(st.history) != 1 || st.history[0].Content != "<system-reminder>\nfirst\n\nsecond\n</system-reminder>" {
		t.Fatalf("history %+v", st.history)
	}
}

func TestStepNotesAddNothingWhenNoHookHasNews(t *testing.T) {
	// Arrange
	st := &runState{in: Input{Run: &store.Run{ID: "run-1"}}, a: &Agent{Harness: Harness{StepNotes: []StepNote{
		func(context.Context, Input) string { return "" },
	}}}}

	// Act
	st.stepNotes(context.Background())

	// Assert
	if len(st.history) != 0 {
		t.Fatalf("history %+v", st.history)
	}
}
