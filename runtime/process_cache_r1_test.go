package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/store"
)

func TestGoalTurnsShareOneSystemPromptAndGetTheirCounterAtTheEnd(t *testing.T) {
	// Successive goal turns send a byte-identical system prompt (the prompt cache keeps the whole history): the "turns used" counter is in a trailing reminder, not in the system prompt.
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: llmtest.Text("I loaded the data.")},
		llmtest.Turn{Chunks: llmtest.Text(`{"met": false, "reason": "The PDF does not exist yet."}`)},
		llmtest.Turn{Chunks: llmtest.Text("The PDF is written.")},
		llmtest.Turn{Chunks: llmtest.Text(`{"met": true, "reason": "Done."}`)},
	)
	th, _, first := r.goalRun(3)
	r.process(first)
	msgs := r.messages(th.ID)
	second := r.reload(r.mustRun(msgs[len(msgs)-1].RunID))

	// Act
	r.process(second)

	// Assert
	calls := r.model.Calls()
	if len(calls) != 4 {
		t.Fatalf("calls %d", len(calls))
	}
	turn1, turn2 := calls[0].Messages, calls[2].Messages
	if turn1[0].Content != turn2[0].Content || strings.Contains(turn1[0].Content, "turns used") {
		t.Fatalf("system prompts differ between goal turns or carry the counter:\n%s\n---\n%s", turn1[0].Content, turn2[0].Content)
	}
	last1, last2 := turn1[len(turn1)-1], turn2[len(turn2)-1]
	if last1.Role != schema.User || !strings.Contains(last1.Content, "1 of 3 automatic goal turns used") || !strings.Contains(last2.Content, "2 of 3 automatic goal turns used") {
		t.Fatalf("trailing reminders %q / %q", last1.Content, last2.Content)
	}
	for i := 0; i < len(turn1)-1; i++ {
		if !sameMessage(turn1[i], turn2[i]) {
			t.Fatalf("goal turn 2 rewrote message %d of turn 1's request", i)
		}
	}
}

// mustRun loads a run.
func (r *rig) mustRun(id string) *store.Run {
	r.t.Helper()
	run, err := r.st.GetRun(context.Background(), id)
	if err != nil {
		r.t.Fatal(err)
	}
	return run
}
