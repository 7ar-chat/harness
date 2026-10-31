package agent_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/agent"
	"github.com/7ar-chat/harness/ai/llm"
	"github.com/7ar-chat/harness/ai/openai"
)

// named is a tool with a given name that records the arguments it was run with.
type named struct {
	name    string
	infoErr error
	out     string
	err     error
	args    *[]string
}

func (n named) Info(context.Context) (*ai.ToolInfo, error) {
	if n.infoErr != nil {
		return nil, n.infoErr
	}
	return &ai.ToolInfo{Name: n.name}, nil
}
func (n named) InvokableRun(_ context.Context, args string) (string, error) {
	if n.args != nil {
		*n.args = append(*n.args, args)
	}
	return n.out, n.err
}

// nilInfo is a tool that describes itself with nothing.
type nilInfo struct{}

func (nilInfo) Info(context.Context) (*ai.ToolInfo, error) { return nil, nil }
func (nilInfo) InvokableRun(context.Context, string) (string, error) {
	return "", errors.New("must not be reachable")
}

func TestRunWithoutAModelFailsBeforeAnything(t *testing.T) {
	// Act
	res, err := agent.Run(context.Background(), agent.Config{}, []*ai.Message{ai.UserMessage("go")})

	// Assert
	if !errors.Is(err, agent.ErrNoModel) || res.Messages != nil || res.Steps != 0 {
		t.Fatalf("res %+v err %v", res, err)
	}
}

func TestRunFailsWhenAToolCannotDescribeItself(t *testing.T) {
	// Arrange
	boom := errors.New("no schema")
	m := &script{}

	// Act
	_, err := agent.Run(context.Background(), agent.Config{Model: m, Tools: []ai.InvokableTool{named{name: "a"}, named{infoErr: boom}}},
		[]*ai.Message{ai.UserMessage("go")})

	// Assert
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "tool info") {
		t.Fatalf("err %v", err)
	}
	if len(m.calls) != 0 {
		t.Error("the model was called")
	}
}

func TestToolsWithoutANameAreNotCallable(t *testing.T) {
	// Arrange
	m := &script{replies: []*ai.Message{
		ai.AssistantMessage("", []ai.ToolCall{call("1", "", `{}`)}),
		ai.AssistantMessage("done", nil),
	}}
	var results []string

	// Act
	_, err := agent.Run(context.Background(), agent.Config{
		Model: m, Tools: []ai.InvokableTool{nilInfo{}, named{name: ""}},
		OnTool: func(_ ai.ToolCall, r string) { results = append(results, r) },
	}, []*ai.Message{ai.UserMessage("go")})

	// Assert
	if err != nil || len(results) != 1 || results[0] != `error: unknown tool ""` {
		t.Fatalf("err %v results %q", err, results)
	}
}

func TestDefaultInvokeGivesToolsAnEmptyObjectAndReportsTheirFailuresAsText(t *testing.T) {
	// Arrange
	var got []string
	m := &script{replies: []*ai.Message{
		ai.AssistantMessage("", []ai.ToolCall{
			call("1", "ok", ""), call("2", "ok", "  \n"), call("3", "ok", `{"k":1}`), call("4", "bad", `{}`)}),
		ai.AssistantMessage("done", nil),
	}}
	tools := []ai.InvokableTool{
		named{name: "ok", out: "fine", args: &got},
		named{name: "bad", err: errors.New("disk full")},
	}
	var results []string

	// Act
	res, err := agent.Run(context.Background(), agent.Config{Model: m, Tools: tools,
		OnTool: func(_ ai.ToolCall, r string) { results = append(results, r) }}, []*ai.Message{ai.UserMessage("go")})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "|") != `{}|{}|{"k":1}` {
		t.Errorf("arguments %q", got)
	}
	if strings.Join(results, "|") != "fine|fine|fine|error: disk full" {
		t.Errorf("results %q", results)
	}
	if last := res.Messages[len(res.Messages)-2]; last.Role != ai.RoleTool || last.Content != "error: disk full" || last.ToolName != "bad" {
		t.Errorf("failure not fed back to the model: %+v", last)
	}
}

