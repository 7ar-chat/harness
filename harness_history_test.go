package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/compaction"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/queue"
)

// flakyStore is a memstore whose history editing and message appends can be made to fail.
type flakyStore struct {
	*memstore.Store
	supersedeErr error
	failContent  string
}

func (f *flakyStore) SupersedeFrom(ctx context.Context, login, threadID, messageID string) error {
	if f.supersedeErr != nil {
		return f.supersedeErr
	}
	return f.Store.SupersedeFrom(ctx, login, threadID, messageID)
}

func (f *flakyStore) AppendMessages(ctx context.Context, msgs ...*store.Message) error {
	for _, m := range msgs {
		if f.failContent != "" && m.Content == f.failContent {
			return errors.New("disk full")
		}
	}
	return f.Store.AppendMessages(ctx, msgs...)
}

// noHistory hides the HistoryStore part of the store it wraps.
type noHistory struct{ harness.Store }

// failingBroker refuses to enqueue runs.
type failingBroker struct {
	*queue.Memory
}

func (failingBroker) Enqueue(context.Context, string) error { return errors.New("queue down") }

func TestUndoRemovesTheLastExchangeAndRedoPutsItBack(t *testing.T) {
	// Arrange
	h, ctx := facade(t, nil, echoModel(), nil)
	th, _ := h.NewThread(ctx, "alice", "")
	exchange(ctx, t, h, "alice", th.ID, "first", harness.RunOptions{})
	exchange(ctx, t, h, "alice", th.ID, "second", harness.RunOptions{})
	before := contents(messagesOf(ctx, t, h, th.ID))

	// Act
	undone, err := h.Undo(ctx, "alice", th.ID)

	// Assert: the second exchange is gone, the first stays
	if err != nil {
		t.Fatal(err)
	}
	if got := contents(messagesOf(ctx, t, h, th.ID)); !slices.Equal(got, []string{"first", "re: first"}) {
		t.Fatalf("messages after undo %q", got)
	}
	if undone.Prompt != "second" || !undone.CanUndo || !undone.CanRedo || len(undone.Files) != 0 {
		t.Fatalf("undo step %+v", undone)
	}

	// Act: redo
	redone, err := h.Redo(ctx, "alice", th.ID)

	// Assert: the very same conversation is back, and nothing is left to redo
	if err != nil {
		t.Fatal(err)
	}
	if got := contents(messagesOf(ctx, t, h, th.ID)); !slices.Equal(got, before) {
		t.Fatalf("messages after redo %q, want %q", got, before)
	}
	if redone.Prompt != "second" || !redone.CanUndo || redone.CanRedo {
		t.Fatalf("redo step %+v", redone)
	}
	if _, err := h.Redo(ctx, "alice", th.ID); !errors.Is(err, harness.ErrNothingToRedo) {
		t.Fatalf("second redo: %v", err)
	}
}

func TestSeveralUndosAreRedoneInReverseOrderAndStopAtTheStart(t *testing.T) {
	// Arrange
	h, ctx := facade(t, nil, echoModel(), nil)
	th, _ := h.NewThread(ctx, "alice", "")
	for _, q := range []string{"one", "two", "three"} {
		exchange(ctx, t, h, "alice", th.ID, q, harness.RunOptions{})
	}

	// Act
	var undone []string
	var canUndo []bool
	for {
		step, err := h.Undo(ctx, "alice", th.ID)
		if errors.Is(err, harness.ErrNothingToUndo) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		undone = append(undone, step.Prompt)
		canUndo = append(canUndo, step.CanUndo)
	}
	var redone []string
	for {
		step, err := h.Redo(ctx, "alice", th.ID)
		if errors.Is(err, harness.ErrNothingToRedo) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		redone = append(redone, step.Prompt)
	}

	// Assert
	if !slices.Equal(undone, []string{"three", "two", "one"}) || !slices.Equal(canUndo, []bool{true, true, false}) {
		t.Fatalf("undone %q canUndo %v", undone, canUndo)
	}
	if !slices.Equal(redone, []string{"one", "two", "three"}) {
		t.Fatalf("redone %q", redone)
	}
	want := []string{"one", "re: one", "two", "re: two", "three", "re: three"}
	if got := contents(messagesOf(ctx, t, h, th.ID)); !slices.Equal(got, want) {
		t.Fatalf("messages %q, want %q", got, want)
	}
}

