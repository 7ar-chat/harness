package agent

import (
	"context"
	"fmt"
	"sync"
	"unicode/utf8"

	"github.com/7ar-chat/harness/ai"
)

// Schedule runs the tool calls of one step in the model's order. A run of two or more consecutive calls for which
// parallel reports true is handed to batch as a whole (it decides how to run them, typically with Parallel); every
// other call goes to single, alone. parallel is asked about a call only after the calls before it have run, so it may
// look at state they changed (an interrupt, a gate). The context is checked before each run; an error from batch or
// single stops the schedule and is returned.
func Schedule(ctx context.Context, calls []ai.ToolCall,
	parallel func(ctx context.Context, call ai.ToolCall) bool,
	single func(ctx context.Context, call ai.ToolCall) error,
	batch func(ctx context.Context, calls []ai.ToolCall) error) error {
	for i := 0; i < len(calls); {
		if err := ctx.Err(); err != nil {
			return err
		}
		j := i
		for j < len(calls) && parallel(ctx, calls[j]) {
			j++
		}
		if j-i >= 2 {
			if err := batch(ctx, calls[i:j]); err != nil {
				return err
			}
			i = j
			continue
		}
		if err := single(ctx, calls[i]); err != nil {
			return err
		}
		i++
	}
	return nil
}

// Parallel calls fn(k) for k in [0, n) with at most limit running at once (limit < 1: all at once) and returns when
// every call has returned.
func Parallel(n, limit int, fn func(k int)) {
	if limit < 1 || limit > n {
		limit = n
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for k := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			fn(k)
		}()
	}
	wg.Wait()
}

// SafeInvoke runs a tool and turns a panic in it into an error, so one broken tool cannot take the agent down.
func SafeInvoke(ctx context.Context, t ai.InvokableTool, arguments string) (out string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("the tool crashed: %v", r)
		}
	}()
	return t.InvokableRun(ctx, arguments)
}

// CapOutput keeps the head (two thirds) and the tail of s within max characters, with a marker between them
// saying how much was cut. max <= 0 or a shorter s: s unchanged.
func CapOutput(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	marker := fmt.Sprintf("\n\n[... %d characters truncated ...]\n\n", len(r)-max)
	keep := max - utf8.RuneCountInString(marker)
	if keep < 0 {
		keep = 0
	}
	head := keep * 2 / 3
	tail := keep - head
	return string(r[:head]) + marker + string(r[len(r)-tail:])
}

// Truncate cuts s to n characters and marks the cut with an ellipsis.
func Truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
