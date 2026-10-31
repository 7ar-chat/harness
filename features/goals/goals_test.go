package goals_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/engine/eventlog"
	"github.com/7ar-chat/harness/engine/queue"
	"github.com/7ar-chat/harness/engine/runtime"
	"github.com/7ar-chat/harness/engine/tools"
	"github.com/7ar-chat/harness/features/goals"
)

const goalText = "Produce /workspace/outputs/report.pdf with a sales chart"

// spyStore is the hook's view of the store: calls can be made to fail.
type spyStore struct {
	*memstore.Store
	goalErr, updateErr, listErr, createErr error
}

func (s *spyStore) ThreadGoal(ctx context.Context, id string) (*store.Goal, error) {
	if s.goalErr != nil {
		return nil, s.goalErr
	}
	return s.Store.ThreadGoal(ctx, id)
}

func (s *spyStore) UpdateGoal(ctx context.Context, id string, u store.GoalUpdate) (*store.Goal, error) {
	if s.updateErr != nil {
		return nil, s.updateErr
	}
	return s.Store.UpdateGoal(ctx, id, u)
}

func (s *spyStore) ListMessages(ctx context.Context, id string) ([]*store.Message, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.Store.ListMessages(ctx, id)
}

func (s *spyStore) CreateRun(ctx context.Context, in store.NewRun) (*store.Run, *store.Message, error) {
	if s.createErr != nil {
		return nil, nil, s.createErr
	}
	return s.Store.CreateRun(ctx, in)
}

// recQueue records the runs the hook enqueues.
type recQueue struct {
	mu  sync.Mutex
	ids []string
	err error
}

func (q *recQueue) Enqueue(_ context.Context, id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.ids = append(q.ids, id)
	return q.err
}

func (q *recQueue) enqueued() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.ids...)
}

// scripted is the run executor: every run stores the same assistant message and returns res / err.
type scripted struct {
	st     *memstore.Store
	answer store.Message
	res    agent.Result
	err    error
}

func (e *scripted) Run(ctx context.Context, in agent.Input, _ eventlog.Writer) (agent.Result, error) {
	if e.err != nil {
		return agent.Result{}, e.err
	}
	m := e.answer
	m.ThreadID, m.RunID, m.Role = in.Run.ThreadID, in.Run.ID, store.RoleAssistant
	return e.res, e.st.AppendMessages(ctx, &m)
}

type rig struct {
	t     *testing.T
	mem   *memstore.Store
	st    *spyStore
	model *llmtest.Model
	hook  *goals.Hook
	q     *recQueue
	exec  *scripted
	rt    *runtime.Runtime
}

func newRig(t *testing.T, turns ...llmtest.Turn) *rig {
	t.Helper()
	mem := memstore.New()
	r := &rig{t: t, mem: mem, st: &spyStore{Store: mem}, model: llmtest.New(turns...), q: &recQueue{}}
	r.exec = &scripted{st: mem, answer: store.Message{Content: "I wrote the report."}}
	r.hook = &goals.Hook{Store: r.st, Models: llmtest.Factory{M: r.model}, Queue: r.q}
	mq := queue.NewMemory()
	r.rt = runtime.New(runtime.Options{ID: "rt", Hooks: []runtime.Hook{r.hook}}, mem, eventlog.NewMemory(mem), mq, mq, r.exec, nil)
	return r
}

// goalRun sets a goal on a new thread and creates its first goal run, like PUT /threads/{id}/goal.
func (r *rig) goalRun(maxTurns int) (*store.Thread, *store.Goal, *store.Run) {
	r.t.Helper()
	ctx := context.Background()
	th, err := r.mem.CreateThread(ctx, "alice", "t")
	if err != nil {
		r.t.Fatal(err)
	}
	g, err := r.mem.PutGoal(ctx, "alice", th.ID, goalText, maxTurns)
	if err != nil {
		r.t.Fatal(err)
	}
	run, _, err := r.mem.CreateRun(ctx, goals.NewRun(g, "alice", "gpt-test", "", "", goals.StartMessage(g.Text)))
	if err != nil {
		r.t.Fatal(err)
	}
	return th, g, run
}

func (r *rig) process(runID string) {
	r.t.Helper()
	if err := r.rt.Process(context.Background(), runID); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) goal(threadID string) *store.Goal {
	r.t.Helper()
	g, err := r.mem.ThreadGoal(context.Background(), threadID)
	if err != nil || g == nil {
		r.t.Fatalf("goal %v %v", g, err)
	}
	return g
}