func TestInvokeReplacesTheDefaultToolRunnerAndSeesEveryCall(t *testing.T) {
	// Arrange
	m := &script{replies: []*ai.Message{
		ai.AssistantMessage("", []ai.ToolCall{call("1", "anything", `{"a":1}`), call("2", "else", `{}`)}),
		ai.AssistantMessage("done", nil),
	}}

	// Act
	res, err := agent.Run(context.Background(), agent.Config{Model: m,
		Invoke: func(_ context.Context, c ai.ToolCall) string {
			return "gated " + c.Function.Name + c.Function.Arguments
		},
	}, []*ai.Message{ai.UserMessage("go")})

	// Assert
	if err != nil || res.Messages[2].Content != `gated anything{"a":1}` || res.Messages[3].Content != "gated else{}" {
		t.Fatalf("transcript %+v err %v", res.Messages, err)
	}
}

func TestHooksSeeEveryStepAndEveryAnswerInOrder(t *testing.T) {
	// Arrange
	m := &script{replies: []*ai.Message{
		ai.AssistantMessage("", []ai.ToolCall{call("1", "echo", `{}`)}),
		ai.AssistantMessage("done", nil),
	}}
	var events []string

	// Act
	_, err := agent.Run(context.Background(), agent.Config{
		Model: m, Tools: []ai.InvokableTool{echo{}},
		OnStep:  func(n int) { events = append(events, fmt.Sprint("step", n)) },
		OnReply: func(n int, r *ai.Message) { events = append(events, fmt.Sprintf("reply%d:%q", n, r.Content)) },
		OnTool:  func(c ai.ToolCall, _ string) { events = append(events, "tool"+c.ID) },
	}, []*ai.Message{ai.UserMessage("go")})

	// Assert
	if want := `step1 reply1:"" tool1 step2 reply2:"done"`; err != nil || strings.Join(events, " ") != want {
		t.Fatalf("events %q err %v, want %q", events, err, want)
	}
}

func TestOnReplyIsNotCalledForACallThatProducedNothing(t *testing.T) {
	// Arrange
	called := false

	// Act
	_, err := agent.Run(context.Background(), agent.Config{
		Model: &script{err: errors.New("down")}, OnReply: func(int, *ai.Message) { called = true },
	}, []*ai.Message{ai.UserMessage("go")})

	// Assert
	if err == nil || called {
		t.Fatalf("err %v, OnReply called: %v", err, called)
	}
}

func TestRunDoesNotModifyTheCallersConversationAndReturnsTheWholeTranscript(t *testing.T) {
	// Arrange
	in := make([]*ai.Message, 1, 10) // spare capacity: an append in place would be visible through in[:2]
	in[0] = ai.UserMessage("go")
	m := &script{replies: []*ai.Message{ai.AssistantMessage("hi", nil)}}

	// Act
	res, err := agent.Run(context.Background(), agent.Config{Model: m}, in)

	// Assert
	if err != nil || len(in) != 1 || in[:2][1] != nil {
		t.Fatalf("input changed: %v", in[:2])
	}
	if len(res.Messages) != 2 || res.Messages[0] != in[0] || res.Messages[1] != res.Reply {
		t.Errorf("transcript %+v", res.Messages)
	}
}

func TestStepLimitStopsTheLoopWithTheLastReplyWhenTheModelKeepsCallingTools(t *testing.T) {
	// Arrange
	m := &script{replies: []*ai.Message{
		ai.AssistantMessage("", []ai.ToolCall{call("1", "echo", `{}`)}),
		ai.AssistantMessage("", []ai.ToolCall{call("2", "echo", `{}`)}),
		ai.AssistantMessage("", []ai.ToolCall{call("3", "echo", `{}`)}),
	}}

	// Act
	res, err := agent.Run(context.Background(), agent.Config{Model: m, Tools: []ai.InvokableTool{echo{}}, MaxSteps: 3}, []*ai.Message{ai.UserMessage("go")})

	// Assert
	if err != nil || res.Steps != 3 {
		t.Fatalf("steps %d err %v", res.Steps, err)
	}
	if len(res.Messages) != 1+3+2 { // calls on the last step are not run
		t.Errorf("transcript of %d messages", len(res.Messages))
	}
	if res.Reply == nil || len(res.Reply.ToolCalls) != 1 {
		t.Errorf("reply %+v", res.Reply)
	}
}

