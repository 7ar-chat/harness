package agent_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/ai/agent"
	"github.com/GhaziBenDahmane/harness/ai/llm"
	"github.com/GhaziBenDahmane/harness/ai/openai"
)

// streamModel plays one scripted stream per Stream call.
type streamModel struct {
	mu    sync.Mutex
	plays []play
	calls int
}

type play struct {
	openErr error
	chunks  []*ai.Message
	// failAfter ends the stream with this error after the chunks.
	failAfter error
	// stall keeps the stream open and silent after the chunks until the context ends.
	stall bool
}

func (m *streamModel) Stream(ctx context.Context, _ []*ai.Message, _ ...ai.Option) (*ai.StreamReader[*ai.Message], error) {
	m.mu.Lock()
	p := m.plays[m.calls]
	m.calls++
	m.mu.Unlock()
	if p.openErr != nil {
		return nil, p.openErr
	}
	sr, sw := ai.Pipe[*ai.Message](len(p.chunks) + 1)
	go func() {
		defer sw.Close()
		for _, c := range p.chunks {
			sw.Send(c, nil)
		}
		switch {
		case p.failAfter != nil:
			sw.Send(nil, p.failAfter)
		case p.stall:
			<-ctx.Done()
			sw.Send(nil, ctx.Err())
		}
	}()
	return sr, nil
}
func (m *streamModel) Generate(context.Context, []*ai.Message, ...ai.Option) (*ai.Message, error) {
	return nil, errors.New("not used")
}

// rec records what the Caller told its attempts.
type rec struct {
	mu     sync.Mutex
	events []string
}

func (r *rec) add(s string) { r.mu.Lock(); r.events = append(r.events, s); r.mu.Unlock() }
func (r *rec) log() string  { r.mu.Lock(); defer r.mu.Unlock(); return strings.Join(r.events, ",") }

type recAttempt struct{ r *rec }

func (a recAttempt) Chunk(_ context.Context, c *ai.Message) error {
	a.r.add("chunk:" + c.Content)
	return nil
}
func (a recAttempt) Settle(_ context.Context, broken bool) error {
	if broken {
		a.r.add("settle:broken")
	} else {
		a.r.add("settle")
	}
	return nil
}
func (a recAttempt) Abort(_ context.Context, retry bool) error {
	if retry {
		a.r.add("abort:retry")
	} else {
		a.r.add("abort:final")
	}
	return nil
}

func text(s string) *ai.Message { return &ai.Message{Role: ai.RoleAssistant, Content: s} }

func TestCallerStreamsChunksToTheAttemptAndReturnsThem(t *testing.T) {
	r := &rec{}
	m := &streamModel{plays: []play{{chunks: []*ai.Message{text("a"), text("b")}}}}

	out, err := (&agent.Caller{}).Call(context.Background(), m, nil, nil, func() agent.Attempt { return recAttempt{r} })

	if err != nil || len(out.Chunks) != 2 || r.log() != "chunk:a,chunk:b,settle" {
		t.Fatalf("out %+v err %v log %s", out, err, r.log())
	}
}

func TestCallerRetriesTransientFailuresAbortingEachFailedAttempt(t *testing.T) {
	r := &rec{}
	var slept []time.Duration
	var retried []string
	m := &streamModel{plays: []play{
		{chunks: []*ai.Message{text("par")}, failAfter: &openai.APIError{Status: 502, Message: "bad gateway"}},
		{openErr: &openai.APIError{Status: 503, Message: "unavailable"}},
		{chunks: []*ai.Message{text("ok")}},
	}}
	c := &agent.Caller{
		Retries: 3, Backoff: time.Millisecond,
		Sleep: func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil },
		OnRetry: func(_ context.Context, attempt int, class llm.ErrorClass, _ error) error {
			retried = append(retried, string(class))
			return nil
		},
	}

	out, err := c.Call(context.Background(), m, nil, nil, func() agent.Attempt { return recAttempt{r} })

	if err != nil || len(out.Chunks) != 1 || out.Chunks[0].Content != "ok" {
		t.Fatalf("out %+v err %v", out, err)
	}
	// the second attempt never got a stream, so it has no Attempt to abort
	if got := r.log(); got != "chunk:par,abort:retry,chunk:ok,settle" {
		t.Fatalf("log %s", got)
	}
	if len(slept) != 2 || m.calls != 3 || strings.Join(retried, ",") != "transient,transient" {
		t.Fatalf("slept %v calls %d retried %v", slept, m.calls, retried)
	}
}

