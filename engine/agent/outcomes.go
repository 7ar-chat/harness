package agent

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"sync"

	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/core/agui"
	"github.com/GhaziBenDahmane/harness/engine/tools"
)

// Harness v3 §1: finish_reason handling.
const (
	// OutputLimitResult answers every tool call of a message cut by the output limit (finish_reason=length).
	OutputLimitResult = "Your previous message was cut off by the output limit before the tool call was complete. Re-issue it in smaller pieces (e.g. write the file in parts)."
	// ContinueAfterLengthNote follows a text answer cut by the output limit (at most maxContinuations per run).
	ContinueAfterLengthNote = "Your previous answer was cut off by the output limit. Continue exactly where it stopped, without repeating what you already wrote."
	// ContentFilterResult answers the tool calls announced by an answer the content filter stopped.
	ContentFilterResult = "(stopped by the content filter)"
	maxContinuations    = 2
)

// emptyDetail is the logged detail of an empty_answer failure.
func emptyDetail(out *stepOutput) string {
	if out.refusal != "" {
		return "refusal: " + truncate(out.refusal, 500)
	}
	return fmt.Sprintf("empty completion (finish_reason=%q)", out.finishReason())
}

// closeAnnounced gives the calls a stopped answer announced an error result.
func (st *runState) closeAnnounced(ctx context.Context, out *stepOutput, content string) error {
	for _, id := range out.announced {
		if err := st.emit(ctx, agui.New(agui.ToolCallResult, map[string]any{
			"messageId": uuid.NewString(), "toolCallId": id, "content": content, "role": "tool", "metadata": map[string]any{"isError": true},
		})); err != nil {
			return err
		}
	}
	return nil
}

// passback reports whether the run's model gets its reasoning back on assistant messages.
func (st *runState) passback() bool {
	f := st.a.Harness.ReasoningPassback
	return f != nil && f(st.in.Run.Model)
}

// maxParallelTools bounds the read-only calls running at once.
const maxParallelTools = 4

// runTools runs the tool calls of a step: consecutive read-only calls
// (tools.ReadOnly, not gated by an approval) run in parallel, the others alone,
// and every result is emitted and recorded in the model's call order.
func (st *runState) runTools(ctx context.Context, calls []ai.ToolCall, toolMap map[string]ai.BaseTool) error {
	for i := 0; i < len(calls); {
		if err := ctx.Err(); err != nil {
			return err
		}
		j := i
		for j < len(calls) && st.parallelizable(ctx, calls[j], toolMap) {
			j++
		}
		if j-i >= 2 {
			if err := st.runParallel(ctx, calls[i:j], toolMap); err != nil {
				return err
			}
			i = j
			continue
		}
		if err := st.callTool(ctx, calls[i], toolMap); err != nil {
			return err
		}
		i++
	}
	st.addToolImages(st.takeToolImages())
	return nil
}

// parallelizable reports a call that may run concurrently with its neighbours.
func (st *runState) parallelizable(ctx context.Context, tc ai.ToolCall, toolMap map[string]ai.BaseTool) bool {
	ro, ok := toolMap[tc.Function.Name].(tools.ReadOnly)
	return ok && ro.ReadOnly() && st.h.interrupt == nil && st.gate(ctx, tc, toolMap) == nil
}

// runParallel runs read-only calls concurrently (at most maxParallelTools at
// once), then finishes them in order.
func (st *runState) runParallel(ctx context.Context, calls []ai.ToolCall, toolMap map[string]ai.BaseTool) error {
	type outcome struct {
		content string
		err     error
		em      *toolEmitter
	}
	results := make([]outcome, len(calls))
	looping := make([]string, len(calls))
	for k, tc := range calls {
		looping[k] = st.g.looping(tc) // in order, like sequential calls
		results[k].em = &toolEmitter{st: st, ctx: ctx, buffered: true}
	}
	sem := make(chan struct{}, maxParallelTools)
	var wg sync.WaitGroup
	for k, tc := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[k].content, results[k].err = st.execTool(ctx, tc, toolMap, results[k].em, looping[k])
		}()
	}
	wg.Wait()
	for k, tc := range calls {
		if err := st.finishTool(ctx, tc, results[k].em, results[k].content, results[k].err); err != nil {
			return err
		}
	}
	return nil
}