func (r *rig) events(runID string) []map[string]any {
	r.t.Helper()
	evs, err := r.mem.ListEvents(context.Background(), runID, 0, 1000)
	if err != nil {
		r.t.Fatal(err)
	}
	var out []map[string]any
	for _, e := range evs {
		var m map[string]any
		if err := json.Unmarshal(e.Data, &m); err != nil {
			r.t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// goalEvent is the value of the run's work.goal event (nil when it has none).
func (r *rig) goalEvent(runID string) map[string]any {
	r.t.Helper()
	var found map[string]any
	for _, e := range r.events(runID) {
		if e["type"] == "CUSTOM" && e["name"] == agui.CustomGoal {
			found = e["value"].(map[string]any)
		}
	}
	return found
}

func (r *rig) lastMessage(threadID string) *store.Message {
	r.t.Helper()
	msgs, _ := r.mem.ListMessages(context.Background(), threadID)
	return msgs[len(msgs)-1]
}

func verdictJSON(met bool, reason string) llmtest.Turn {
	return llmtest.Turn{Chunks: llmtest.Text(fmt.Sprintf(`{"met": %v, "reason": %q}`, met, reason))}
}

func TestContinueMessageNamesTheLastCheckAndWhatIsMissing(t *testing.T) {
	// Act
	plain := goals.ContinueMessage("No chart yet.")
	withBlockers := goals.ContinueMessage("No chart yet.", "chart missing", "title wrong")

	// Assert
	if plain != "Continue working toward the goal. Last check: No chart yet." {
		t.Errorf("plain = %q", plain)
	}
	if want := "Continue working toward the goal. Last check: No chart yet.\nStill missing:\n- chart missing\n- title wrong"; withBlockers != want {
		t.Errorf("with blockers = %q", withBlockers)
	}
	if goals.StartMessage("ship it") != "Work toward the goal: ship it" {
		t.Errorf("start = %q", goals.StartMessage("ship it"))
	}
}

func TestNewRunClaimsATurnOfTheGoalAndIsMarkedAsAGoalMessage(t *testing.T) {
	// Arrange
	g := &store.Goal{ID: "g1", ThreadID: "t1"}
	content := "Work   toward\nthe goal: " + strings.Repeat("é ", 60)

	// Act
	in := goals.NewRun(g, "alice", "gpt-test", "high", store.InteractionInteractive, content)

	// Assert
	if in.Kind != store.KindGoal || in.GoalID != "g1" || in.ThreadID != "t1" || in.UserLogin != "alice" || in.Model != "gpt-test" ||
		in.ReasoningEffort != "high" || in.InteractionMode != store.InteractionInteractive || in.RunID == "" {
		t.Fatalf("run = %+v", in)
	}
	if in.Message.Role != store.RoleUser || in.Message.Content != content || in.Message.Metadata["source"] != "goal" {
		t.Fatalf("message = %+v", in.Message)
	}
	if n := len([]rune(in.AutoTitle)); n != 60 || !strings.HasSuffix(in.AutoTitle, "…") || strings.ContainsAny(in.AutoTitle, "\n") || strings.Contains(in.AutoTitle, "  ") {
		t.Fatalf("auto title = %q (%d runes)", in.AutoTitle, n)
	}
	if other := goals.NewRun(g, "alice", "m", "", "", "x"); other.RunID == in.RunID || other.AutoTitle != "x" {
		t.Fatalf("a second run reuses the id or title: %+v", other)
	}
}

func TestTheGoalIsInThePromptOnlyWhileItIsActive(t *testing.T) {
	// Arrange
	r := newRig(t)
	th, g, run := r.goalRun(5)
	in := agent.Input{Run: &store.Run{ID: run.ID, ThreadID: th.ID}}
	ctxSection, turns := goals.ContextSection(r.st), goals.TurnsSection(r.st)
	ctx := t.Context()

	// Act
	activeCtx, activeTurns := ctxSection(ctx, in), turns(ctx, in)
	_ = r.mem.ClearGoal(ctx, "alice", th.ID)
	clearedCtx, clearedTurns := ctxSection(ctx, in), turns(ctx, in)

	// Assert
	if !strings.Contains(activeCtx, "Goal (checked automatically after each of your answers): "+g.Text) {
		t.Errorf("context = %q", activeCtx)
	}
	if activeTurns != "Goal: 1 of 5 automatic goal turns used." {
		t.Errorf("turns = %q", activeTurns)
	}
	if clearedCtx != "" || clearedTurns != "" {
		t.Errorf("a cleared goal is still in the prompt: %q %q", clearedCtx, clearedTurns)
	}
}

func TestThePromptSectionsAreEmptyWithoutAGoalOrWhenItCannotBeRead(t *testing.T) {
	// Arrange
	r := newRig(t)
	th, _ := r.mem.CreateThread(t.Context(), "alice", "t")
	in := agent.Input{Run: &store.Run{ID: "r", ThreadID: th.ID}}

	// Act
	noGoal := goals.ContextSection(r.st)(t.Context(), in)
	r.st.goalErr = errors.New("db down")
	unreadable := goals.TurnsSection(r.st)(t.Context(), in)

	// Assert
	if noGoal != "" || unreadable != "" {
		t.Fatalf("no goal %q unreadable %q", noGoal, unreadable)
	}
}

func TestEventDescribesTheGoalItsVerdictAndTheNextRun(t *testing.T) {
	// Arrange
	created := mustTime(t, "2026-10-05T12:00:00Z")
	updated := mustTime(t, "2026-10-05T12:30:00+02:00")
	g := &store.Goal{Text: "t", Status: store.GoalActive, TurnsUsed: 2, MaxTurns: 8, CreatedAt: created, UpdatedAt: updated}

	// Act
	bare := goals.Event(g, "")
	g.LastVerdict = &store.GoalVerdict{Met: false, Reason: "r", At: updated}
	judged := goals.Event(g, "run-9")

	// Assert
	if _, ok := bare["lastVerdict"]; ok || bare["nextRunId"] != nil || bare["turnsUsed"] != 2 || bare["maxTurns"] != 8 ||
		bare["createdAt"] != "2026-10-05T12:00:00Z" || bare["updatedAt"] != "2026-10-05T10:30:00Z" {
		t.Fatalf("bare = %v", bare)
	}
	lv := judged["lastVerdict"].(map[string]any)
	if judged["nextRunId"] != "run-9" || lv["met"] != false || lv["reason"] != "r" || lv["at"] != "2026-10-05T10:30:00Z" {
		t.Fatalf("judged = %v", judged)
	}
	if b, ok := lv["blockers"].([]string); !ok || b == nil || len(b) != 0 {
		t.Fatalf("a verdict without blockers must announce an empty list, got %#v", lv["blockers"])
	}
}

func TestAnUnmetGoalWithTurnsLeftStartsTheNextGoalRun(t *testing.T) {
	// Arrange
	r := newRig(t, llmtest.Turn{Chunks: llmtest.Text(`{"met": false, "reason": "The PDF is missing.", "blockers": ["no pdf", "no chart"]}`)})
	var handed [2]string
	r.hook.Handoff = func(_ context.Context, prev, next *store.Run) { handed = [2]string{prev.ID, next.ID} }
	th, g, run := r.goalRun(3)

	// Act
	r.process(run.ID)

	// Assert
	ev := r.goalEvent(run.ID)
	next := ev["nextRunId"].(string)
	if got := r.goal(th.ID); got.Status != store.GoalActive || got.TurnsUsed != 2 || got.ID != g.ID || got.LastVerdict.Reason != "The PDF is missing." ||
		strings.Join(got.LastVerdict.Blockers, "|") != "no pdf|no chart" || got.EvalFailures != 0 {
		t.Fatalf("goal = %+v verdict %+v", got, got.LastVerdict)
	}
	if ev["turnsUsed"] != float64(2) || ev["status"] != "active" {
		t.Fatalf("event = %v", ev)
	}
	if handed != [2]string{run.ID, next} {
		t.Fatalf("handoff %v, want prev %s next %s", handed, run.ID, next)
	}
	if ids := r.q.enqueued(); len(ids) != 1 || ids[0] != next {
		t.Fatalf("enqueued %v", ids)
	}
	nr, err := r.mem.GetRun(context.Background(), next)
	if err != nil || nr.Kind != store.KindGoal || nr.Status != store.StatusQueued || nr.UserLogin != "alice" || nr.Model != "gpt-test" {
		t.Fatalf("next run %+v %v", nr, err)
	}
	m := r.lastMessage(th.ID)
	if want := "Continue working toward the goal. Last check: The PDF is missing.\nStill missing:\n- no pdf\n- no chart"; m.RunID != next || m.Content != want || m.Metadata["source"] != "goal" {
		t.Fatalf("message %+v", m)
	}
}

func TestAMetGoalStopsTheLoop(t *testing.T) {
	// Arrange
	r := newRig(t, verdictJSON(true, "The PDF with the chart exists."))
	th, _, run := r.goalRun(3)

	// Act
	r.process(run.ID)

	// Assert
	ev := r.goalEvent(run.ID)
	got := r.goal(th.ID)
	if got.Status != store.GoalMet || !got.LastVerdict.Met || ev["status"] != "met" || ev["nextRunId"] != nil || len(r.q.enqueued()) != 0 {
		t.Fatalf("goal %+v event %v enqueued %v", got, ev, r.q.enqueued())
	}
}

func TestAnUnmetGoalWithoutTurnsLeftIsExhausted(t *testing.T) {
	// Arrange
	r := newRig(t, verdictJSON(false, "Still no chart."))
	th, _, run := r.goalRun(1)

	// Act
	r.process(run.ID)

	// Assert
	ev := r.goalEvent(run.ID)
	if got := r.goal(th.ID); got.Status != store.GoalExhausted || ev["status"] != "exhausted" || ev["nextRunId"] != nil || len(r.q.enqueued()) != 0 {
		t.Fatalf("goal %+v event %v", got, ev)
	}
}

func TestTheVerdictIsReadFromWhateverSurroundsTheJSON(t *testing.T) {
	long := strings.Repeat("r", 1200)
	var twelve []string
	for i := 0; i < 12; i++ {
		twelve = append(twelve, fmt.Sprintf("%q", fmt.Sprintf("b%d", i)))
	}
	cases := []struct {
		name, answer string
		met          bool
		reason       string
		blockers     string
	}{
		{"code fence", "```json\n{\"met\": true, \"reason\": \"done\"}\n```", true, "done", ""},
		{"braces before the object", `I think {maybe} so: {"met": false, "reason": "no pdf", "blockers": ["a", " b ", "", "c"]}`, false, "no pdf", "a|b|c"},
		{"met as the string true", `{"met": "true", "reason": "ok"}`, true, "ok", ""},
		{"met as the string false", `{"met": "false", "reason": "nope"}`, false, "nope", ""},
		{"an object whose met is not a boolean is skipped", `{"met": "yes", "reason": "x"} then {"met": true, "reason": "y"}`, true, "y", ""},
		{"missing reason", `{"met": true}`, true, "No reason given.", ""},
		{"blank reason", `{"met": false, "reason": "  \n "}`, false, "No reason given.", ""},
		{"reason whitespace collapsed", `{"met": false, "reason": "  a \n\n b\t c "}`, false, "a b c", ""},
		{"reason cut at 1000 characters", fmt.Sprintf(`{"met": false, "reason": %q}`, long), false, strings.Repeat("r", 1000) + "…", ""},
		{"at most 10 blockers", `{"met": false, "reason": "x", "blockers": [` + strings.Join(twelve, ",") + `]}`, false, "x", "b0|b1|b2|b3|b4|b5|b6|b7|b8|b9"},
		{"a blocker is cut at 300 characters", fmt.Sprintf(`{"met": false, "reason": "x", "blockers": [%q]}`, strings.Repeat("k", 400)), false, "x", strings.Repeat("k", 300) + "…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			r := newRig(t, llmtest.Turn{Chunks: llmtest.Text(tc.answer)})
			th, _, run := r.goalRun(5)

			// Act
			r.process(run.ID)

			// Assert
			v := r.goal(th.ID).LastVerdict
			if v == nil || v.Met != tc.met || v.Reason != tc.reason || strings.Join(v.Blockers, "|") != tc.blockers {
				t.Fatalf("verdict = %+v", v)
			}
			if len(r.model.Calls()) != 1 {
				t.Fatalf("%d judge calls; a parsable answer must not be asked again", len(r.model.Calls()))
			}
		})
	}
}

func TestAnAnswerThatIsNotAVerdictIsAskedAgainOnce(t *testing.T) {
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: llmtest.Text("Yes, it looks complete.")},
		llmtest.Turn{Chunks: llmtest.Text(`{"met": true, "reason": "Complete."}`)},
	)
	th, _, run := r.goalRun(3)

	// Act
	r.process(run.ID)

	// Assert
	calls := r.model.Calls()
	if len(calls) != 2 {
		t.Fatalf("%d judge calls", len(calls))
	}
	retry := calls[1].Messages
	if len(retry) != 4 || retry[2].Role != ai.RoleAssistant || retry[2].Content != "Yes, it looks complete." || !strings.Contains(retry[3].Content, "That was not valid JSON") {
		t.Fatalf("retry conversation %+v", retry)
	}
	if got := r.goal(th.ID); got.Status != store.GoalMet || got.EvalFailures != 0 {
		t.Fatalf("goal %+v", got)
	}
}