func TestDefaultMaxStepsApplies(t *testing.T) {
	// Arrange
	var replies []*ai.Message
	for i := 0; i < agent.DefaultMaxSteps+5; i++ {
		replies = append(replies, ai.AssistantMessage("", []ai.ToolCall{call(fmt.Sprint(i), "echo", `{}`)}))
	}
	m := &script{replies: replies}

	// Act
	res, err := agent.Run(context.Background(), agent.Config{Model: m, Tools: []ai.InvokableTool{echo{}}}, []*ai.Message{ai.UserMessage("go")})

	// Assert
	if err != nil || res.Steps != agent.DefaultMaxSteps || len(m.calls) != agent.DefaultMaxSteps {
		t.Fatalf("steps %d calls %d err %v", res.Steps, len(m.calls), err)
	}
}

func TestMaxParallelBoundsTheConcurrentReadOnlyCalls(t *testing.T) {
	// Arrange
	var active, peak atomic.Int32
	calls := make([]ai.ToolCall, 6)
	for i := range calls {
		calls[i] = call(fmt.Sprint(i), "echo", `{}`)
	}
	m := &script{replies: []*ai.Message{ai.AssistantMessage("", calls), ai.AssistantMessage("ok", nil)}}

	// Act
	res, err := agent.Run(context.Background(), agent.Config{
		Model: m, Tools: []ai.InvokableTool{echo{ro: true, active: &active, peak: &peak}}, Parallel: true, MaxParallel: 2,
	}, []*ai.Message{ai.UserMessage("go")})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if peak.Load() != 2 {
		t.Errorf("peak %d, want exactly the bound of 2", peak.Load())
	}
	for i := 0; i < 6; i++ { // results land in call order whatever finished first
		if got := res.Messages[2+i]; got.ToolCallID != fmt.Sprint(i) {
			t.Errorf("result %d is for call %s", i, got.ToolCallID)
		}
	}
}

func TestStreamingStepsDefaultTheRoleAndFailWhenChunksDisagree(t *testing.T) {
	// Arrange
	roleless := &streamModel{plays: []play{{chunks: []*ai.Message{{Content: "a"}, {Content: "b"}}}}}
	clash := &streamModel{plays: []play{{chunks: []*ai.Message{{Role: ai.RoleAssistant, Content: "a"}, {Role: ai.RoleUser, Content: "b"}}}}}
	cfg := func(m ai.Model) agent.Config { return agent.Config{Model: m, Caller: &agent.Caller{}} }

	// Act
	res, err := agent.Run(context.Background(), cfg(streamAdapter{roleless}), []*ai.Message{ai.UserMessage("go")})
	_, clashErr := agent.Run(context.Background(), cfg(streamAdapter{clash}), []*ai.Message{ai.UserMessage("go")})

	// Assert
	if err != nil || res.Reply.Role != ai.RoleAssistant || res.Reply.Content != "ab" {
		t.Fatalf("reply %+v err %v", res.Reply, err)
	}
	if clashErr == nil || !strings.Contains(clashErr.Error(), "different roles") {
		t.Errorf("err %v", clashErr)
	}
}

func TestAStreamedCallThatFailsReturnsTheCallError(t *testing.T) {
	// Arrange
	m := streamAdapter{&streamModel{plays: []play{{openErr: &openai.APIError{Status: 401, Message: "bad key"}}}}}

	// Act
	res, err := agent.Run(context.Background(), agent.Config{Model: m, Caller: &agent.Caller{Retries: 3}}, []*ai.Message{ai.UserMessage("go")})

	// Assert
	var ce *agent.CallError
	if !errors.As(err, &ce) || ce.Class != llm.ClassFatal || !strings.Contains(ce.Error(), "bad key") || len(res.Messages) != 1 {
		t.Fatalf("err %v res %+v", err, res)
	}
}

// streamAdapter makes a streamModel an ai.Model.
type streamAdapter struct{ *streamModel }

func (a streamAdapter) WithTools([]*ai.ToolInfo) (ai.Model, error) { return a, nil }