func TestSendingAMessageEndsTheRedoOfAnEarlierUndo(t *testing.T) {
	// Arrange
	h, ctx := facade(t, nil, echoModel(), nil)
	th, _ := h.NewThread(ctx, "alice", "")
	exchange(ctx, t, h, "alice", th.ID, "old", harness.RunOptions{})
	if _, err := h.Undo(ctx, "alice", th.ID); err != nil {
		t.Fatal(err)
	}

	// Act: a new message makes the undone one unreachable
	exchange(ctx, t, h, "alice", th.ID, "new", harness.RunOptions{})
	_, redoErr := h.Redo(ctx, "alice", th.ID)

	// Assert
	if !errors.Is(redoErr, harness.ErrNothingToRedo) {
		t.Fatalf("redo after a new message: %v", redoErr)
	}
	if got := contents(messagesOf(ctx, t, h, th.ID)); !slices.Equal(got, []string{"new", "re: new"}) {
		t.Fatalf("messages %q", got)
	}

	// Act: undoing the new message starts a fresh redo history that no longer offers the old one
	step, err := h.Undo(ctx, "alice", th.ID)
	redone, rerr := h.Redo(ctx, "alice", th.ID)

	// Assert
	if err != nil || step.Prompt != "new" {
		t.Fatalf("undo %+v %v", step, err)
	}
	if rerr != nil || redone.Prompt != "new" || redone.CanRedo {
		t.Fatalf("redo %+v %v: the stale step of the earlier undo must be gone", redone, rerr)
	}
}

func TestUndoSkipsSteersToFindTheLastUserMessage(t *testing.T) {
	// Arrange: a steer injected during the run is a user-role message the user did not type as a request
	h, ctx := facade(t, nil, echoModel(), nil)
	th, _ := h.NewThread(ctx, "alice", "")
	exchange(ctx, t, h, "alice", th.ID, "request", harness.RunOptions{})
	if err := h.Store.AppendMessages(ctx, &store.Message{ThreadID: th.ID, Role: store.RoleUser, Content: "also do this", Metadata: map[string]any{"steer": true}}); err != nil {
		t.Fatal(err)
	}

	// Act
	step, err := h.Undo(ctx, "alice", th.ID)

	// Assert: the request goes, with the steer that followed it
	if err != nil || step.Prompt != "request" {
		t.Fatalf("undo %+v %v", step, err)
	}
	if got := messagesOf(ctx, t, h, th.ID); len(got) != 0 {
		t.Fatalf("messages left %q", contents(got))
	}
	if _, err := h.Undo(ctx, "alice", th.ID); !errors.Is(err, harness.ErrNothingToUndo) {
		t.Fatalf("undo of an empty thread: %v", err)
	}
}

func TestHistoryEditsRefuseABusyThreadAndAForeignOne(t *testing.T) {
	// Arrange: alice's run is stuck on a blocked model call
	model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		return llmtest.Turn{Chunks: llmtest.Text("working"), Block: true}
	})
	h, ctx := facade(t, nil, model, nil)
	th, _ := h.NewThread(ctx, "alice", "")
	if _, err := h.Send(ctx, "alice", th.ID, "start", harness.RunOptions{Model: "gpt-test"}); err != nil {
		t.Fatal(err)
	}

	// Act
	_, undoBusy := h.Undo(ctx, "alice", th.ID)
	_, redoBusy := h.Redo(ctx, "alice", th.ID)
	_, compactBusy := h.Compact(ctx, "alice", th.ID, "gpt-test")
	_, undoForeign := h.Undo(ctx, "bob", th.ID)
	_, redoForeign := h.Redo(ctx, "bob", th.ID)
	_, compactForeign := h.Compact(ctx, "bob", th.ID, "gpt-test")
	_, forkForeign := h.Fork(ctx, "bob", th.ID, "")

	// Assert
	for name, err := range map[string]error{"undo": undoBusy, "redo": redoBusy, "compact": compactBusy} {
		if !errors.Is(err, store.ErrRunActive) {
			t.Errorf("%s on a busy thread: %v", name, err)
		}
	}
	for name, err := range map[string]error{"undo": undoForeign, "redo": redoForeign, "compact": compactForeign, "fork": forkForeign} {
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s on another user's thread: %v", name, err)
		}
	}
}

func TestUndoNeedsAStoreThatEditsHistory(t *testing.T) {
	// Arrange
	h, ctx := facade(t, nil, echoModel(), func(o *harness.Options) { o.Store = noHistory{memstore.New()} })
	th, _ := h.NewThread(ctx, "alice", "")
	exchange(ctx, t, h, "alice", th.ID, "hello", harness.RunOptions{})

	// Act
	_, err := h.Undo(ctx, "alice", th.ID)

	// Assert
	if !errors.Is(err, harness.ErrHistoryUnsupported) {
		t.Fatalf("err %v", err)
	}
	if got := contents(messagesOf(ctx, t, h, th.ID)); !slices.Equal(got, []string{"hello", "re: hello"}) {
		t.Fatalf("messages %q", got)
	}
}