func TestTwoUnusableAnswersCountAsAFailedEvaluationThatStillContinues(t *testing.T) {
	// Arrange
	r := newRig(t, llmtest.Turn{Chunks: llmtest.Text("hmm")}, llmtest.Turn{Chunks: llmtest.Text("{\"met\": 3}")})
	th, _, run := r.goalRun(3)

	// Act
	r.process(run.ID)

	// Assert
	got := r.goal(th.ID)
	ev := r.goalEvent(run.ID)
	if got.Status != store.GoalActive || got.EvalFailures != 1 || got.LastVerdict.Reason != goals.EvaluationFailed || got.LastVerdict.Met || ev["nextRunId"] == nil {
		t.Fatalf("goal %+v verdict %+v event %v", got, got.LastVerdict, ev)
	}
}

func TestTwoFailedEvaluationsInARowStopTheAutomaticContinuation(t *testing.T) {
	// Arrange
	r := newRig(t, llmtest.Turn{Err: errors.New("bad gateway")}, llmtest.Turn{Err: errors.New("bad gateway")},
		llmtest.Turn{Err: errors.New("bad gateway")}, llmtest.Turn{Err: errors.New("bad gateway")})
	th, _, first := r.goalRun(5)
	r.process(first.ID)
	second := r.goalEvent(first.ID)["nextRunId"].(string)

	// Act
	r.process(second)

	// Assert
	got := r.goal(th.ID)
	ev := r.goalEvent(second)
	if got.Status != store.GoalActive || got.EvalFailures != goals.MaxEvalFailures || ev["nextRunId"] != nil || len(r.q.enqueued()) != 1 {
		t.Fatalf("goal %+v event %v enqueued %v", got, ev, r.q.enqueued())
	}
}