func TestCallerStopsRetryingWhenTheRetryHookRefuses(t *testing.T) {
	// Arrange
	stop := errors.New("budget exhausted")
	m := &streamModel{plays: []play{{openErr: &openai.APIError{Status: 503}}, {chunks: []*ai.Message{text("never")}}}}
	r := &rec{}
	c := &agent.Caller{Retries: 3, OnRetry: func(context.Context, int, llm.ErrorClass, error) error { return stop },
		Sleep: func(context.Context, time.Duration) error { t.Error("slept after the hook refused"); return nil }}

	// Act
	_, err := c.Call(context.Background(), m, nil, nil, func() agent.Attempt { return recAttempt{r} })

	// Assert
	if !errors.Is(err, stop) || m.calls != 1 {
		t.Fatalf("err %v calls %d", err, m.calls)
	}
}

func TestCallerEndsTheCallWhenAbortOrSettleOrSleepFail(t *testing.T) {
	boom := errors.New("hook failed")
	retryable := func() *streamModel {
		return &streamModel{plays: []play{{chunks: []*ai.Message{text("p")}, failAfter: &openai.APIError{Status: 503}}, {chunks: []*ai.Message{text("ok")}}}}
	}
	t.Run("abort", func(t *testing.T) {
		m := retryable()
		_, err := (&agent.Caller{Retries: 2}).Call(context.Background(), m, nil, nil, func() agent.Attempt { return hookAttempt{abort: boom} })
		if !errors.Is(err, boom) || m.calls != 1 {
			t.Fatalf("err %v calls %d", err, m.calls)
		}
	})
	t.Run("abort of the final failure", func(t *testing.T) {
		m := retryable()
		_, err := (&agent.Caller{}).Call(context.Background(), m, nil, nil, func() agent.Attempt { return hookAttempt{abort: boom} })
		var ce *agent.CallError
		if !errors.Is(err, boom) || errors.As(err, &ce) {
			t.Fatalf("err %v: the abort failure replaces the call error", err)
		}
	})
	t.Run("settle", func(t *testing.T) {
		m := &streamModel{plays: []play{{chunks: []*ai.Message{text("a")}}}}
		_, err := (&agent.Caller{Retries: 2}).Call(context.Background(), m, nil, nil, func() agent.Attempt { return hookAttempt{settle: boom} })
		if !errors.Is(err, boom) || m.calls != 1 {
			t.Fatalf("err %v calls %d", err, m.calls)
		}
	})
	t.Run("sleep", func(t *testing.T) {
		m := retryable()
		c := &agent.Caller{Retries: 2, Sleep: func(context.Context, time.Duration) error { return boom }}
		_, err := c.Call(context.Background(), m, nil, nil, func() agent.Attempt { return hookAttempt{} })
		if !errors.Is(err, boom) || m.calls != 1 {
			t.Fatalf("err %v calls %d", err, m.calls)
		}
	})
}

type hookAttempt struct{ abort, settle error }

func (hookAttempt) Chunk(context.Context, *ai.Message) error { return nil }
func (a hookAttempt) Settle(context.Context, bool) error     { return a.settle }
func (a hookAttempt) Abort(context.Context, bool) error      { return a.abort }

func TestCallerWaitsAtLeastTheProvidersRetryAfter(t *testing.T) {
	// Arrange
	m := &hintModel{after: 7 * time.Second, inner: &streamModel{plays: []play{{openErr: &openai.APIError{Status: 429}}, {chunks: []*ai.Message{text("ok")}}}}}
	var slept []time.Duration
	c := &agent.Caller{Retries: 1, Backoff: time.Millisecond, Sleep: func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }}

	// Act
	out, err := c.Call(context.Background(), m, nil, nil, func() agent.Attempt { return recAttempt{&rec{}} })

	// Assert
	if err != nil || len(out.Chunks) != 1 {
		t.Fatalf("out %+v err %v", out, err)
	}
	if len(slept) != 1 || slept[0] < 7*time.Second {
		t.Fatalf("slept %v, want at least the 7s hint", slept)
	}
}

// hintModel records a Retry-After hint into the call's context when the first call fails.
type hintModel struct {
	after time.Duration
	inner *streamModel
}

