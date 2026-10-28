package runtime_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/engine/runtime"
	"github.com/GhaziBenDahmane/harness/features/goals"
	"github.com/GhaziBenDahmane/harness/store"
)

const goalText = "Produce /workspace/report.pdf with a sales chart"

// goalRun sets a goal on a new thread and queues its first goal run, like PUT /threads/{id}/goal.
func (r *rig) goalRun(maxTurns int) (*store.Thread, *store.Goal, *store.Run) {
	r.t.Helper()
	ctx := context.Background()
	th, _ := r.st.CreateThread(ctx, "alice", "t")
	g, err := r.st.PutGoal(ctx, "alice", th.ID, goalText, maxTurns)
	if err != nil {
		r.t.Fatal(err)
	}
	run, _, err := r.st.CreateRun(ctx, goals.NewRun(g, "alice", "gpt-test", "", "", goals.StartMessage(g.Text)))
	if err != nil {
		r.t.Fatal(err)
	}
	return th, g, run
}

func (r *rig) goal(threadID string) *store.Goal {
	g, _ := r.st.ThreadGoal(context.Background(), threadID)
	return g
}

func goalEvent(t *testing.T, evs []event) map[string]any {
	t.Helper()
	tt := types(evs)
	if n := len(tt); n < 2 || tt[n-2] != "CUSTOM:work.goal" {
		t.Fatalf("work.goal is not right before the terminal event: %v", tt)
	}
	return evs[len(evs)-2]["value"].(map[string]any)
}

func TestProcessGoalNotMetStartsTheNextGoalRun(t *testing.T) {
	// An unmet goal with turns left gets a queued goal run whose user message (source goal) carries the judge's reason; work.goal announces it.
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: llmtest.Text("I loaded the data.")},
		llmtest.Turn{Chunks: llmtest.Text(`{"met": false, "reason": "The PDF does not exist yet."}`)},
	)
	th, _, run := r.goalRun(3)

	// Act
	r.process(run)

	// Assert
	if sys := r.model.Calls()[0].Messages[0].Content; !strings.Contains(sys, "<context>") || !strings.Contains(sys[strings.Index(sys, "<context>"):], "Goal (checked automatically after each of your answers): "+goalText) {
		t.Fatalf("goal not in the system prompt: %q", sys)
	}
	if first := r.model.Calls()[0].Messages; !strings.Contains(first[len(first)-1].Content, "1 of 3 automatic goal turns used") {
		t.Fatalf("goal turn counter not at the end of the request: %q", first[len(first)-1].Content)
	}
	if judge := r.model.Calls()[1].Messages; judge[0].Content != goals.JudgeInstructions || !strings.Contains(judge[1].Content, "I loaded the data.") {
		t.Fatalf("judge call %+v", judge)
	}
	ev := goalEvent(t, r.events(run.ID))
	g := r.goal(th.ID)
	if g.Status != store.GoalActive || g.TurnsUsed != 2 || g.LastVerdict == nil || g.LastVerdict.Met || g.LastVerdict.Reason != "The PDF does not exist yet." {
		t.Fatalf("goal %+v", g)
	}
	next, err := r.st.GetRun(context.Background(), ev["nextRunId"].(string))
	if err != nil || next.Kind != store.KindGoal || next.Status != store.StatusQueued || ev["turnsUsed"] != float64(2) || ev["status"] != "active" {
		t.Fatalf("event %v next %+v %v", ev, next, err)
	}
	msgs := r.messages(th.ID)
	m := msgs[len(msgs)-1]
	if m.RunID != next.ID || m.Metadata["source"] != "goal" || m.Content != "Continue working toward the goal. Last check: The PDF does not exist yet." {
		t.Fatalf("goal message %+v", m)
	}
	entries, _ := r.rdb.XRange(context.Background(), "work:runs", "-", "+").Result()
	if len(entries) != 1 || entries[0].Values["runId"] != next.ID {
		t.Fatalf("queue %v", entries)
	}
}

func TestProcessGoalMetStopsTheLoop(t *testing.T) {
	// A met goal is marked met and no run is started.
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: llmtest.Text("report.pdf is ready.")},
		llmtest.Turn{Chunks: llmtest.Text("```json\n{\"met\": true, \"reason\": \"The PDF with the chart was written.\"}\n```")},
	)
	th, _, run := r.goalRun(3)

	// Act
	r.process(run)

	// Assert
	ev := goalEvent(t, r.events(run.ID))
	if g := r.goal(th.ID); g.Status != store.GoalMet || ev["status"] != "met" || ev["nextRunId"] != nil || g.TurnsUsed != 1 {
		t.Fatalf("goal %+v event %v", g, ev)
	}
}