func TestASuccessfulEvaluationResetsTheFailureCount(t *testing.T) {
	// Arrange
	r := newRig(t, llmtest.Turn{Err: errors.New("down")}, llmtest.Turn{Err: errors.New("down")}, verdictJSON(false, "Not yet."))
	th, _, first := r.goalRun(5)
	r.process(first.ID)
	second := r.goalEvent(first.ID)["nextRunId"].(string)

	// Act
	r.process(second)

	// Assert
	if got := r.goal(th.ID); got.EvalFailures != 0 || got.LastVerdict.Reason != "Not yet." || r.goalEvent(second)["nextRunId"] == nil {
		t.Fatalf("goal %+v", got)
	}
}

func TestAFailedEvaluationOnTheLastTurnExhaustsTheGoal(t *testing.T) {
	// Arrange
	r := newRig(t, llmtest.Turn{Err: errors.New("down")}, llmtest.Turn{Err: errors.New("down")})
	th, _, run := r.goalRun(1)

	// Act
	r.process(run.ID)

	// Assert
	got := r.goal(th.ID)
	if got.Status != store.GoalExhausted || got.LastVerdict.Reason != goals.EvaluationFailed || r.goalEvent(run.ID)["nextRunId"] != nil {
		t.Fatalf("goal %+v", got)
	}
}

