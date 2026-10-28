package runtime_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	goopenai "github.com/meguminnnnnnnnn/go-openai"

	"github.com/GhaziBenDahmane/harness/engine/agent"
	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/store"
)

// recordSleeps makes the agent's retry waits instant and records them.
func recordSleeps(r *rig) *[]time.Duration {
	var mu sync.Mutex
	delays := &[]time.Duration{}
	r.agent.Sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		*delays = append(*delays, d)
		mu.Unlock()
		return ctx.Err()
	}
	return delays
}

func TestProcessRetriesTheWholeStepWhenTheStreamBreaks(t *testing.T) {
	// A stream cut mid-step is re-issued: the failed attempt's text and call are closed, the call gets a retried error result, work.retry precedes the new attempt, and nothing of it is persisted.
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: []*schema.Message{{Role: schema.Assistant, Content: "Let me"}, llmtest.ToolCall(0, "c1", "write_file", `{"path":"a.txt","con`)},
			FailAfter: &goopenai.APIError{HTTPStatusCode: 502, Message: "bad gateway"}},
		llmtest.Turn{Chunks: []*schema.Message{{Role: schema.Assistant, Content: "Let me write it."}, llmtest.ToolCall(0, "c2", "write_file", `{"path":"a.txt","content":"hi"}`)}},
		llmtest.Turn{Chunks: llmtest.Text("done")},
	)
	recordSleeps(r)
	th, run := r.queued("write a.txt")

	// Act
	got := r.process(run)

	// Assert
	evs := r.events(run.ID)
	checkStrictOrder(t, evs)
	retries := find(evs, "CUSTOM:work.retry")
	results := find(evs, "TOOL_CALL_RESULT")
	if got.Status != store.StatusCompleted || len(retries) != 1 || len(results) != 2 {
		t.Fatalf("status %s retries %v results %d types %v", got.Status, retries, len(results), types(evs))
	}
	if v := retries[0]["value"].(map[string]any); v["attempt"] != float64(2) || v["reason"] != "transient" {
		t.Fatalf("retry %v", v)
	}
	md, _ := results[0]["metadata"].(map[string]any)
	if results[0]["toolCallId"] != "c1" || results[0]["content"] != agent.StreamRetriedResult || md["isError"] != true || md["retried"] != true {
		t.Fatalf("failed attempt's result %v", results[0])
	}
	tys := types(evs)
	if index(tys, "CUSTOM:work.retry") < index(tys, "TOOL_CALL_RESULT") {
		t.Fatalf("work.retry before the retried result: %v", tys)
	}
	for _, m := range r.messages(th.ID) {
		if m.Content == "Let me" || (len(m.ToolCalls) > 0 && m.ToolCalls[0].ID == "c1") || m.ToolCallID == "c1" {
			t.Fatalf("the failed attempt was persisted: %+v", m)
		}
	}
}

func TestProcessFailsWithModelTimeoutWhenTheStreamStalls(t *testing.T) {
	// A stream silent for LLM_STREAM_IDLE_SECONDS is cut and retried; when every attempt stalls the run ends with RUN_ERROR model_timeout.
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: llmtest.Text("par"), Block: true},
		llmtest.Turn{Chunks: llmtest.Text("again"), Block: true},
	)
	r.agent.StreamIdleTimeout = 50 * time.Millisecond
	r.agent.StepRetries = 1
	recordSleeps(r)
	_, run := r.queued("q")

	// Act
	got := r.process(run)

	// Assert
	evs := r.events(run.ID)
	checkStrictOrder(t, evs)
	retries := find(evs, "CUSTOM:work.retry")
	if got.Status != store.StatusFailed || last(evs)["code"] != "model_timeout" || len(r.model.Calls()) != 2 || len(retries) != 1 ||
		retries[0]["value"].(map[string]any)["reason"] != "timeout" {
		t.Fatalf("status %s last %v calls %d types %v", got.Status, last(evs), len(r.model.Calls()), types(evs))
	}
}

func TestProcessTimesOutAModelThatNeverSendsAChunk(t *testing.T) {
	// LLM_TIMEOUT_SECONDS bounds the wait for the first chunk: a silent model is retried, then the run fails with model_timeout.
	// Arrange
	r := newRig(t, llmtest.Turn{Block: true}, llmtest.Turn{Block: true})
	r.agent.ModelTimeout = 50 * time.Millisecond
	r.agent.StepRetries = 1
	recordSleeps(r)
	_, run := r.queued("q")

	// Act
	got := r.process(run)

	// Assert
	evs := r.events(run.ID)
	checkStrictOrder(t, evs)
	if got.Status != store.StatusFailed || last(evs)["code"] != "model_timeout" || len(r.model.Calls()) != 2 {
		t.Fatalf("status %s last %v calls %d", got.Status, last(evs), len(r.model.Calls()))
	}
}