func TestAFailedUndoKeepsTheMessagesAndOffersNoRedo(t *testing.T) {
	// Arrange
	boom := errors.New("store down")
	st := &flakyStore{Store: memstore.New(), supersedeErr: boom}
	h, ctx := facade(t, nil, echoModel(), func(o *harness.Options) { o.Store = st })
	th, _ := h.NewThread(ctx, "alice", "")
	exchange(ctx, t, h, "alice", th.ID, "hello", harness.RunOptions{})

	// Act
	_, err := h.Undo(ctx, "alice", th.ID)
	_, redoErr := h.Redo(ctx, "alice", th.ID)

	// Assert
	if !errors.Is(err, boom) {
		t.Fatalf("undo err %v", err)
	}
	if !errors.Is(redoErr, harness.ErrNothingToRedo) {
		t.Fatalf("redo err %v", redoErr)
	}
	if got := contents(messagesOf(ctx, t, h, th.ID)); !slices.Equal(got, []string{"hello", "re: hello"}) {
		t.Fatalf("messages %q", got)
	}
}

func TestCompactSummarisesTheHistoryForTheNextRun(t *testing.T) {
	// Arrange
	model := echoModel()
	h, ctx := facade(t, nil, model, nil)
	th, _ := h.NewThread(ctx, "alice", "")
	exchange(ctx, t, h, "alice", th.ID, "plan the migration", harness.RunOptions{})

	// Act
	res, err := h.Compact(ctx, "alice", th.ID, "gpt-test")
	exchange(ctx, t, h, "alice", th.ID, "continue", harness.RunOptions{})

	// Assert
	if err != nil || !res.Compacted || res.TokensBefore <= 0 || res.TokensAfter <= 0 {
		t.Fatalf("compact %+v %v", res, err)
	}
	calls := model.Calls()
	last := calls[len(calls)-1]
	var all strings.Builder
	for _, m := range last.Messages {
		all.WriteString(string(m.Role) + ": " + m.Content + "\n")
	}
	if !strings.Contains(all.String(), "SUMMARY of the conversation") || strings.Contains(all.String(), "plan the migration") {
		t.Fatalf("the run after a compaction must see the summary instead of the old messages:\n%s", all.String())
	}
	if lastUserText(last) != "continue" {
		t.Fatalf("last user text %q", lastUserText(last))
	}
}

func TestCompactOfAnEmptyThreadReportsNothingToSummarise(t *testing.T) {
	// Arrange
	h, ctx := facade(t, nil, echoModel(), nil)
	th, _ := h.NewThread(ctx, "alice", "")

	// Act
	res, err := h.Compact(ctx, "alice", th.ID, "gpt-test")

	// Assert
	if err != nil || res.Compacted {
		t.Fatalf("compact %+v %v", res, err)
	}
}

func TestCompactRefusesAModelTheConfigDoesNotAllow(t *testing.T) {
	// Arrange
	h, ctx := facade(t, nil, echoModel(), nil)
	th, _ := h.NewThread(ctx, "alice", "")

	// Act
	_, none := h.Compact(ctx, "alice", th.ID, "")
	_, unknown := h.Compact(ctx, "alice", th.ID, "gpt-unknown")

	// Assert
	if !errors.Is(none, harness.ErrInvalidRequest) || !errors.Is(unknown, harness.ErrInvalidRequest) {
		t.Fatalf("errors %v / %v", none, unknown)
	}
}

// switchFactory fails every model request once broken is set.
type switchFactory struct {
	llmtest.Factory
	broken chan struct{}
}

func (f switchFactory) Model(ctx context.Context, name string) (ai.Model, error) {
	select {
	case <-f.broken:
		return nil, errors.New("provider unavailable")
	default:
		return f.Factory.Model(ctx, name)
	}
}

func TestCompactReportsTheFailureOfTheCompactionRun(t *testing.T) {
	// Arrange
	broken := make(chan struct{})
	h, ctx := facade(t, nil, echoModel(), func(o *harness.Options) {
		o.Models = switchFactory{Factory: llmtest.Factory{M: echoModel()}, broken: broken}
	})
	th, _ := h.NewThread(ctx, "alice", "")
	exchange(ctx, t, h, "alice", th.ID, "hello", harness.RunOptions{})
	close(broken)

	// Act
	res, err := h.Compact(ctx, "alice", th.ID, "gpt-test")

	// Assert
	if res != nil || err == nil || !strings.Contains(err.Error(), "compaction failed") {
		t.Fatalf("compact %+v %v", res, err)
	}
}