func TestAnUnavailableJudgeModelOrUnreadableThreadIsAFailedEvaluation(t *testing.T) {
	cases := []struct {
		name  string
		setup func(r *rig)
	}{
		{"model unavailable", func(r *rig) { r.hook.Models = failingFactory{} }},
		{"messages unreadable", func(r *rig) { r.st.listErr = errors.New("db down") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			r := newRig(t, verdictJSON(true, "would be met"))
			tc.setup(r)
			th, _, run := r.goalRun(3)

			// Act
			r.process(run.ID)

			// Assert
			got := r.goal(th.ID)
			if got.Status != store.GoalActive || got.EvalFailures != 1 || got.LastVerdict.Reason != goals.EvaluationFailed || len(r.model.Calls()) != 0 {
				t.Fatalf("goal %+v calls %d", got, len(r.model.Calls()))
			}
		})
	}
}

type failingFactory struct{}

func (failingFactory) Model(context.Context, string) (ai.Model, error) {
	return nil, errors.New("no such model")
}

func TestAJudgeThatOutlastsTheTimeoutIsAFailedEvaluation(t *testing.T) {
	// Arrange
	r := newRig(t, llmtest.Turn{Block: true})
	r.hook.Timeout = 30_000_000 // 30 ms
	th, _, run := r.goalRun(3)

	// Act
	r.process(run.ID)

	// Assert
	got := r.goal(th.ID)
	if got.EvalFailures != 1 || got.LastVerdict.Reason != goals.EvaluationFailed || len(r.model.Calls()) != 1 {
		t.Fatalf("goal %+v calls %d (a timed out judge must not be retried)", got, len(r.model.Calls()))
	}
}

func TestRunsThatAreNotCompletedAnswersAreNotJudged(t *testing.T) {
	cases := []struct {
		name  string
		setup func(e *scripted)
	}{
		{"failed run", func(e *scripted) { e.err = &agent.Error{Code: agui.CodeInternal, Message: "boom"} }},
		{"run waiting for an ask_user answer", func(e *scripted) {
			e.res = agent.Result{Interrupt: &tools.Interrupt{Reason: "ask_user", Question: "Which chart?", ID: "i1", ToolCallID: "c1"}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			r := newRig(t, verdictJSON(true, "would be met"))
			tc.setup(r.exec)
			th, _, run := r.goalRun(3)

			// Act
			r.process(run.ID)

			// Assert
			got := r.goal(th.ID)
			if len(r.model.Calls()) != 0 || got.LastVerdict != nil || got.Status != store.GoalActive || r.goalEvent(run.ID) != nil || len(r.q.enqueued()) != 0 {
				t.Fatalf("goal %+v calls %d event %v", got, len(r.model.Calls()), r.goalEvent(run.ID))
			}
		})
	}
}

func TestACompactionRunOfAGoalThreadIsNotJudged(t *testing.T) {
	// Arrange
	r := newRig(t, verdictJSON(true, "would be met"))
	th, _, goalRun := r.goalRun(3)
	if _, err := r.mem.ClaimRun(t.Context(), goalRun.ID, "x"); err != nil {
		t.Fatal(err)
	}
	if err := r.mem.FinishRun(t.Context(), goalRun.ID, "x", store.StatusCancelled, "", ""); err != nil {
		t.Fatal(err)
	}
	run, _, err := r.mem.CreateRun(t.Context(), store.NewRun{RunID: "compact-1", ThreadID: th.ID, UserLogin: "alice", Model: "m", Kind: store.KindCompact,
		Message: store.Message{Role: store.RoleUser, Content: "compact"}, AutoTitle: "compact"})
	if err != nil {
		t.Fatal(err)
	}

	// Act
	r.process(run.ID)

	// Assert
	if len(r.model.Calls()) != 0 || r.goal(th.ID).LastVerdict != nil {
		t.Fatalf("calls %d goal %+v", len(r.model.Calls()), r.goal(th.ID))
	}
}

func TestAThreadWithoutAnActiveGoalIsNotJudged(t *testing.T) {
	// Arrange
	r := newRig(t, verdictJSON(true, "x"))
	ctx := t.Context()
	th, _ := r.mem.CreateThread(ctx, "alice", "t")
	run, _, _ := r.mem.CreateRun(ctx, store.NewRun{RunID: "plain", ThreadID: th.ID, UserLogin: "alice", Model: "m",
		Message: store.Message{Role: store.RoleUser, Content: "hi"}, AutoTitle: "hi"})

	// Act
	r.process(run.ID)

	// Assert
	if len(r.model.Calls()) != 0 || r.goalEvent(run.ID) != nil {
		t.Fatalf("calls %d event %v", len(r.model.Calls()), r.goalEvent(run.ID))
	}
}

func TestAGoalThatCannotBeReadOrRecordedLeavesTheRunAlone(t *testing.T) {
	cases := []struct {
		name  string
		setup func(r *rig)
		calls int
	}{
		{"goal unreadable", func(r *rig) { r.st.goalErr = errors.New("db down") }, 0},
		{"verdict not recorded", func(r *rig) { r.st.updateErr = errors.New("db down") }, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			r := newRig(t, verdictJSON(false, "not yet"))
			tc.setup(r)
			th, _, run := r.goalRun(3)

			// Act
			r.process(run.ID)

			// Assert
			got := r.goal(th.ID)
			if got.LastVerdict != nil || r.goalEvent(run.ID) != nil || len(r.q.enqueued()) != 0 || len(r.model.Calls()) != tc.calls {
				t.Fatalf("goal %+v event %v calls %d", got, r.goalEvent(run.ID), len(r.model.Calls()))
			}
		})
	}
}