func TestProcessGoalOutOfTurnsIsExhausted(t *testing.T) {
	// An unmet goal whose turns are used up becomes exhausted.
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: llmtest.Text("partial")},
		llmtest.Turn{Chunks: llmtest.Text(`{"met": false, "reason": "Still missing the chart."}`)},
	)
	th, _, run := r.goalRun(1)

	// Act
	r.process(run)

	// Assert
	ev := goalEvent(t, r.events(run.ID))
	if g := r.goal(th.ID); g.Status != store.GoalExhausted || ev["nextRunId"] != nil {
		t.Fatalf("goal %+v event %v", g, ev)
	}
}

func TestProcessGoalRetriesAnInvalidVerdictOnce(t *testing.T) {
	// A verdict that is not JSON is asked again once.
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: llmtest.Text("done")},
		llmtest.Turn{Chunks: llmtest.Text("Yes, it looks complete.")},
		llmtest.Turn{Chunks: llmtest.Text(`{"met": true, "reason": "Complete."}`)},
	)
	th, _, run := r.goalRun(3)

	// Act
	r.process(run)

	// Assert
	if g := r.goal(th.ID); g.Status != store.GoalMet || len(r.model.Calls()) != 3 {
		t.Fatalf("goal %+v calls %d", g, len(r.model.Calls()))
	}
}

func TestProcessGoalStopsAfterTwoFailedEvaluations(t *testing.T) {
	// A failed evaluation counts as not met ("evaluation failed") and continues once; a second consecutive failure stops auto-continuation.
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: llmtest.Text("step one")},
		llmtest.Turn{Chunks: llmtest.Text("no json")},
		llmtest.Turn{Chunks: llmtest.Text("still no json")},
		llmtest.Turn{Chunks: llmtest.Text("step two")},
		llmtest.Turn{Chunks: llmtest.Text("nope")},
		llmtest.Turn{Chunks: llmtest.Text("nope again")},
	)
	th, _, run := r.goalRun(5)
	r.process(run)
	first := goalEvent(t, r.events(run.ID))
	next, _ := r.st.GetRun(context.Background(), first["nextRunId"].(string))

	// Act
	r.process(next)

	// Assert
	second := goalEvent(t, r.events(next.ID))
	g := r.goal(th.ID)
	if first["lastVerdict"].(map[string]any)["reason"] != goals.EvaluationFailed || second["nextRunId"] != nil ||
		g.Status != store.GoalActive || g.EvalFailures != 2 || g.LastVerdict.Reason != goals.EvaluationFailed {
		t.Fatalf("first %v second %v goal %+v", first, second, g)
	}
}

func TestProcessGoalIsNotJudgedWhileWaitingForAnAnswer(t *testing.T) {
	// A run waiting for an ask_user answer is not judged and starts nothing.
	// Arrange
	r := newRig(t, llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "q", "ask_user", `{"question":"Which year?"}`)}})
	th, _, run := r.goalRun(3)

	// Act
	r.process(run)

	// Assert
	g := r.goal(th.ID)
	if len(find(r.events(run.ID), "CUSTOM:work.goal")) != 0 || g.LastVerdict != nil || len(r.model.Calls()) != 1 {
		t.Fatalf("goal %+v calls %d", g, len(r.model.Calls()))
	}
}

// userRunMeanwhile starts a user run right after the finishing run's status is recorded.
type userRunMeanwhile struct {
	r   *rig
	run *store.Run
}

func (u *userRunMeanwhile) Finishing(_ context.Context, f *runtime.Finish) func(context.Context) {
	return func(context.Context) {
		th := u.r.thread(f.Run.ThreadID)
		u.run = u.r.create(th, store.NewRun{Message: store.Message{Role: store.RoleUser, Content: "wait, use 2024 data"}})
	}
}

func TestProcessGoalYieldsToAUserMessageSentMeanwhile(t *testing.T) {
	// When a user run gets in first, no goal run is started; the goal stays active and is checked after that run.
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: llmtest.Text("working")},
		llmtest.Turn{Chunks: llmtest.Text(`{"met": false, "reason": "Not yet."}`)},
	)
	meanwhile := &userRunMeanwhile{r: r}
	r.opts.Hooks = append([]runtime.Hook{meanwhile}, r.opts.Hooks...)
	th, _, run := r.goalRun(3)

	// Act
	r.process(run)

	// Assert
	ev := goalEvent(t, r.events(run.ID))
	g := r.goal(th.ID)
	if ev["nextRunId"] != nil || g.Status != store.GoalActive || g.TurnsUsed != 1 || r.thread(th.ID).ActiveRun.ID != meanwhile.run.ID {
		t.Fatalf("event %v goal %+v", ev, g)
	}
}