func (h *hintModel) Stream(ctx context.Context, msgs []*ai.Message, opts ...ai.Option) (*ai.StreamReader[*ai.Message], error) {
	sr, err := h.inner.Stream(ctx, msgs, opts...)
	if err != nil {
		llm.RecordRetryAfter(ctx, h.after)
	}
	return sr, err
}
func (h *hintModel) Generate(context.Context, []*ai.Message, ...ai.Option) (*ai.Message, error) {
	return nil, errors.New("not used")
}

func TestCallerSleepsOnTheClockAndStopsWhenTheContextEnds(t *testing.T) {
	// Arrange
	m := &streamModel{plays: []play{{openErr: &openai.APIError{Status: 503}}, {chunks: []*ai.Message{text("ok")}}}}
	c := &agent.Caller{Retries: 1, Backoff: time.Millisecond}

	// Act: a real (tiny) backoff
	out, err := c.Call(context.Background(), m, nil, nil, func() agent.Attempt { return recAttempt{&rec{}} })

	// Assert
	if err != nil || len(out.Chunks) != 1 {
		t.Fatalf("out %+v err %v", out, err)
	}

	// Arrange: the context ends during a long backoff
	ctx, cancel := context.WithCancel(context.Background())
	m2 := &streamModel{plays: []play{{openErr: &openai.APIError{Status: 503}}, {chunks: []*ai.Message{text("never")}}}}
	c2 := &agent.Caller{Retries: 1, Backoff: time.Hour,
		OnRetry: func(context.Context, int, llm.ErrorClass, error) error { cancel(); return nil }}

	// Act
	_, err = c2.Call(ctx, m2, nil, nil, func() agent.Attempt { return recAttempt{&rec{}} })

	// Assert
	if !errors.Is(err, context.Canceled) || m2.calls != 1 {
		t.Fatalf("err %v calls %d", err, m2.calls)
	}
}

// pulseModel sends chunks, with activity-only events in between, pausing between them.
type pulseModel struct {
	steps []string // "chunk:x" or "pulse"
	gap   time.Duration
}

func (p pulseModel) Stream(ctx context.Context, _ []*ai.Message, _ ...ai.Option) (*ai.StreamReader[*ai.Message], error) {
	sr, sw := ai.Pipe[*ai.Message](4)
	go func() {
		defer sw.Close()
		for _, s := range p.steps {
			select {
			case <-time.After(p.gap):
			case <-ctx.Done():
				sw.Send(nil, ctx.Err())
				return
			}
			if s == "pulse" {
				llm.RecordActivity(ctx)
			} else {
				sw.Send(text(strings.TrimPrefix(s, "chunk:")), nil)
			}
		}
	}()
	return sr, nil
}
func (pulseModel) Generate(context.Context, []*ai.Message, ...ai.Option) (*ai.Message, error) {
	return nil, errors.New("not used")
}

func TestStreamActivityThatIsNotAChunkKeepsTheWatchdogQuiet(t *testing.T) {
	// Arrange: the first chunk comes after 600ms in total, far past FirstChunk (400ms), but the stream shows life
	// every 150ms; after the first chunk the gaps between chunks (also 150ms × 4) exceed Idle (400ms) the same way.
	m := pulseModel{gap: 150 * time.Millisecond, steps: []string{"pulse", "pulse", "pulse", "chunk:a", "pulse", "pulse", "pulse", "chunk:b"}}
	c := &agent.Caller{FirstChunk: 400 * time.Millisecond, Idle: 400 * time.Millisecond}

	// Act
	out, err := c.Call(context.Background(), m, nil, nil, func() agent.Attempt { return recAttempt{&rec{}} })

	// Assert
	if err != nil || len(out.Chunks) != 2 {
		t.Fatalf("out %+v err %v: a reasoning model that is alive was cut", out, err)
	}
}

func TestCeilingCutsACallThatNeverEndsEvenWhileItStaysActive(t *testing.T) {
	// Arrange
	m := pulseModel{gap: 20 * time.Millisecond, steps: append(make([]string, 0), strings.Split(strings.Repeat("pulse ", 200), " ")[:200]...)}
	c := &agent.Caller{FirstChunk: time.Second, Idle: time.Second, Ceiling: 150 * time.Millisecond}

	// Act
	_, err := c.Call(context.Background(), m, nil, nil, func() agent.Attempt { return recAttempt{&rec{}} })

	// Assert
	var ce *agent.CallError
	if !errors.As(err, &ce) || ce.Class != llm.ClassTimeout || !errors.Is(err, agent.ErrStalled) {
		t.Fatalf("err %v, want a stall timeout", err)
	}
}

