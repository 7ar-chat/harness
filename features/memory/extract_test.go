package memory_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/runtime"
	"github.com/7ar-chat/harness/features/memory"
)

// extractRig is an extractor over a scripted model that records what it was asked.
type extractRig struct {
	f     *fixture
	x     *memory.Extractor
	model *llmtest.Model
}

func newExtractRig(t *testing.T, answer string, msgs []*store.Message) *extractRig {
	t.Helper()
	f := newFixture(memory.Options{Extract: true})
	f.svc.Messages = func(context.Context, string) ([]*store.Message, error) { return msgs, nil }
	f.svc.UserKeyFor = func(context.Context, string, string) string { return "pu:p1:alice" }
	m := llmtest.NewFunc(func(llmtest.Call) llmtest.Turn { return llmtest.Turn{Chunks: llmtest.Text(answer)} })
	return &extractRig{f: f, x: &memory.Extractor{Svc: f.svc, Models: llmtest.Factory{M: m}}, model: m}
}

func (r *extractRig) finish(run *store.Run, status string) {
	r.x.Finishing(context.Background(), &runtime.Finish{Run: run, Status: status})
}

func (r *extractRig) run() *store.Run {
	return &store.Run{ID: "r1", ThreadID: "t1", UserLogin: "alice", Model: "run-model"}
}

// settled waits until the model has been asked (the prompt is then recorded).
func (r *extractRig) settled(t *testing.T) {
	t.Helper()
	waitFor(t, "the model to be asked", func() bool { return len(r.model.Calls()) > 0 })
}

func TestExtractionPromptShowsTheExchangeAndTheKnownMemories(t *testing.T) {
	// The model is shown the latest exchange from the user's last message (who spoke, tool calls, trimmed results) and
	// the existing memories that resemble it, by id and layer; with none it is told so.
	// Arrange
	msgs := []*store.Message{
		{Role: store.RoleUser, Content: "an old question that is not part of the latest exchange"},
		{Role: store.RoleUser, Content: "steered: also mention the freeze", Metadata: map[string]any{"steer": true}},
		{Role: store.RoleUser, Content: "Please remember that deploys go through staging before production.", AuthorLogin: "alice"},
		{Role: store.RoleUser, Content: "Also, a note on behalf of the manager about staging.", AuthorKind: store.AuthorSupervisor, OnBehalfOf: "bob"},
		{Role: store.RoleAssistant, Content: "I will note the staging rule.", ToolCalls: []store.ToolCall{{Function: store.ToolCallFunction{Name: "bash", Arguments: `{"cmd":"make deploy"}`}}}},
		{Role: store.RoleTool, Content: "deploy   finished\n\nOK"},
		{Role: store.RoleAssistant, Content: "   "},
	}
	rig := newExtractRig(t, `{"memories":[]}`, msgs)
	known := newEntry(t, rig.f, rig.f.w, memory.CreateInput{Title: ptr("Staging first"), Content: "Deploys go through staging before production."})

	// Act
	rig.finish(rig.run(), store.StatusCompleted)
	rig.settled(t)

	// Assert
	calls := rig.model.Calls()
	if len(calls) != 1 {
		t.Fatalf("model calls = %d", len(calls))
	}
	prompt := calls[0].Messages[1].Content
	// The exchange starts at the last user message that is not a steer: the earlier alice message is part of it only when it is that one.
	for _, want := range []string{"id=" + known + " [user] Staging first", "assistant on behalf of bob: Also, a note", "agent: I will note the staging rule.",
		"agent ran bash({\"cmd\":\"make deploy\"})", "result: deploy finished OK"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "an old question") || strings.Contains(prompt, "Please remember") || strings.Contains(prompt, "agent: \n") {
		t.Errorf("prompt has more than the latest exchange or blank agent lines:\n%s", prompt)
	}
	if calls[0].ReasoningEffort != "" {
		t.Fatalf("reasoning effort %q for a model that does not reason", calls[0].ReasoningEffort)
	}

	// Act: no memory resembles the exchange (and a user message names its author).
	empty := newExtractRig(t, `{"memories":[]}`, convo)
	empty.finish(empty.run(), store.StatusCompleted)
	empty.settled(t)

	// Assert
	if p := empty.model.Calls()[0].Messages[1].Content; !strings.Contains(p, "Existing memories:\n(none)") || !strings.Contains(p, "user alice: We decided to ship") {
		t.Fatalf("prompt = %s", p)
	}
}