func TestProcessGoalIsNotJudgedAfterAFailedRun(t *testing.T) {
	// A failed run is not judged and starts nothing.
	// Arrange
	r := newRig(t, llmtest.Turn{Err: errors.New("400 bad request")})
	th, _, run := r.goalRun(3)

	// Act
	got := r.process(run)

	// Assert
	if g := r.goal(th.ID); got.Status != store.StatusFailed || g.LastVerdict != nil || len(r.model.Calls()) != 1 {
		t.Fatalf("status %s goal %+v calls %d", got.Status, g, len(r.model.Calls()))
	}
}

// hookFunc is a runtime.Hook running fn in Finishing.
type hookFunc func(ctx context.Context, f *runtime.Finish)

func (h hookFunc) Finishing(ctx context.Context, f *runtime.Finish) func(context.Context) {
	h(ctx, f)
	return nil
}

func TestProcessGoalReplacedDuringTheJudgeGetsItsOwnRun(t *testing.T) {
	// A goal replaced (PUT) while the previous goal is being judged starts a goal run for the new goal once the run ends.
	// Arrange
	r := newRig(t, llmtest.Turn{Chunks: llmtest.Text("Report done.")})
	th, _, run := r.goalRun(3)
	var replaced *store.Goal
	r.model = llmtest.New(
		llmtest.Turn{Chunks: llmtest.Text("Report done.")},
		llmtest.Turn{Chunks: llmtest.Text(`{"met": true, "reason": "Done."}`), OnCall: func() {
			replaced, _ = r.st.PutGoal(context.Background(), "alice", th.ID, "Now translate the report", 3) // the run is active: no run started
		}},
	)
	r.agent.Models = llmtest.Factory{M: r.model}
	for _, h := range r.opts.Hooks {
		if g, ok := h.(*goals.Hook); ok {
			g.Models = llmtest.Factory{M: r.model}
		}
	}

	// Act
	r.process(run)

	// Assert
	ev := goalEvent(t, r.events(run.ID))
	next, err := r.st.GetRun(context.Background(), ev["nextRunId"].(string))
	if err != nil || next.Kind != store.KindGoal || next.Status != store.StatusQueued || ev["text"] != "Now translate the report" {
		t.Fatalf("event %v next %+v %v", ev, next, err)
	}
	g := r.goal(th.ID)
	msgs := r.messages(th.ID)
	if g.ID != replaced.ID || g.TurnsUsed != 1 || msgs[len(msgs)-1].Content != goals.StartMessage("Now translate the report") {
		t.Fatalf("goal %+v last message %+v", g, msgs[len(msgs)-1])
	}
}

func TestProcessGoalReplacedAfterTheVerdictGetsItsOwnRun(t *testing.T) {
	// A goal replaced after the old goal's verdict was recorded (still before the run ended) also gets its goal run.
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: llmtest.Text("Report done.")},
		llmtest.Turn{Chunks: llmtest.Text(`{"met": true, "reason": "Done."}`)},
	)
	th, _, run := r.goalRun(3)
	r.opts.Hooks = append(r.opts.Hooks, hookFunc(func(ctx context.Context, f *runtime.Finish) {
		_, _ = r.st.PutGoal(ctx, "alice", th.ID, "Now translate the report", 3)
	}))

	// Act
	r.process(run)

	// Assert
	g := r.goal(th.ID)
	msgs := r.messages(th.ID)
	last := msgs[len(msgs)-1]
	next, err := r.st.GetRun(context.Background(), last.RunID)
	if g.Text != "Now translate the report" || g.TurnsUsed != 1 || err != nil || next.Kind != store.KindGoal || next.Status != store.StatusQueued {
		t.Fatalf("goal %+v next %+v %v", g, next, err)
	}
}

func TestProcessGoalJudgeIsBoundedInTime(t *testing.T) {
	// A judge that does not answer within its time budget counts as a failed evaluation; the run is not held longer.
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: llmtest.Text("working")},
		llmtest.Turn{Block: true},
	)
	for _, h := range r.opts.Hooks {
		if g, ok := h.(*goals.Hook); ok {
			g.Timeout = 50 * time.Millisecond
		}
	}
	th, _, run := r.goalRun(3)
	start := time.Now()

	// Act
	got := r.process(run)

	// Assert
	g := r.goal(th.ID)
	if elapsed := time.Since(start); got.Status != store.StatusCompleted || elapsed > time.Second {
		t.Fatalf("status %s after %v", got.Status, elapsed)
	}
	if g.LastVerdict == nil || g.LastVerdict.Reason != goals.EvaluationFailed || len(r.model.Calls()) != 2 {
		t.Fatalf("verdict %+v calls %d", g.LastVerdict, len(r.model.Calls()))
	}
}