func TestANextRunThatCannotBeStartedIsNotAnnounced(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"another run is active", store.ErrRunActive},
		{"goal no longer active", store.ErrGoalInactive},
		{"question pending", store.ErrQuestionPending},
		{"approval pending", store.ErrApprovalPending},
		{"store failure", errors.New("db down")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			r := newRig(t, verdictJSON(false, "not yet"))
			r.st.createErr = tc.err
			th, _, run := r.goalRun(5)

			// Act
			r.process(run.ID)

			// Assert
			ev := r.goalEvent(run.ID)
			if got := r.goal(th.ID); got.Status != store.GoalActive || got.TurnsUsed != 1 || ev == nil || ev["nextRunId"] != nil || len(r.q.enqueued()) != 0 {
				t.Fatalf("goal %+v event %v", got, ev)
			}
		})
	}
}

func TestAFailedEnqueueStillAnnouncesTheRunTheSweeperWillPick(t *testing.T) {
	// Arrange
	r := newRig(t, verdictJSON(false, "not yet"))
	r.q.err = errors.New("redis down")
	_, _, run := r.goalRun(5)

	// Act
	r.process(run.ID)

	// Assert
	next, _ := r.goalEvent(run.ID)["nextRunId"].(string)
	if ids := r.q.enqueued(); next == "" || len(ids) != 1 || ids[0] != next {
		t.Fatalf("next %q enqueued %v", next, ids)
	}
	if got, err := r.mem.GetRun(t.Context(), next); err != nil || got.Status != store.StatusQueued {
		t.Fatalf("run %+v %v", got, err)
	}
}

func TestAGoalReplacedWhileJudgedStartsTheNewGoalAndKeepsItsOwnVerdictOut(t *testing.T) {
	// A PUT /goal that lands while the old goal is being judged: the verdict belongs to the old goal and is dropped, the new goal gets its first run.
	// Arrange
	r := newRig(t, verdictJSON(true, "the old goal is met"))
	var threadID string
	r.hook.Evidence = func(context.Context, *store.Run, *store.Goal) []goals.Evidence {
		if _, err := r.mem.PutGoal(t.Context(), "alice", threadID, "second goal", 4); err != nil {
			t.Error(err)
		}
		return nil
	}
	th, old, run := r.goalRun(3)
	threadID = th.ID

	// Act
	r.process(run.ID)

	// Assert
	got := r.goal(th.ID)
	ev := r.goalEvent(run.ID)
	next, _ := ev["nextRunId"].(string)
	if got.ID == old.ID || got.Text != "second goal" || got.Status != store.GoalActive || got.TurnsUsed != 1 || got.LastVerdict != nil {
		t.Fatalf("goal %+v", got)
	}
	if ev["text"] != "second goal" || ev["status"] != "active" || next == "" {
		t.Fatalf("event %v", ev)
	}
	if m := r.lastMessage(th.ID); m.RunID != next || m.Content != "Work toward the goal: second goal" {
		t.Fatalf("message %+v", m)
	}
}

func TestAGoalClearedWhileJudgedGetsNoMoreRuns(t *testing.T) {
	// Arrange
	r := newRig(t, verdictJSON(false, "not yet"))
	var threadID string
	r.hook.Evidence = func(context.Context, *store.Run, *store.Goal) []goals.Evidence {
		_ = r.mem.ClearGoal(t.Context(), "alice", threadID)
		return nil
	}
	th, _, run := r.goalRun(5)
	threadID = th.ID

	// Act
	r.process(run.ID)

	// Assert
	ev := r.goalEvent(run.ID)
	if got := r.goal(th.ID); got.Status != store.GoalCleared || got.LastVerdict != nil || ev["status"] != "cleared" || ev["nextRunId"] != nil || len(r.q.enqueued()) != 0 {
		t.Fatalf("goal %+v event %v", got, ev)
	}
}