func TestExtractionUsesTheConfiguredModelAndReasoningEffort(t *testing.T) {
	// ExtractModel overrides the run's model; a reasoning model gets a low effort.
	// Arrange
	var asked []string
	rig := newExtractRig(t, `{"memories":[]}`, convo)
	rig.x.Models = recordingFactory{rig.model, &asked}
	rig.x.SupportsReasoning = func(model string) bool { return model == "extractor-model" }
	rig.f.svc = memory.New(rig.f.st, memory.Options{Extract: true, ExtractModel: "extractor-model"})
	rig.f.svc.Messages = func(context.Context, string) ([]*store.Message, error) { return convo, nil }
	rig.x.Svc = rig.f.svc

	// Act
	rig.finish(rig.run(), store.StatusCompleted)
	rig.settled(t)

	// Assert
	if len(asked) != 1 || asked[0] != "extractor-model" {
		t.Fatalf("model asked = %v", asked)
	}
	if got := rig.model.Calls()[0].ReasoningEffort; got != "low" {
		t.Fatalf("reasoning effort = %q", got)
	}
}

type recordingFactory struct {
	m     *llmtest.Model
	asked *[]string
}

func (f recordingFactory) Model(ctx context.Context, name string) (ai.Model, error) {
	*f.asked = append(*f.asked, name)
	return f.m, nil
}

func TestExtractionIsThrottledPerThread(t *testing.T) {
	// A second run finishing on the same thread right after the first does not trigger another extraction; another thread does.
	// Arrange
	rig := newExtractRig(t, `{"memories":[]}`, convo)

	// Act
	rig.finish(rig.run(), store.StatusCompleted)
	rig.settled(t)
	rig.finish(rig.run(), store.StatusCompleted)
	other := rig.run()
	other.ThreadID = "t2"
	rig.finish(other, store.StatusCompleted)
	waitFor(t, "the other thread's extraction", func() bool { return len(rig.model.Calls()) == 2 })

	// Assert
	if n := len(rig.model.Calls()); n != 2 {
		t.Fatalf("model calls = %d, want 2 (the repeated thread was throttled)", n)
	}
}

func TestExtractionIgnoresCompactionRunsAndMissingParts(t *testing.T) {
	// No extraction for compaction runs, nor without the service, the message source or the models.
	// Arrange
	rig := newExtractRig(t, `{"memories":[{"op":"add","content":"Something durable and long enough."}]}`, convo)
	compact := rig.run()
	compact.Kind = store.KindCompact
	noMessages := &memory.Extractor{Svc: memory.New(rig.f.st, memory.Options{Extract: true}), Models: rig.x.Models}
	noModels := &memory.Extractor{Svc: rig.f.svc}
	noSvc := &memory.Extractor{Models: rig.x.Models}

	// Act
	rig.finish(compact, store.StatusCompleted)
	for _, x := range []*memory.Extractor{noMessages, noModels, noSvc} {
		x.Finishing(context.Background(), &runtime.Finish{Run: rig.run(), Status: store.StatusCompleted})
	}

	// Assert
	if n := len(rig.model.Calls()); n != 0 {
		t.Fatalf("the model was called %d times", n)
	}
	if es, _ := rig.f.st.ListMemoryEntries(context.Background(), "alice"); len(es) != 0 {
		t.Fatalf("entries = %d", len(es))
	}
}

func TestExtractionFailuresAreCountedAndWriteNothing(t *testing.T) {
	// A model error, an unavailable model, unreadable messages or entries and an answer without JSON each end the extraction without a memory;
	// the first four count as errors.
	// Arrange
	run := func(prepare func(r *extractRig)) memory.Stats {
		rig := newExtractRig(t, `no json here at all`, convo)
		prepare(rig)
		rig.finish(rig.run(), store.StatusCompleted)
		waitFor(t, "the extraction to end", func() bool {
			st := rig.f.svc.Stats()
			return st.ExtractErrors > 0 || (st.ExtractRuns > 0 && len(rig.model.Calls()) > 0)
		})
		if es, _ := rig.f.st.ListMemoryEntries(context.Background(), "alice"); len(es) != 0 {
			t.Errorf("entries written: %d", len(es))
		}
		return rig.f.svc.Stats()
	}

	// Act
	modelErr := run(func(r *extractRig) {
		r.x.Models = llmtest.Factory{M: llmtest.NewFunc(func(llmtest.Call) llmtest.Turn { return llmtest.Turn{Err: errors.New("provider down")} })}
	})
	noModel := run(func(r *extractRig) { r.x.Models = failingFactory{} })
	noMessages := run(func(r *extractRig) {
		r.f.svc.Messages = func(context.Context, string) ([]*store.Message, error) { return nil, errors.New("db") }
	})
	st, svc, _ := newFaulty(memory.Options{Extract: true})
	svc.Messages = func(context.Context, string) ([]*store.Message, error) { return convo, nil }
	st.errList = map[string]error{"alice": errBoom}
	listing := &memory.Extractor{Svc: svc, Models: llmtest.Factory{M: llmtest.New()}}
	listing.Finishing(context.Background(), &runtime.Finish{Run: &store.Run{ID: "r", ThreadID: "t", UserLogin: "alice"}, Status: store.StatusCompleted})
	waitFor(t, "the listing error", func() bool { return svc.Stats().ExtractErrors == 1 })
	noJSON := run(func(*extractRig) {})

	// Assert
	if modelErr.ExtractErrors != 1 || noModel.ExtractErrors != 1 || noMessages.ExtractErrors != 1 {
		t.Fatalf("errors: model %+v no model %+v messages %+v", modelErr, noModel, noMessages)
	}
	if noJSON.ExtractErrors != 0 || noJSON.ExtractProposed != 0 || noJSON.ExtractRuns != 1 {
		t.Fatalf("an answer without JSON is a quiet run: %+v", noJSON)
	}
}