func TestCompactStopsWaitingWhenTheCallerGivesUp(t *testing.T) {
	// Arrange: the summariser never answers
	model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if len(c.Messages) > 0 && c.Messages[0].Content == compaction.Instructions {
			return llmtest.Turn{Block: true}
		}
		return llmtest.Turn{Chunks: llmtest.Text("ok")}
	})
	h, ctx := facade(t, map[string]string{"RUNTIME_SHUTDOWN_GRACE_SECONDS": "1"}, model, nil)
	th, _ := h.NewThread(ctx, "alice", "")
	exchange(ctx, t, h, "alice", th.ID, "hello", harness.RunOptions{})
	callCtx, cancel := context.WithCancel(ctx)
	before := len(model.Calls())
	go func() {
		eventually(ctx, t, "the summariser call", func() bool { return len(model.Calls()) > before })
		cancel()
	}()

	// Act
	res, err := h.Compact(callCtx, "alice", th.ID, "gpt-test")

	// Assert
	if res != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("compact %+v %v", res, err)
	}
}

func TestForkCopiesTheThreadUpToAMessageAndKeepsToolResultsWithTheirCall(t *testing.T) {
	// Arrange
	h, ctx := facade(t, nil, echoModel(), nil)
	th, _ := h.NewThread(ctx, "alice", "Refactor")
	call := func(id string) store.ToolCall {
		return store.ToolCall{ID: id, Type: "function", Function: store.ToolCallFunction{Name: "read_file", Arguments: "{}"}}
	}
	seed := []*store.Message{
		{ThreadID: th.ID, RunID: "r1", Role: store.RoleUser, Content: "q"},
		{ThreadID: th.ID, RunID: "r1", Role: store.RoleAssistant, Content: "calling", ToolCalls: []store.ToolCall{call("c1"), call("c2")}},
		{ThreadID: th.ID, RunID: "r1", Role: store.RoleTool, ToolCallID: "c1", Content: "result 1"},
		{ThreadID: th.ID, RunID: "r1", Role: store.RoleTool, ToolCallID: "c2", Content: "result 2"},
		{ThreadID: th.ID, RunID: "r1", Role: store.RoleTool, ToolCallID: "c9", Content: "unrelated"},
		{ThreadID: th.ID, RunID: "r1", Role: store.RoleAssistant, Content: "done"},
	}
	if err := h.Store.AppendMessages(ctx, seed...); err != nil {
		t.Fatal(err)
	}
	originals := messagesOf(ctx, t, h, th.ID)

	cases := []struct {
		name string
		upTo string
		want []string
	}{
		{"the whole thread", "", []string{"q", "calling", "result 1", "result 2", "unrelated", "done"}},
		{"a user message", originals[0].ID, []string{"q"}},
		{"an assistant call brings the results that answer it", originals[1].ID, []string{"q", "calling", "result 1", "result 2"}},
		{"a tool result alone", originals[2].ID, []string{"q", "calling", "result 1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Act
			fork, err := h.Fork(ctx, "alice", th.ID, c.upTo)

			// Assert
			if err != nil {
				t.Fatal(err)
			}
			if fork.ID == th.ID || fork.Title != "Refactor (fork)" {
				t.Fatalf("fork %+v", fork)
			}
			got := messagesOf(ctx, t, h, fork.ID)
			if !slices.Equal(contents(got), c.want) {
				t.Fatalf("fork messages %q, want %q", contents(got), c.want)
			}
			for i, m := range got {
				if m.ID == originals[i].ID || m.RunID != "" || m.ThreadID != fork.ID {
					t.Fatalf("copied message %d shares identity with the original: %+v", i, m)
				}
			}
		})
	}
	if got := messagesOf(ctx, t, h, th.ID); len(got) != len(seed) {
		t.Fatalf("forking changed the original thread: %q", contents(got))
	}
}

func TestForkRefusesAnUnknownMessage(t *testing.T) {
	// Arrange
	h, ctx := facade(t, nil, echoModel(), nil)
	th, _ := h.NewThread(ctx, "alice", "")
	exchange(ctx, t, h, "alice", th.ID, "hello", harness.RunOptions{})

	// Act
	_, err := h.Fork(ctx, "alice", th.ID, "no-such-message")

	// Assert
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err %v", err)
	}
	if list, _ := h.Store.ListThreads(ctx, "alice", 10, nil); len(list) != 1 {
		t.Fatalf("a refused fork left %d threads", len(list))
	}
}