func TestCallerStopsOnAFatalErrorAndReportsItsClass(t *testing.T) {
	r := &rec{}
	m := &streamModel{plays: []play{{chunks: []*ai.Message{text("x")}, failAfter: &openai.APIError{Status: 401, Message: "bad key"}}}}

	_, err := (&agent.Caller{Retries: 5, Sleep: func(context.Context, time.Duration) error { return nil }}).
		Call(context.Background(), m, nil, nil, func() agent.Attempt { return recAttempt{r} })

	var ce *agent.CallError
	if !errors.As(err, &ce) || ce.Class != llm.ClassFatal || m.calls != 1 || r.log() != "chunk:x,abort:final" {
		t.Fatalf("err %v calls %d log %s", err, m.calls, r.log())
	}
}

func TestCallerGivesUpAfterTheRetries(t *testing.T) {
	m := &streamModel{plays: []play{{openErr: &openai.APIError{Status: 429}}, {openErr: &openai.APIError{Status: 429}}, {openErr: &openai.APIError{Status: 429}}}}

	_, err := (&agent.Caller{Retries: 2, Sleep: func(context.Context, time.Duration) error { return nil }}).
		Call(context.Background(), m, nil, nil, func() agent.Attempt { return recAttempt{&rec{}} })

	var ce *agent.CallError
	if !errors.As(err, &ce) || ce.Class != llm.ClassRateLimited || m.calls != 3 {
		t.Fatalf("err %v calls %d (want 1 call + 2 retries, then the failure)", err, m.calls)
	}
}

func TestWatchdogCutsASilentStreamAndTheCallIsRetriedAsATimeout(t *testing.T) {
	r := &rec{}
	m := &streamModel{plays: []play{{stall: true}, {chunks: []*ai.Message{text("late")}}}}
	c := &agent.Caller{FirstChunk: 40 * time.Millisecond, Retries: 1, Sleep: func(context.Context, time.Duration) error { return nil }}

	start := time.Now()
	out, err := c.Call(context.Background(), m, nil, nil, func() agent.Attempt { return recAttempt{r} })

	if err != nil || out.Chunks[0].Content != "late" || r.log() != "abort:retry,chunk:late,settle" {
		t.Fatalf("out %+v err %v log %s", out, err, r.log())
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("took %v, the watchdog should fire after ~40ms", time.Since(start))
	}
}

func TestWatchdogErrorNamesTheStallWhenNoRetriesRemain(t *testing.T) {
	m := &streamModel{plays: []play{{chunks: []*ai.Message{text("a")}, stall: true}}}
	c := &agent.Caller{FirstChunk: time.Second, Idle: 40 * time.Millisecond}

	_, err := c.Call(context.Background(), m, nil, nil, func() agent.Attempt { return recAttempt{&rec{}} })

	var ce *agent.CallError
	if !errors.As(err, &ce) || ce.Class != llm.ClassTimeout || !errors.Is(err, agent.ErrStalled) {
		t.Fatalf("err %v, want a timeout caused by ErrStalled", err)
	}
}

func TestCancellingTheContextReturnsThePartialStream(t *testing.T) {
	r := &rec{}
	ctx, cancel := context.WithCancel(context.Background())
	m := &streamModel{plays: []play{{chunks: []*ai.Message{text("par")}, stall: true}}}
	at := cancelOnChunk{recAttempt{r}, cancel}

	out, err := (&agent.Caller{}).Call(ctx, m, nil, nil, func() agent.Attempt { return at })

	if !errors.Is(err, context.Canceled) || out == nil || len(out.Chunks) != 1 || r.log() != "chunk:par,settle:broken" {
		t.Fatalf("out %+v err %v log %s", out, err, r.log())
	}
}

type cancelOnChunk struct {
	recAttempt
	cancel context.CancelFunc
}

func (a cancelOnChunk) Chunk(ctx context.Context, c *ai.Message) error {
	err := a.recAttempt.Chunk(ctx, c)
	a.cancel()
	return err
}

func TestAHookErrorEndsTheCallWithoutRetry(t *testing.T) {
	boom := errors.New("event log down")
	m := &streamModel{plays: []play{{chunks: []*ai.Message{text("a")}}}}

	_, err := (&agent.Caller{Retries: 3}).Call(context.Background(), m, nil, nil, func() agent.Attempt { return failingAttempt{boom} })

	if !errors.Is(err, boom) || m.calls != 1 {
		t.Fatalf("err %v calls %d", err, m.calls)
	}
}

type failingAttempt struct{ err error }

func (a failingAttempt) Chunk(context.Context, *ai.Message) error { return a.err }
func (a failingAttempt) Settle(context.Context, bool) error       { return nil }
func (a failingAttempt) Abort(context.Context, bool) error        { return nil }