type failingFactory struct{}

func (failingFactory) Model(context.Context, string) (ai.Model, error) {
	return nil, errors.New("unknown model")
}

func TestExtractionStopsWhenMemoryIsOffForTheUser(t *testing.T) {
	// A user who turned memory off gets no extraction: the messages are not even read and the model is not called.
	// Arrange
	checked := make(chan struct{}, 1)
	st, svc, _ := newFaulty(memory.Options{Extract: true})
	st.onEnabled = func() { checked <- struct{}{} }
	var read atomic.Int32
	svc.Messages = func(context.Context, string) ([]*store.Message, error) { read.Add(1); return convo, nil }
	_ = st.Store.SetMemoryEnabled(context.Background(), "alice", false)
	model := llmtest.New()
	x := &memory.Extractor{Svc: svc, Models: llmtest.Factory{M: model}}

	// Act
	x.Finishing(context.Background(), &runtime.Finish{Run: &store.Run{ID: "r", ThreadID: "t", UserLogin: "alice"}, Status: store.StatusCompleted})
	<-checked

	// Assert
	if read.Load() != 0 || len(model.Calls()) != 0 {
		t.Fatalf("messages read %d, model calls %d with memory off", read.Load(), len(model.Calls()))
	}
}

func TestExtractionAppliesAtMostFiveItemsWithValidKinds(t *testing.T) {
	// Only the first five items of an answer are looked at (ignored ones count); unknown kinds become notes; empty items and unknown ops are ignored;
	// proposals are pending, marked auto and carry the run id.
	// Arrange
	var items []string
	for i := range 7 {
		items = append(items, fmt.Sprintf(`{"op":"add","kind":"Weird","content":"Distinct knowledge number %d that deserves a memory %s"}`, i, strings.Repeat(fmt.Sprintf("w%d ", i), 4)))
	}
	items = append([]string{`{"op":"add","content":"   "}`, `{"op":"delete","content":"Forget everything we know about the system."}`}, items...)
	rig := newExtractRig(t, `{"memories":[`+strings.Join(items, ",")+`]}`, convo)

	// Act
	rig.finish(rig.run(), store.StatusCompleted)
	waitFor(t, "the proposals", func() bool { return rig.f.svc.Stats().ExtractProposed == 3 })

	// Assert
	pending, _ := rig.f.st.ListMemoryEntries(context.Background(), "alice", store.MemoryPending)
	if len(pending) != 3 { // five items looked at, two of them ignored
		t.Fatalf("pending = %d", len(pending))
	}
	for _, e := range pending {
		if !strings.Contains(e.Path, "memories/semantic/notes/") || e.Metadata["auto"] != true || e.Metadata["run_id"] != "r1" || e.Metadata["kind"] != "notes" {
			t.Fatalf("proposal = %+v", e)
		}
	}
}