// recordingGoalStore records the runs the goal hook creates.
type recordingGoalStore struct {
	goals.Store
	created []store.NewRun
}

func (s *recordingGoalStore) CreateRun(ctx context.Context, in store.NewRun) (*store.Run, *store.Message, error) {
	s.created = append(s.created, in)
	return s.Store.CreateRun(ctx, in)
}

func TestProcessGoalContinuationKeepsTheRunsInteractionMode(t *testing.T) {
	// The next goal run inherits the interaction mode of the judged run (an interactive goal must keep asking for approvals).
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: llmtest.Text("I loaded the data.")},
		llmtest.Turn{Chunks: llmtest.Text(`{"met": false, "reason": "The PDF does not exist yet."}`)},
	)
	rec := &recordingGoalStore{Store: r.st}
	r.opts.Hooks = []runtime.Hook{&goals.Hook{Store: rec, Models: llmtest.Factory{M: r.model}, Queue: r.q}}
	ctx := context.Background()
	th, _ := r.st.CreateThread(ctx, "alice", "t")
	g, _ := r.st.PutGoal(ctx, "alice", th.ID, goalText, 3)
	run, _, err := r.st.CreateRun(ctx, goals.NewRun(g, "alice", "gpt-test", "", store.InteractionInteractive, goals.StartMessage(g.Text)))
	if err != nil {
		t.Fatal(err)
	}

	// Act
	r.process(run)

	// Assert
	if len(rec.created) != 1 {
		t.Fatalf("continuation runs %+v", rec.created)
	}
	next, err := r.st.GetRun(ctx, rec.created[0].RunID)
	if err != nil || next.InteractionMode != store.InteractionInteractive {
		t.Fatalf("continuation run %+v %v", next, err)
	}
}

func TestProcessGoalContinuationFollowsAModeSwitchedDuringTheRun(t *testing.T) {
	// A mode switched while a goal run is active is kept by the next goal run (and the thread is not switched back).
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "s1", "switch_mode", `{}`)}},
		llmtest.Turn{Chunks: llmtest.Text("I loaded the data.")},
		llmtest.Turn{Chunks: llmtest.Text(`{"met": false, "reason": "The PDF does not exist yet."}`)},
	)
	rec := &recordingGoalStore{Store: r.st}
	r.opts.Hooks = []runtime.Hook{&goals.Hook{Store: rec, Models: llmtest.Factory{M: r.model}, Queue: r.q}}
	ctx := context.Background()
	th, _ := r.st.CreateThread(ctx, "alice", "t")
	g, _ := r.st.PutGoal(ctx, "alice", th.ID, goalText, 3)
	run, _, err := r.st.CreateRun(ctx, goals.NewRun(g, "alice", "gpt-test", "", store.InteractionInteractive, goals.StartMessage(g.Text)))
	if err != nil {
		t.Fatal(err)
	}
	addTools(t, r, switchModeTool(t, r, th, store.InteractionAutomatic))

	// Act
	r.process(run)

	// Assert
	if len(rec.created) != 1 {
		t.Fatalf("continuation runs %+v", rec.created)
	}
	next, err := r.st.GetRun(ctx, rec.created[0].RunID)
	if err != nil || next.InteractionMode != store.InteractionAutomatic || r.thread(th.ID).InteractionMode != store.InteractionAutomatic {
		t.Fatalf("continuation run %+v %v thread mode %q", next, err, r.thread(th.ID).InteractionMode)
	}
}

func TestProcessGoalHandsOffBeforeEnqueueingTheNextRun(t *testing.T) {
	// The hand-off (web search token) of the next goal run happens before that run is enqueued, so a runtime picking it up already finds the token.
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: llmtest.Text("I loaded the data.")},
		llmtest.Turn{Chunks: llmtest.Text(`{"met": false, "reason": "The PDF does not exist yet."}`)},
	)
	var queuedAtHandoff int64 = -1
	var handed [2]string
	r.opts.Hooks = []runtime.Hook{&goals.Hook{Store: r.st, Models: llmtest.Factory{M: r.model}, Queue: r.q,
		Handoff: func(ctx context.Context, prev, next *store.Run) {
			queuedAtHandoff, _ = r.rdb.XLen(ctx, "work:runs").Result()
			handed = [2]string{prev.ID, next.ID}
		}}}
	_, _, run := r.goalRun(3)

	// Act
	r.process(run)

	// Assert
	ev := goalEvent(t, r.events(run.ID))
	if queuedAtHandoff != 0 || handed[0] != run.ID || handed[1] != ev["nextRunId"] {
		t.Fatalf("queued at hand-off %d, handed %v, next %v", queuedAtHandoff, handed, ev["nextRunId"])
	}
}