func TestCallerReportsAStreamedRefusal(t *testing.T) {
	// Arrange
	m := refusingModel{}

	// Act
	out, err := (&agent.Caller{}).Call(context.Background(), m, nil, nil, func() agent.Attempt { return recAttempt{&rec{}} })

	// Assert
	if err != nil || out.Refusal != "I can't help with that." {
		t.Fatalf("out %+v err %v", out, err)
	}
}

type refusingModel struct{}

func (refusingModel) Stream(ctx context.Context, _ []*ai.Message, _ ...ai.Option) (*ai.StreamReader[*ai.Message], error) {
	llm.RecordRefusal(ctx, "I can't help ")
	llm.RecordRefusal(ctx, "with that.")
	sr, sw := ai.Pipe[*ai.Message](1)
	sw.Close()
	return sr, nil
}
func (refusingModel) Generate(context.Context, []*ai.Message, ...ai.Option) (*ai.Message, error) {
	return nil, errors.New("not used")
}

func TestCallErrorUnwrapsToTheProviderError(t *testing.T) {
	// Arrange
	inner := &openai.APIError{Status: 500, Message: "boom"}
	err := &agent.CallError{Err: inner, Class: llm.ClassTransient}

	// Assert
	var api *openai.APIError
	if !errors.As(err, &api) || err.Error() != inner.Error() {
		t.Fatalf("err %v", err)
	}
}

func TestParallelWithoutALimitRunsEverythingAtOnce(t *testing.T) {
	// Arrange: every fn waits for all the others, which only works when all run together.
	const n = 5
	var wg sync.WaitGroup
	wg.Add(n)
	released := make(chan struct{})
	go func() { wg.Wait(); close(released) }()
	var done atomic.Int32

	// Act
	agent.Parallel(n, 0, func(int) {
		wg.Done()
		<-released
		done.Add(1)
	})

	// Assert
	if done.Load() != n {
		t.Fatalf("done %d", done.Load())
	}
}

func TestCapOutputCountsCharactersNotBytesAndCopesWithATinyLimit(t *testing.T) {
	// Arrange
	s := strings.Repeat("a", 100) + strings.Repeat("é", 100) + strings.Repeat("z", 100)

	// Act
	got := agent.CapOutput(s, 150)
	tiny := agent.CapOutput(s, 10)

	// Assert
	if n := len([]rune(got)); n > 150 {
		t.Errorf("%d characters for a limit of 150", n)
	}
	if !strings.HasPrefix(got, "aaaa") || !strings.HasSuffix(got, "zzzz") || !strings.Contains(got, "[... 150 characters truncated ...]") {
		t.Errorf("capped %q", got)
	}
	if h := strings.Index(got, "\n\n[..."); h < 50 || h > len(got)/2+20 {
		t.Errorf("head is %d bytes: want about two thirds of the kept text", h)
	}
	if !strings.Contains(tiny, "290 characters truncated") || strings.Contains(tiny, "aaa") {
		t.Errorf("a limit smaller than the marker keeps only the marker: %q", tiny)
	}
	if agent.CapOutput(s, 0) != s || agent.CapOutput(s, -1) != s || agent.CapOutput(s, 300) != s || agent.CapOutput("", 5) != "" {
		t.Error("text within the limit, or no limit, must come back unchanged")
	}
}

func TestTruncateCutsOnCharactersAndMarksTheCut(t *testing.T) {
	// Act / Assert
	if got := agent.Truncate("héllo wörld", 5); got != "héllo…" {
		t.Errorf("got %q", got)
	}
	if got := agent.Truncate("héllo", 5); got != "héllo" {
		t.Errorf("a text of exactly n characters was cut: %q", got)
	}
	if got := agent.Truncate("", 3); got != "" {
		t.Errorf("got %q", got)
	}
}