func TestExtractionUpdateNeedsAnActiveEntryOfTheRunsLayers(t *testing.T) {
	// An update is only applied to an active entry the run can see: not to a pending proposal, not to another user's entry, not when it would repeat the entry.
	// Arrange
	rig := newExtractRig(t, "", convo)
	ctx := context.Background()
	active := newEntry(t, rig.f, rig.f.w, memory.CreateInput{Content: "Deploy with make deploy."})
	pendingW := memory.Writer{Login: "alice", Source: store.MemorySourceProxy, Untrusted: true}
	pending := newEntry(t, rig.f, pendingW, memory.CreateInput{Content: "A pending proposal about deploys."})
	foreign := seed(rig.f.st, "bob", "bobs", "memories/semantic/facts/b.md", "Bob's private deploy note")
	answer := fmt.Sprintf(`{"memories":[
		{"op":"update","id":%q,"content":"Pending proposal rewritten."},
		{"op":"update","id":%q,"content":"Bob's note rewritten."},
		{"op":"update","id":%q,"content":"Deploy with make deploy"},
		{"op":"update","id":%q,"content":"Deploy through staging, then production."},
		{"op":"update","id":%q,"content":"  "}]}`, pending, foreign.ID, active, active, active)
	rig.x.Models = llmtest.Factory{M: llmtest.NewFunc(func(llmtest.Call) llmtest.Turn { return llmtest.Turn{Chunks: llmtest.Text(answer)} })}

	// Act
	rig.finish(rig.run(), store.StatusCompleted)
	waitFor(t, "the one real update", func() bool { return rig.f.svc.Stats().ExtractProposed == 1 })

	// Assert
	e, _ := rig.f.st.GetMemoryEntry(ctx, "alice", active)
	p, _ := rig.f.st.GetMemoryEntry(ctx, "alice", pending)
	b, _ := rig.f.st.GetMemoryEntry(ctx, "bob", foreign.ID)
	if e.Proposal == nil || e.Proposal.Content != "Deploy through staging, then production." || e.Content != "Deploy with make deploy." {
		t.Fatalf("active entry = %+v", e)
	}
	if p.Proposal != nil || p.Content != "A pending proposal about deploys." || b.Content != "Bob's private deploy note" || b.Proposal != nil {
		t.Fatalf("pending %+v bob %+v", p, b)
	}
	if st := rig.f.svc.Stats(); st.ExtractSkipped != 1 {
		t.Fatalf("the repeated content must be skipped: %+v", st)
	}
}

func TestExtractionAudienceMeFallsBackToTheOnlyLayer(t *testing.T) {
	// Without a private layer, a "me" item goes to the primary memory.
	// Arrange
	rig := newExtractRig(t, `{"memories":[{"op":"add","audience":"me","content":"Alice wants short answers in this repository."}]}`, convo)
	rig.f.svc.UserKeyFor = nil

	// Act
	rig.finish(rig.run(), store.StatusCompleted)
	waitFor(t, "the proposal", func() bool { return rig.f.svc.Stats().ExtractProposed == 1 })

	// Assert
	if es, _ := rig.f.st.ListMemoryEntries(context.Background(), "alice", store.MemoryPending); len(es) != 1 {
		t.Fatalf("pending = %d", len(es))
	}
}

func TestExtractionSkipsExchangesTooShortToTeachAnything(t *testing.T) {
	// A short user message or a short transcript is counted as skipped and never reaches the model.
	// Arrange
	rig := newExtractRig(t, `{"memories":[]}`, []*store.Message{{Role: store.RoleUser, Content: "ok thanks"}, {Role: store.RoleAssistant, Content: "You are welcome, anytime and always!"}})

	// Act
	rig.finish(rig.run(), store.StatusCompleted)
	waitFor(t, "the skip", func() bool { return rig.f.svc.Stats().ExtractSkipped == 1 })

	// Assert
	if n := len(rig.model.Calls()); n != 0 {
		t.Fatalf("model calls = %d", n)
	}
}

func TestExtractionKeepsTheEndOfALongTranscript(t *testing.T) {
	// A very long exchange is cut from the start: the outcome at the end is what the model sees.
	// Arrange
	msgs := []*store.Message{{Role: store.RoleUser, Content: "Please investigate why the nightly build fails on the arm runners."}}
	for i := range 40 {
		msgs = append(msgs, &store.Message{Role: store.RoleAssistant, Content: fmt.Sprintf("step %d: %s", i, strings.Repeat("log line ", 200))})
	}
	msgs = append(msgs, &store.Message{Role: store.RoleAssistant, Content: "Root cause: the arm runners lack the libfoo package."})
	rig := newExtractRig(t, `{"memories":[]}`, msgs)

	// Act
	rig.finish(rig.run(), store.StatusCompleted)
	rig.settled(t)

	// Assert
	prompt := rig.model.Calls()[0].Messages[1].Content
	if !strings.Contains(prompt, "Root cause: the arm runners lack the libfoo package.") || strings.Contains(prompt, "step 0:") || !strings.Contains(prompt, "…") {
		t.Fatalf("prompt (%d chars) does not keep the end of the exchange", len([]rune(prompt)))
	}
	if n := len([]rune(prompt)); n > 14000 {
		t.Fatalf("prompt of %d chars is not bounded", n)
	}
}