func TestTheJudgeUsageJoinsTheRunsUsageAndTheVerdictIsStampedWithTheHooksClock(t *testing.T) {
	// Arrange
	reply := llmtest.WithUsage(llmtest.Text(`{"met": true, "reason": "done"}`)[0], 40, 8)
	r := newRig(t, llmtest.Turn{Chunks: []*ai.Message{reply}})
	now := mustTime(t, "2026-10-05T12:00:00+02:00")
	r.hook.Now = func() time.Time { return now }
	th, _, run := r.goalRun(3)

	// Act
	r.process(run.ID)

	// Assert
	evs := r.events(run.ID)
	final := evs[len(evs)-1]
	usage := final["usage"].([]any)
	if final["type"] != "RUN_FINISHED" || len(usage) != 1 || usage[0].(map[string]any)["model"] != "gpt-test" || usage[0].(map[string]any)["inputTokens"] != float64(40) {
		t.Fatalf("final %v", final)
	}
	if at := r.goal(th.ID).LastVerdict.At; !at.Equal(now) || at.Location().String() != "UTC" {
		t.Fatalf("verdict time %v", at)
	}
}

func TestTheJudgeRunsAtLowEffortOnlyForReasoningModels(t *testing.T) {
	// Arrange
	r := newRig(t, verdictJSON(true, "ok"))
	r.hook.SupportsReasoning = func(m string) bool { return m == "gpt-test" }
	_, _, run := r.goalRun(3)

	// Act
	r.process(run.ID)

	// Assert
	if got := r.model.Calls()[0].ReasoningEffort; got != "low" {
		t.Fatalf("effort %q", got)
	}
}

func TestTheJudgeSeesTheGoalTheLatestAnswerTheToolsAndTheFiles(t *testing.T) {
	// Arrange
	r := newRig(t, verdictJSON(false, "not yet"))
	r.exec.answer = store.Message{Content: "I made the report.", ToolCalls: []store.ToolCall{
		{ID: "c1", Type: "function", Function: store.ToolCallFunction{Name: "write_file", Arguments: `{"path":"/workspace/outputs/report.md"}`}},
		{ID: "c2", Type: "function", Function: store.ToolCallFunction{Name: "edit_file", Arguments: `{"path":"/workspace/outputs/report.md"}`}},
		{ID: "c3", Type: "function", Function: store.ToolCallFunction{Name: "write_file", Arguments: `{"path":"/workspace/b.txt"}`}},
		{ID: "c4", Type: "function", Function: store.ToolCallFunction{Name: "bash", Arguments: `{"command":"ls"}`}},
		{ID: "c5", Type: "function", Function: store.ToolCallFunction{Name: "write_file", Arguments: `not json`}},
	}}
	r.hook.ListArtifacts = func(context.Context, string) ([]string, error) {
		return []string{"/workspace/old.txt", "/workspace/outputs/up.pdf"}, nil
	}
	th, _, run := r.goalRun(3)
	if err := r.mem.AppendMessages(t.Context(), &store.Message{ThreadID: th.ID, RunID: "older-run", Role: store.RoleAssistant, Content: "old answer",
		ToolCalls: []store.ToolCall{{ID: "o1", Function: store.ToolCallFunction{Name: "write_file", Arguments: `{"path":"/workspace/old.txt"}`}}}}); err != nil {
		t.Fatal(err)
	}

	// Act
	r.process(run.ID)

	// Assert
	prompt := r.model.Calls()[0].Messages[1].Content
	want := "GOAL:\n" + goalText + "\n\nLATEST ANSWER OF THE AGENT:\nI made the report.\n\nTOOLS USED IN THE LATEST TURN:\n" +
		"- write_file {\"path\":\"/workspace/outputs/report.md\"}\n- edit_file {\"path\":\"/workspace/outputs/report.md\"}\n" +
		"- write_file {\"path\":\"/workspace/b.txt\"}\n- bash {\"command\":\"ls\"}\n- write_file not json" +
		"\n\nFILES PRODUCED IN THIS CONVERSATION:\n- /workspace/old.txt\n- /workspace/outputs/report.md\n- /workspace/b.txt\n- /workspace/outputs/up.pdf"
	if prompt != want {
		t.Fatalf("prompt:\n%s\nwant:\n%s", prompt, want)
	}
	if sys := r.model.Calls()[0].Messages[0]; sys.Role != ai.RoleSystem || sys.Content != goals.JudgeInstructions {
		t.Fatalf("system message %+v", sys)
	}
}

func TestAnAnswerlessTurnIsShownAsSuch(t *testing.T) {
	// Arrange
	r := newRig(t, verdictJSON(false, "nothing"))
	r.exec.answer = store.Message{Content: "  "}
	_, _, run := r.goalRun(3)

	// Act
	r.process(run.ID)

	// Assert
	prompt := r.model.Calls()[0].Messages[1].Content
	for _, part := range []string{"LATEST ANSWER OF THE AGENT:\n(no text answer)", "TOOLS USED IN THE LATEST TURN:\n(none)", "FILES PRODUCED IN THIS CONVERSATION:\n(none)"} {
		if !strings.Contains(prompt, part) {
			t.Errorf("prompt lacks %q:\n%s", part, prompt)
		}
	}
}