func TestAForkThatCannotBeWrittenLeavesNoThreadBehind(t *testing.T) {
	// Arrange
	st := &flakyStore{Store: memstore.New()}
	h, ctx := facade(t, nil, echoModel(), func(o *harness.Options) { o.Store = st })
	th, _ := h.NewThread(ctx, "alice", "t")
	if err := st.Store.AppendMessages(ctx, &store.Message{ThreadID: th.ID, Role: store.RoleUser, Content: "boom"}); err != nil {
		t.Fatal(err)
	}
	st.failContent = "boom"

	// Act
	_, err := h.Fork(ctx, "alice", th.ID, "")

	// Assert
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err %v", err)
	}
	list, _ := h.Store.ListThreads(ctx, "alice", 10, nil)
	if len(list) != 1 || list[0].ID != th.ID {
		t.Fatalf("threads after the failed fork: %d", len(list))
	}
}

// finalOutcome is the outcome type of the last event of a run ("cancelled", "success", "interrupt").
func finalOutcome(t *testing.T, evs []store.Event) string {
	t.Helper()
	var ev struct {
		Outcome struct {
			Type string `json:"type"`
		} `json:"outcome"`
	}
	if err := json.Unmarshal(evs[len(evs)-1].Data, &ev); err != nil {
		t.Fatal(err)
	}
	return ev.Outcome.Type
}

func TestCancelEndsAQueuedRunAtOnceWithACancelledFinish(t *testing.T) {
	// Arrange: no runtime is consuming the queue, so the run stays queued
	h := buildFacade(t, nil, echoModel(), nil)
	ctx := context.Background()
	th, _ := h.NewThread(ctx, "alice", "")
	run, err := h.Send(ctx, "alice", th.ID, "never runs", harness.RunOptions{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}

	// Act
	err = h.Cancel(ctx, "alice", run.ID)
	again := h.Cancel(ctx, "alice", run.ID)
	foreign := h.Cancel(ctx, "bob", run.ID)

	// Assert
	if err != nil || again != nil {
		t.Fatalf("cancel %v, cancel again %v", err, again)
	}
	if !errors.Is(foreign, store.ErrNotFound) {
		t.Fatalf("cancel by another user: %v", foreign)
	}
	got, _ := h.Store.GetRun(ctx, run.ID)
	if got.Status != store.StatusCancelled {
		t.Fatalf("status %s", got.Status)
	}
	evs := waitRun(ctx, t, h, run)
	if evs[0].Type != "RUN_STARTED" || finalOutcome(t, evs) != "cancelled" {
		t.Fatalf("events %v", evs)
	}
}

func TestCancelStopsARunningRun(t *testing.T) {
	// Arrange
	model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		return llmtest.Turn{Chunks: llmtest.Text("thinking"), Block: true}
	})
	h, ctx := facade(t, nil, model, nil)
	th, _ := h.NewThread(ctx, "alice", "")
	run, err := h.Send(ctx, "alice", th.ID, "long task", harness.RunOptions{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(ctx, t, "the run to start", func() bool {
		r, _ := h.Store.GetRun(ctx, run.ID)
		return r != nil && r.Status == store.StatusRunning && len(model.Calls()) > 0
	})

	// Act
	err = h.Cancel(ctx, "alice", run.ID)
	evs := waitRun(ctx, t, h, run)

	// Assert
	if err != nil || finalOutcome(t, evs) != "cancelled" {
		t.Fatalf("cancel %v, outcome %q", err, finalOutcome(t, evs))
	}
	eventually(ctx, t, "the run to be recorded as cancelled", func() bool {
		r, _ := h.Store.GetRun(ctx, run.ID)
		return r.Status == store.StatusCancelled
	})
}

func TestSendReportsAQueueThatRefusesTheRun(t *testing.T) {
	// Arrange
	h := buildFacade(t, nil, echoModel(), func(o *harness.Options) {
		o.Infra = &harness.Infra{Broker: failingBroker{queue.NewMemory()}}
	})
	ctx := context.Background()
	th, _ := h.NewThread(ctx, "alice", "")

	// Act
	_, send := h.Send(ctx, "alice", th.ID, "hi", harness.RunOptions{Model: "gpt-test"})
	_, compact := h.Compact(ctx, "alice", th.ID, "gpt-test")

	// Assert: the enqueue failure surfaces (the first run is still queued, hence the second error may be a conflict)
	if send == nil || !strings.Contains(send.Error(), "queue down") {
		t.Fatalf("send: %v", send)
	}
	if compact == nil {
		t.Fatal("compact succeeded although its run could not be queued")
	}
}
