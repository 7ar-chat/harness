package titles_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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
	"github.com/7ar-chat/harness/features/titles"
)

// answerer is the run executor of the rig: it stores one assistant answer and reports one model's usage.
type answerer struct {
	st     *memstore.Store
	answer string
	status error
}

func (a answerer) Run(ctx context.Context, in agent.Input, _ eventlog.Writer) (agent.Result, error) {
	if a.status != nil {
		return agent.Result{}, a.status
	}
	err := a.st.AppendMessages(ctx, &store.Message{ThreadID: in.Run.ThreadID, RunID: in.Run.ID, Role: store.RoleAssistant, Content: a.answer})
	return agent.Result{Usage: []agui.Usage{{Model: in.Run.Model, InputTokens: 100, OutputTokens: 10, TotalTokens: 110}}}, err
}

// flakyStore fails the calls a test selects; the others reach the in-memory store.
type flakyStore struct {
	*memstore.Store
	listErr  error
	saveErr  error
	saved    chan string
	getCalls int
}

func (s *flakyStore) ListMessages(ctx context.Context, threadID string) ([]*store.Message, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.Store.ListMessages(ctx, threadID)
}

func (s *flakyStore) SetGeneratedTitle(ctx context.Context, threadID, title string) (bool, error) {
	if s.saved != nil {
		defer func() { s.saved <- title }()
	}
	if s.saveErr != nil {
		return false, s.saveErr
	}
	return s.Store.SetGeneratedTitle(ctx, threadID, title)
}

type rig struct {
	t     *testing.T
	st    *flakyStore
	model *llmtest.Model
	hook  *titles.Hook
	rt    *runtime.Runtime
}

func newRig(t *testing.T, answer string, turns ...llmtest.Turn) *rig {
	t.Helper()
	mem := memstore.New()
	r := &rig{t: t, st: &flakyStore{Store: mem}, model: llmtest.New(turns...)}
	r.hook = &titles.Hook{Store: r.st, Models: llmtest.Factory{M: r.model}}
	q := queue.NewMemory()
	r.rt = runtime.New(runtime.Options{ID: "rt", Hooks: []runtime.Hook{r.hook}}, mem, eventlog.NewMemory(mem), q, q, answerer{st: mem, answer: answer}, nil)
	return r
}

// run executes one run of a new thread whose first message is question and returns the thread and the run's event log.
func (r *rig) run(question string) (*store.Thread, *store.Run, []map[string]any) {
	r.t.Helper()
	ctx := context.Background()
	th, err := r.st.CreateThread(ctx, "alice", "")
	if err != nil {
		r.t.Fatal(err)
	}
	return r.runOn(th, question)
}