func TestProcessKeepsASlowButLiveStream(t *testing.T) {
	// The idle timeout is per gap between chunks, not per call: a stream slower in total than the budget completes.
	// Arrange
	r := newRig(t, llmtest.Turn{Chunks: llmtest.Text("a", "b", "c", "d", "e", "f"), ChunkDelay: 30 * time.Millisecond})
	r.agent.StreamIdleTimeout = 120 * time.Millisecond
	r.agent.ModelTimeout = 120 * time.Millisecond
	th, run := r.queued("q")

	// Act
	got := r.process(run)

	// Assert
	checkStrictOrder(t, r.events(run.ID))
	if msgs := r.messages(th.ID); got.Status != store.StatusCompleted || len(r.model.Calls()) != 1 || msgs[len(msgs)-1].Content != "abcdef" {
		t.Fatalf("status %s calls %d", got.Status, len(r.model.Calls()))
	}
}

func TestProcessHonoursRetryAfterOnRateLimits(t *testing.T) {
	// A 429 waits at least the provider's Retry-After before the retry, which work.retry announces as rate_limited.
	// Arrange
	r := newRig(t,
		llmtest.Turn{Err: &goopenai.APIError{HTTPStatusCode: 429, Message: "Request too large for gpt-5 on tokens per min (TPM)"}, RetryAfter: 7 * time.Second},
		llmtest.Turn{Chunks: llmtest.Text("ok")},
	)
	delays := recordSleeps(r)
	_, run := r.queued("q")

	// Act
	got := r.process(run)

	// Assert
	evs := r.events(run.ID)
	checkStrictOrder(t, evs)
	retries := find(evs, "CUSTOM:work.retry")
	if got.Status != store.StatusCompleted || len(*delays) != 1 || (*delays)[0] < 7*time.Second || len(retries) != 1 ||
		retries[0]["value"].(map[string]any)["reason"] != "rate_limited" || len(find(evs, "CUSTOM:work.compaction")) != 0 {
		t.Fatalf("status %s delays %v types %v", got.Status, *delays, types(evs))
	}
}

func TestProcessFailsWithRateLimitedWhenRetriesRunOut(t *testing.T) {
	// 429 on every attempt ends the run with RUN_ERROR rate_limited after LLM_STEP_RETRIES retries with growing, jittered delays.
	// Arrange
	limited := llmtest.Turn{Err: &goopenai.APIError{HTTPStatusCode: 429, Message: "slow down"}}
	r := newRig(t, limited, limited, limited, limited)
	delays := recordSleeps(r)
	_, run := r.queued("q")

	// Act
	got := r.process(run)

	// Assert
	evs := r.events(run.ID)
	checkStrictOrder(t, evs)
	d := *delays
	if got.Status != store.StatusFailed || last(evs)["code"] != "rate_limited" || len(r.model.Calls()) != 4 || len(d) != 3 ||
		d[0] < r.agent.RetryBackoff/2 || d[2] < 2*r.agent.RetryBackoff {
		t.Fatalf("status %s last %v calls %d delays %v", got.Status, last(evs), len(r.model.Calls()), d)
	}
}

func TestProcessRefusesToolCallsCutByTheOutputLimit(t *testing.T) {
	// finish_reason=length never runs the message's tool calls: they get the "cut off" error result and the model continues.
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: []*schema.Message{llmtest.Finish(llmtest.ToolCall(0, "c1", "write_file", `{"path":"a.txt","content":"aaaa`), "length")}},
		llmtest.Turn{Chunks: llmtest.Text("ok")},
	)
	_, run := r.queued("write a long file")

	// Act
	got := r.process(run)

	// Assert
	evs := r.events(run.ID)
	checkStrictOrder(t, evs)
	res := find(evs, "TOOL_CALL_RESULT")
	if got.Status != store.StatusCompleted || len(res) != 1 || res[0]["content"] != agent.OutputLimitResult || len(r.backend.Created()) != 0 {
		t.Fatalf("status %s results %v sandboxes %d", got.Status, res, len(r.backend.Created()))
	}
	second := r.model.Calls()[1].Messages
	if tm := second[len(second)-1]; tm.Role != schema.Tool || tm.Content != agent.OutputLimitResult {
		t.Fatalf("model did not get the cut-off result: %+v", tm)
	}
}

func TestProcessContinuesAnAnswerCutByTheOutputLimit(t *testing.T) {
	// A text answer cut by finish_reason=length is kept and the model is asked to continue where it stopped.
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: []*schema.Message{llmtest.Finish(&schema.Message{Role: schema.Assistant, Content: "part one"}, "length")}},
		llmtest.Turn{Chunks: llmtest.Text(" and part two")},
	)
	_, run := r.queued("explain")

	// Act
	got := r.process(run)

	// Assert
	checkStrictOrder(t, r.events(run.ID))
	calls := r.model.Calls()
	if got.Status != store.StatusCompleted || len(calls) != 2 {
		t.Fatalf("status %s calls %d", got.Status, len(calls))
	}
	msgs := calls[1].Messages
	if msgs[len(msgs)-2].Content != "part one" || msgs[len(msgs)-1].Content != agent.ContinueAfterLengthNote {
		t.Fatalf("second call ends with %+v / %+v", msgs[len(msgs)-2], msgs[len(msgs)-1])
	}
}