func TestTheJudgePromptBoundsTheAnswerTheToolLinesAndTheFiles(t *testing.T) {
	// Arrange
	r := newRig(t, verdictJSON(false, "x"))
	var calls []store.ToolCall
	for i := 0; i < 45; i++ {
		calls = append(calls, store.ToolCall{ID: fmt.Sprintf("c%d", i), Function: store.ToolCallFunction{Name: "write_file", Arguments: fmt.Sprintf(`{"path":"/workspace/f%02d"}`, i)}})
	}
	r.exec.answer = store.Message{Content: strings.Repeat("a", 7000), ToolCalls: calls}
	_, _, run := r.goalRun(3)

	// Act
	r.process(run.ID)

	// Assert
	prompt := r.model.Calls()[0].Messages[1].Content
	if !strings.Contains(prompt, strings.Repeat("a", 6000)+"…") || strings.Contains(prompt, strings.Repeat("a", 6001)) {
		t.Errorf("the answer is not cut at 6000 characters")
	}
	tools, files, _ := strings.Cut(prompt, "FILES PRODUCED IN THIS CONVERSATION:\n")
	if n := strings.Count(tools, "- write_file"); n != 30 || !strings.Contains(tools, "f29") || strings.Contains(tools, "f30") {
		t.Errorf("%d tool lines, want the first 30", n)
	}
	if n := strings.Count(files, "- /workspace/"); n != 40 || strings.Contains(files, "f04") || !strings.Contains(files, "f05") || !strings.Contains(files, "f44") {
		t.Errorf("%d files, want the latest 40 (f05..f44)", n)
	}
}

func TestAFailingDeliverableCheckerBlocksAMetVerdict(t *testing.T) {
	// Arrange
	pass, fail := true, false
	r := newRig(t, verdictJSON(true, "Everything is there."))
	r.hook.Evidence = func(context.Context, *store.Run, *store.Goal) []goals.Evidence {
		return []goals.Evidence{
			{Path: "/workspace/outputs/a.docx", Checker: "inspect_docx", OK: &pass, Summary: "3 checks passed"},
			{Path: "/workspace/outputs/b.xlsx", Checker: "recalc", OK: &fail, Summary: "2 formula errors", Text: "=A1/0"},
			{Path: "/workspace/outputs/c.pptx", Checker: "fit_text", Summary: "stale"},
			{Path: "/workspace/outputs/d.txt"},
		}
	}
	th, _, run := r.goalRun(4)

	// Act
	r.process(run.ID)

	// Assert
	got := r.goal(th.ID)
	v := got.LastVerdict
	if v.Met || got.Status != store.GoalActive || v.Reason != "A deliverable's checker reports a failure: /workspace/outputs/b.xlsx (2 formula errors)" ||
		strings.Join(v.Blockers, "|") != "/workspace/outputs/b.xlsx: 2 formula errors" {
		t.Fatalf("goal %+v verdict %+v", got, v)
	}
	if r.goalEvent(run.ID)["nextRunId"] == nil {
		t.Fatal("a goal held back by a checker must get another run")
	}
	prompt := r.model.Calls()[0].Messages[1].Content
	for _, part := range []string{
		"DELIVERABLES OF THE GOAL'S TURNS",
		"\n- /workspace/outputs/a.docx — checker passed: 3 checks passed",
		"\n- /workspace/outputs/b.xlsx — CHECKER FAILED: 2 formula errors\n",
		"=A1/0",
		"deliverable_text",
		"\n- /workspace/outputs/c.pptx — not checked since its last change",
		"\n- /workspace/outputs/d.txt — no checker for this file type",
	} {
		if !strings.Contains(prompt, part) {
			t.Errorf("prompt lacks %q:\n%s", part, prompt)
		}
	}
	if strings.Contains(prompt, "stale") && strings.Contains(prompt, "c.pptx — checker") {
		t.Error("an unchecked deliverable is reported as checked")
	}
}

func TestAFailingCheckerAddsABlockerToAnUnmetVerdictWithoutChangingItsReason(t *testing.T) {
	// Arrange
	fail := false
	r := newRig(t, llmtest.Turn{Chunks: llmtest.Text(`{"met": false, "reason": "Chart missing.", "blockers": ["no chart"]}`)})
	r.hook.Evidence = func(context.Context, *store.Run, *store.Goal) []goals.Evidence {
		return []goals.Evidence{{Path: "/workspace/outputs/b.xlsx", OK: &fail, Summary: strings.Repeat("s", 400)}}
	}
	th, _, run := r.goalRun(4)

	// Act
	r.process(run.ID)

	// Assert
	v := r.goal(th.ID).LastVerdict
	if v.Reason != "Chart missing." || len(v.Blockers) != 2 || v.Blockers[0] != "no chart" || !strings.HasPrefix(v.Blockers[1], "/workspace/outputs/b.xlsx: sss") || !strings.HasSuffix(v.Blockers[1], "…") {
		t.Fatalf("verdict %+v", v)
	}
}

func TestPassingCheckersLeaveAMetVerdictMet(t *testing.T) {
	// Arrange
	pass := true
	r := newRig(t, verdictJSON(true, "Done."))
	r.hook.Evidence = func(context.Context, *store.Run, *store.Goal) []goals.Evidence {
		return []goals.Evidence{{Path: "/workspace/outputs/a.docx", Checker: "inspect_docx", OK: &pass, Summary: "ok"}}
	}
	th, _, run := r.goalRun(4)

	// Act
	r.process(run.ID)

	// Assert
	if got := r.goal(th.ID); got.Status != store.GoalMet || len(got.LastVerdict.Blockers) != 0 {
		t.Fatalf("goal %+v", got)
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