func TestScheduleBatchesConsecutiveParallelCallsAndKeepsOrder(t *testing.T) {
	calls := []ai.ToolCall{{ID: "r1"}, {ID: "r2"}, {ID: "w"}, {ID: "r3"}, {ID: "r4"}, {ID: "r5"}}
	var order []string
	err := agent.Schedule(context.Background(), calls,
		func(_ context.Context, c ai.ToolCall) bool { return strings.HasPrefix(c.ID, "r") },
		func(_ context.Context, c ai.ToolCall) error { order = append(order, "single:"+c.ID); return nil },
		func(_ context.Context, b []ai.ToolCall) error {
			ids := make([]string, len(b))
			for i, c := range b {
				ids[i] = c.ID
			}
			order = append(order, "batch:"+strings.Join(ids, "+"))
			return nil
		})

	if err != nil || strings.Join(order, " ") != "batch:r1+r2 single:w batch:r3+r4+r5" {
		t.Fatalf("order %v err %v", order, err)
	}
}

func TestScheduleJudgesACallOnlyAfterTheCallsBeforeItRan(t *testing.T) {
	// "w" (an approval request, say) changes what later calls may do: r2 is read-only, but not once w interrupted the run.
	calls := []ai.ToolCall{{ID: "r1"}, {ID: "w"}, {ID: "r2"}, {ID: "r3"}}
	interrupted := false
	var order []string
	err := agent.Schedule(context.Background(), calls,
		func(_ context.Context, c ai.ToolCall) bool { return strings.HasPrefix(c.ID, "r") && !interrupted },
		func(_ context.Context, c ai.ToolCall) error {
			order = append(order, "single:"+c.ID)
			if c.ID == "w" {
				interrupted = true
			}
			return nil
		},
		func(_ context.Context, b []ai.ToolCall) error { order = append(order, "batch"); return nil })

	if err != nil || strings.Join(order, " ") != "single:r1 single:w single:r2 single:r3" {
		t.Fatalf("order %v err %v (r2 and r3 must not be batched: they were judged before w ran)", order, err)
	}
}

func TestScheduleStopsAtTheFirstErrorOrACancelledContext(t *testing.T) {
	boom := errors.New("boom")
	n := 0
	err := agent.Schedule(context.Background(), []ai.ToolCall{{}, {}, {}},
		func(context.Context, ai.ToolCall) bool { return false },
		func(context.Context, ai.ToolCall) error { n++; return boom },
		func(context.Context, []ai.ToolCall) error { return nil })
	if !errors.Is(err, boom) || n != 1 {
		t.Fatalf("err %v after %d calls", err, n)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = agent.Schedule(ctx, []ai.ToolCall{{}}, func(context.Context, ai.ToolCall) bool { return false },
		func(context.Context, ai.ToolCall) error { t.Fatal("ran under a cancelled context"); return nil },
		func(context.Context, []ai.ToolCall) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
}

func TestParallelBoundsHowManyRunAtOnce(t *testing.T) {
	var mu sync.Mutex
	active, peak, done := 0, 0, 0
	agent.Parallel(8, 3, func(int) {
		mu.Lock()
		active++
		peak = max(peak, active)
		mu.Unlock()
		time.Sleep(15 * time.Millisecond)
		mu.Lock()
		active--
		done++
		mu.Unlock()
	})

	if peak > 3 || peak < 2 || done != 8 {
		t.Fatalf("peak %d done %d", peak, done)
	}
}

type panicTool struct{}

func (panicTool) Info(context.Context) (*ai.ToolInfo, error) { return &ai.ToolInfo{Name: "p"}, nil }
func (panicTool) InvokableRun(context.Context, string) (string, error) {
	var m map[string]int
	m["x"] = 1
	return "", nil
}

func TestSafeInvokeTurnsAPanicIntoAnError(t *testing.T) {
	_, err := agent.SafeInvoke(context.Background(), panicTool{}, "{}")

	if err == nil || !strings.Contains(err.Error(), "the tool crashed") {
		t.Fatalf("err %v", err)
	}
}

func TestCapOutputKeepsHeadAndTailWithinTheLimit(t *testing.T) {
	long := strings.Repeat("a", 500) + strings.Repeat("z", 500)

	got := agent.CapOutput(long, 200)

	if n := len([]rune(got)); n > 200 || !strings.HasPrefix(got, "aaaa") || !strings.HasSuffix(got, "zzzz") || !strings.Contains(got, "characters truncated") {
		t.Fatalf("len %d: %q", n, got)
	}
	if agent.CapOutput("short", 200) != "short" || agent.CapOutput(long, 0) != long {
		t.Fatal("a short text or no limit must pass through unchanged")
	}
}