func (r *rig) runOn(th *store.Thread, question string) (*store.Thread, *store.Run, []map[string]any) {
	r.t.Helper()
	ctx := context.Background()
	run, _, err := r.st.CreateRun(ctx, store.NewRun{RunID: "run-" + th.ID[:8], ThreadID: th.ID, UserLogin: "alice", Model: "gpt-test",
		Message: store.Message{Role: store.RoleUser, Content: question}, AutoTitle: strings.TrimSpace(question)})
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.rt.Process(ctx, run.ID); err != nil {
		r.t.Fatal(err)
	}
	got, err := r.st.GetRun(ctx, run.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	return r.thread(th.ID), got, r.events(run.ID)
}

func (r *rig) thread(id string) *store.Thread {
	r.t.Helper()
	th, err := r.st.GetThread(context.Background(), "alice", id)
	if err != nil {
		r.t.Fatal(err)
	}
	return th
}

func (r *rig) events(runID string) []map[string]any {
	r.t.Helper()
	evs, err := r.st.ListEvents(context.Background(), runID, 0, 1000)
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

func titleEvents(evs []map[string]any) []string {
	var out []string
	for _, e := range evs {
		if e["type"] == "CUSTOM" && e["name"] == agui.CustomTitle {
			out = append(out, e["value"].(map[string]any)["title"].(string))
		}
	}
	return out
}

func TestACompletedFirstRunTitlesTheThreadAndAnnouncesIt(t *testing.T) {
	// Arrange
	r := newRig(t, "The invoice total is 42 EUR.", llmtest.Turn{Chunks: []*ai.Message{llmtest.WithUsage(llmtest.Text("Title: \"Invoice total\"")[0], 30, 5)}})

	// Act
	th, run, evs := r.run("What is the total of my invoice?")

	// Assert
	if th.Title != "Invoice total" || th.TitleSource != store.TitleGenerated {
		t.Fatalf("thread = %q (%s)", th.Title, th.TitleSource)
	}
	if got := titleEvents(evs); len(got) != 1 || got[0] != "Invoice total" {
		t.Fatalf("work.title events = %v", got)
	}
	if run.Status != store.StatusCompleted {
		t.Fatalf("run %s", run.Status)
	}
	call := r.model.Calls()[0]
	if call.Messages[0].Content != titles.Instructions || call.Messages[1].Content != "User:\nWhat is the total of my invoice?\n\nAssistant:\nThe invoice total is 42 EUR." {
		t.Fatalf("title prompt = %q / %q", call.Messages[0].Content, call.Messages[1].Content)
	}
	if call.ReasoningEffort != "" {
		t.Fatalf("a model without reasoning support got effort %q", call.ReasoningEffort)
	}
}

func TestTheTitleCallsUsageIsCountedInTheRunsFinishedEvent(t *testing.T) {
	// The title call is the hook's own model call: its tokens join the run's usage of the same model.
	// Arrange
	r := newRig(t, "answer", llmtest.Turn{Chunks: []*ai.Message{llmtest.WithUsage(llmtest.Text("Short title")[0], 30, 5)}})

	// Act
	_, _, evs := r.run("question")

	// Assert
	final := evs[len(evs)-1]
	if final["type"] != "RUN_FINISHED" {
		t.Fatalf("last event %v", final)
	}
	usage := final["usage"].([]any)
	if len(usage) != 1 {
		t.Fatalf("usage = %v, want the title tokens merged into the run's gpt-test entry", usage)
	}
	u := usage[0].(map[string]any)
	if u["model"] != "gpt-test" || u["inputTokens"] != float64(130) || u["outputTokens"] != float64(15) {
		t.Fatalf("usage = %v", u)
	}
}

func TestTheTitleCallRunsAtLowEffortOnlyForReasoningModels(t *testing.T) {
	// Arrange
	r := newRig(t, "answer", llmtest.Turn{Chunks: llmtest.Text("A title")})
	r.hook.SupportsReasoning = func(m string) bool { return m == "gpt-test" }

	// Act
	r.run("question")

	// Assert
	if got := r.model.Calls()[0].ReasoningEffort; got != "low" {
		t.Fatalf("effort = %q, want low", got)
	}
}

func TestTheTitlePromptUsesTheFirstQuestionAndLastAnswerCut(t *testing.T) {
	// A long conversation sends the first user message (cut at 2000 characters) and the latest answer (cut at 1000).
	// Arrange
	r := newRig(t, strings.Repeat("a", 1500), llmtest.Turn{Chunks: llmtest.Text("Long one")})
	ctx := context.Background()
	th, _ := r.st.CreateThread(ctx, "alice", "")
	if err := r.st.AppendMessages(ctx,
		&store.Message{ThreadID: th.ID, Role: store.RoleUser, Content: "   "},
		&store.Message{ThreadID: th.ID, Role: store.RoleUser, Content: strings.Repeat("q", 2500)},
		&store.Message{ThreadID: th.ID, Role: store.RoleAssistant, Content: "old answer"},
		&store.Message{ThreadID: th.ID, Role: store.RoleUser, Content: "second question"},
	); err != nil {
		t.Fatal(err)
	}

	// Act
	r.runOn(th, "third question")

	// Assert
	prompt := r.model.Calls()[0].Messages[1].Content
	want := "User:\n" + strings.Repeat("q", 2000) + "…\n\nAssistant:\n" + strings.Repeat("a", 1000) + "…"
	if prompt != want {
		t.Fatalf("prompt has %d chars; question or answer was not cut as expected: %.80q", len(prompt), prompt)
	}
}

func TestAThreadWithoutTextInItsFirstMessageIsNotTitled(t *testing.T) {
	// Arrange
	r := newRig(t, "answer")

	// Act
	th, _, evs := r.run("   ")

	// Assert
	if len(r.model.Calls()) != 0 || th.TitleSource != store.TitleAuto || len(titleEvents(evs)) != 0 {
		t.Fatalf("calls %d thread %+v events %v", len(r.model.Calls()), th, titleEvents(evs))
	}
}

func TestOnlyCompletedNonCompactionRunsAreTitled(t *testing.T) {
	// Arrange
	r := newRig(t, "")
	r.rt = runtime.New(runtime.Options{ID: "rt", Hooks: []runtime.Hook{r.hook}}, r.st.Store, eventlog.NewMemory(r.st.Store), queue.NewMemory(), queue.NewMemory(),
		answerer{st: r.st.Store, status: &agent.Error{Code: agui.CodeInternal, Message: "boom"}}, nil)

	// Act
	th, run, _ := r.run("question")

	// Assert
	if run.Status != store.StatusFailed || len(r.model.Calls()) != 0 || th.TitleSource != store.TitleAuto {
		t.Fatalf("status %s calls %d thread %+v", run.Status, len(r.model.Calls()), th)
	}
}

func TestACompactionRunIsNotTitled(t *testing.T) {
	// Arrange
	r := newRig(t, "summary")
	ctx := context.Background()
	th, _ := r.st.CreateThread(ctx, "alice", "")
	run, _, err := r.st.CreateRun(ctx, store.NewRun{RunID: "compact-1", ThreadID: th.ID, UserLogin: "alice", Model: "m", Kind: store.KindCompact,
		Message: store.Message{Role: store.RoleUser, Content: "compact"}, AutoTitle: "compact"})
	if err != nil {
		t.Fatal(err)
	}

	// Act
	if err := r.rt.Process(ctx, run.ID); err != nil {
		t.Fatal(err)
	}

	// Assert
	if len(r.model.Calls()) != 0 || r.thread(th.ID).TitleSource != store.TitleAuto {
		t.Fatalf("calls %d thread %+v", len(r.model.Calls()), r.thread(th.ID))
	}
}

func TestAFailingTitleCallKeepsTheAutoTitleAndTheRunCompleted(t *testing.T) {
	// Arrange
	r := newRig(t, "answer", llmtest.Turn{Err: errors.New("provider down")})

	// Act
	th, run, evs := r.run("hello there")

	// Assert
	if run.Status != store.StatusCompleted || th.Title != "hello there" || th.TitleSource != store.TitleAuto || len(titleEvents(evs)) != 0 {
		t.Fatalf("run %s thread %+v events %v", run.Status, th, titleEvents(evs))
	}
}

func TestAnUnavailableModelKeepsTheAutoTitle(t *testing.T) {
	// Arrange
	r := newRig(t, "answer")
	r.hook.Models = failingFactory{}

	// Act
	th, run, evs := r.run("hello there")

	// Assert
	if run.Status != store.StatusCompleted || th.TitleSource != store.TitleAuto || len(titleEvents(evs)) != 0 {
		t.Fatalf("run %s thread %+v", run.Status, th)
	}
}

type failingFactory struct{}

func (failingFactory) Model(context.Context, string) (ai.Model, error) {
	return nil, errors.New("no such model")
}

func TestAnUnreadableThreadIsLeftAlone(t *testing.T) {
	// Arrange: the messages cannot be listed, so there is nothing to title from.
	r := newRig(t, "answer")
	r.st.listErr = errors.New("db down")

	// Act
	th, run, _ := r.run("hello")

	// Assert
	if run.Status != store.StatusCompleted || len(r.model.Calls()) != 0 || th.TitleSource != store.TitleAuto {
		t.Fatalf("run %s calls %d thread %+v", run.Status, len(r.model.Calls()), th)
	}
}

func TestATitleThatCannotBeSavedIsNotAnnounced(t *testing.T) {
	// Arrange
	r := newRig(t, "answer", llmtest.Turn{Chunks: llmtest.Text("Fine title")})
	r.st.saveErr = errors.New("write failed")

	// Act
	th, run, evs := r.run("hello")

	// Assert
	if run.Status != store.StatusCompleted || th.TitleSource != store.TitleAuto || len(titleEvents(evs)) != 0 {
		t.Fatalf("run %s thread %+v events %v", run.Status, th, titleEvents(evs))
	}
}

func TestAnEmptyModelAnswerLeavesTheAutoTitle(t *testing.T) {
	// Arrange
	r := newRig(t, "answer", llmtest.Turn{Chunks: llmtest.Text("  \n\"\"  ")})

	// Act
	th, _, evs := r.run("hello")

	// Assert
	if th.TitleSource != store.TitleAuto || len(titleEvents(evs)) != 0 {
		t.Fatalf("thread %+v events %v", th, titleEvents(evs))
	}
}

func TestATitleChosenByTheUserWhileTheModelWorkedIsNotOverwritten(t *testing.T) {
	// The user renames the thread while the title call is in flight: the generated title is not saved, no work.title goes out.
	// Arrange
	r := newRig(t, "answer")
	ctx := context.Background()
	th, _ := r.st.CreateThread(ctx, "alice", "")
	r.model = llmtest.New(llmtest.Turn{Chunks: llmtest.Text("Generated"), OnCall: func() {
		mine := "Mine"
		_, _ = r.st.UpdateThread(ctx, "alice", th.ID, store.ThreadPatch{Title: &mine})
	}})
	r.hook.Models = llmtest.Factory{M: r.model}

	// Act
	got, _, evs := r.runOn(th, "hello")

	// Assert
	if got.Title != "Mine" || got.TitleSource != store.TitleUser || len(titleEvents(evs)) != 0 {
		t.Fatalf("thread %+v events %v", got, titleEvents(evs))
	}
}

func TestASlowTitleIsSavedAfterTheRunWithoutHoldingIt(t *testing.T) {
	// A title that takes longer than InlineWait does not delay the run: it finishes without work.title and the title is stored when the model answers.
	// Arrange
	release := make(chan struct{})
	r := newRig(t, "answer", llmtest.Turn{Chunks: llmtest.Text("Late title"), OnCall: func() { <-release }})
	r.hook.InlineWait = 10 * time.Millisecond
	r.st.saved = make(chan string, 1)

	// Act
	th, run, evs := r.run("hello")
	beforeRelease := r.thread(th.ID)
	close(release)
	saved := <-r.st.saved

	// Assert
	if run.Status != store.StatusCompleted || len(titleEvents(evs)) != 0 || beforeRelease.TitleSource != store.TitleAuto {
		t.Fatalf("run %s events %v thread %+v", run.Status, titleEvents(evs), beforeRelease)
	}
	if saved != "Late title" {
		t.Fatalf("saved %q", saved)
	}
	if after := r.thread(th.ID); after.Title != "Late title" || after.TitleSource != store.TitleGenerated {
		t.Fatalf("thread after = %+v", after)
	}
}

func TestCleanTurnsAModelAnswerIntoAShortPlainTitle(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain", "Quarterly sales review", "Quarterly sales review"},
		{"first non-empty line only", "\n\n  Budget plan  \nWith a long explanation", "Budget plan"},
		{"prefix", "Title: Budget plan", "Budget plan"},
		{"french prefix with space", "Titre : Plan du budget", "Plan du budget"},
		{"french prefix", "Titre: Plan du budget", "Plan du budget"},
		{"quotes and markdown", "**\"Budget plan\"**", "Budget plan"},
		{"typographic quotes", "«Budget plan»", "Budget plan"},
		{"trailing period after quotes", "\"Budget plan.\".", "Budget plan"},
		{"heading marker", "## Budget plan", "Budget plan"},
		{"inner whitespace", "Budget\t  plan\u00a0now", "Budget plan now"},
		{"invalid utf-8 replaced", "Budget\xff plan", "Budget\ufffd plan"},
		{"empty", "  \n ", ""},
		{"only quotes", `"'`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Act
			got := titles.Clean(tc.in)

			// Assert
			if got != tc.want {
				t.Errorf("Clean(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCleanCutsALongTitleAtMaxCharsRunes(t *testing.T) {
	// Arrange
	long := strings.Repeat("é", 80)

	// Act
	got := titles.Clean(long)

	// Assert
	if want := strings.Repeat("é", titles.MaxChars-1) + "…"; got != want {
		t.Fatalf("Clean(80 runes) = %q (%d runes), want %d runes", got, len([]rune(got)), titles.MaxChars)
	}
}