func TestProcessFailsWithContentFilter(t *testing.T) {
	// finish_reason=content_filter ends the run with RUN_ERROR content_filter and persists no half answer.
	// Arrange
	r := newRig(t, llmtest.Turn{Chunks: []*schema.Message{llmtest.Finish(&schema.Message{Role: schema.Assistant, Content: "Here is"}, "content_filter")}})
	th, run := r.queued("q")

	// Act
	got := r.process(run)

	// Assert
	evs := r.events(run.ID)
	checkStrictOrder(t, evs)
	if got.Status != store.StatusFailed || last(evs)["code"] != "content_filter" || len(r.messages(th.ID)) != 1 {
		t.Fatalf("status %s last %v messages %d", got.Status, last(evs), len(r.messages(th.ID)))
	}
}

func TestProcessFailsWithEmptyAnswerAfterOneNudge(t *testing.T) {
	// Two empty answers in a row (after one nudge) end the run with RUN_ERROR empty_answer, not a silent success.
	// Arrange
	r := newRig(t, llmtest.Turn{Chunks: llmtest.Text("")}, llmtest.Turn{Chunks: llmtest.Text("")})
	_, run := r.queued("q")

	// Act
	got := r.process(run)

	// Assert
	evs := r.events(run.ID)
	checkStrictOrder(t, evs)
	calls := r.model.Calls()
	if got.Status != store.StatusFailed || last(evs)["code"] != "empty_answer" || len(calls) != 2 ||
		calls[1].Messages[len(calls[1].Messages)-1].Content != agent.EmptyAnswerNudge {
		t.Fatalf("status %s last %v calls %d", got.Status, last(evs), len(calls))
	}
}

func TestProcessTreatsARefusalLikeAnEmptyAnswer(t *testing.T) {
	// A refusal gets one nudge; a second refusal ends the run with empty_answer.
	// Arrange
	r := newRig(t, llmtest.Turn{Refusal: "I'm sorry, I can't help with that."}, llmtest.Turn{Refusal: "I'm sorry."})
	_, run := r.queued("q")

	// Act
	got := r.process(run)

	// Assert
	evs := r.events(run.ID)
	checkStrictOrder(t, evs)
	if got.Status != store.StatusFailed || last(evs)["code"] != "empty_answer" || len(r.model.Calls()) != 2 {
		t.Fatalf("status %s last %v calls %d", got.Status, last(evs), len(r.model.Calls()))
	}
}

func TestProcessKeepsParallelCallsThatShareAnIndex(t *testing.T) {
	// A backend numbering every parallel call index 0 with distinct ids yields two calls instead of failing the stream.
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "c1", "list_files", `{}`), llmtest.ToolCall(0, "c2", "list_files", `{"path":"/workspace"}`)}},
		llmtest.Turn{Chunks: llmtest.Text("ok")},
	)
	th, run := r.queued("q")

	// Act
	got := r.process(run)

	// Assert
	evs := r.events(run.ID)
	checkStrictOrder(t, evs)
	calls := r.messages(th.ID)[1].ToolCalls
	if got.Status != store.StatusCompleted || len(calls) != 2 || calls[1].ID != "c2" || calls[1].Function.Arguments != `{"path":"/workspace"}` {
		t.Fatalf("status %s calls %+v types %v", got.Status, calls, types(evs))
	}
}

func TestProcessClosesTextAndCallsBeforeTheNextOneStarts(t *testing.T) {
	// The text message ends before the first tool call starts and each call ends before the next starts (strict AG-UI order).
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: []*schema.Message{{Role: schema.Assistant, Content: "Let me look."},
			llmtest.ToolCall(0, "c1", "list_files", `{}`), llmtest.ToolCall(1, "c2", "list_files", `{"path":"/workspace"}`)}},
		llmtest.Turn{Chunks: llmtest.Text("ok")},
	)
	_, run := r.queued("q")

	// Act
	got := r.process(run)

	// Assert
	if got.Status != store.StatusCompleted {
		t.Fatalf("status %s", got.Status)
	}
	checkStrictOrder(t, r.events(run.ID))
}

func TestProcessReportsCachedInputTokens(t *testing.T) {
	// Cached prompt tokens reported by the provider reach work.usage and the RUN_FINISHED usage as cachedInputTokens.
	// Arrange
	r := newRig(t, llmtest.Turn{Chunks: []*schema.Message{llmtest.WithCachedUsage(&schema.Message{Role: schema.Assistant, Content: "ok"}, 1000, 800, 10)}})
	_, run := r.queued("q")

	// Act
	r.process(run)

	// Assert
	checkStrictOrder(t, r.events(run.ID))
	evs := r.events(run.ID)
	usage := find(evs, "CUSTOM:work.usage")
	fin := last(evs)["usage"].([]any)[0].(map[string]any)
	if len(usage) != 1 || usage[0]["value"].(map[string]any)["cachedInputTokens"] != float64(800) || fin["cachedInputTokens"] != float64(800) {
		t.Fatalf("usage %v finished %v", usage, fin)
	}
}

var _ = strings.Contains
