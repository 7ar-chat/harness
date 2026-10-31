package runtime_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/ai/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/core/store"
)

// readOnlyTool is a fakeTool implementing tools.ReadOnly.
type readOnlyTool struct{ fakeTool }

func (readOnlyTool) ReadOnly() bool { return true }

// toolLog records tool starts/ends and the peak of concurrent calls.
type toolLog struct {
	mu      sync.Mutex
	entries []string
	running int
	peak    int
}

func (l *toolLog) enter(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, "start "+name)
	l.running++
	l.peak = max(l.peak, l.running)
}

func (l *toolLog) leave(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, "end "+name)
	l.running--
}

func calls(names ...string) llmtest.Turn {
	var chunks []*ai.Message
	for i, n := range names {
		chunks = append(chunks, llmtest.ToolCall(i, "c-"+n, n, `{}`))
	}
	return llmtest.Turn{Chunks: chunks}
}

func TestProcessRunsReadOnlyCallsInParallelAndReportsThemInOrder(t *testing.T) {
	// Consecutive read-only calls run concurrently (each waits for the others), yet their results are emitted and stored in the model's call order.
	// Arrange
	var started sync.WaitGroup
	started.Add(3)
	log := &toolLog{}
	reader := func(name string, delay time.Duration) *readOnlyTool {
		return &readOnlyTool{fakeTool{name: name, run: func(ctx context.Context, _ string) (string, error) {
			log.enter(name)
			defer log.leave(name)
			started.Done()
			done := make(chan struct{})
			go func() { started.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				return "ran alone", nil
			}
			time.Sleep(delay)
			return "read by " + name, nil
		}}}
	}
	r := newRig(t, calls("r1", "r2", "r3"), llmtest.Turn{Chunks: llmtest.Text("ok")})
	r.withTools(reader("r1", 30*time.Millisecond), reader("r2", 20*time.Millisecond), reader("r3", 0))
	th, run := r.queued("read three files")

	// Act
	got := r.process(run)

	// Assert
	evs := r.events(run.ID)
	checkStrictOrder(t, evs)
	res := find(evs, "TOOL_CALL_RESULT")
	if got.Status != store.StatusCompleted || len(res) != 3 {
		t.Fatalf("status %s results %d", got.Status, len(res))
	}
	for i, name := range []string{"r1", "r2", "r3"} {
		if res[i]["toolCallId"] != "c-"+name || res[i]["content"] != "read by "+name {
			t.Fatalf("result %d: %v", i, res[i])
		}
	}
	var stored []string
	for _, m := range r.messages(th.ID) {
		if m.Role == store.RoleTool {
			stored = append(stored, m.ToolCallID)
		}
	}
	if fmt.Sprint(stored) != "[c-r1 c-r2 c-r3]" || log.peak != 3 {
		t.Fatalf("stored %v peak %d", stored, log.peak)
	}
}

func TestProcessRunsWritesAloneBetweenReadOnlyBatches(t *testing.T) {
	// A write between read-only calls waits for the reads before it and runs before the reads after it; at most 4 reads run at once.
	// Arrange
	log := &toolLog{}
	tool := func(name string, ro bool) any {
		f := fakeTool{name: name, run: func(context.Context, string) (string, error) {
			log.enter(name)
			time.Sleep(10 * time.Millisecond)
			log.leave(name)
			return "ok", nil
		}}
		if ro {
			return &readOnlyTool{f}
		}
		return &f
	}
	r := newRig(t, calls("a", "b", "c", "d", "e", "w", "f"), llmtest.Turn{Chunks: llmtest.Text("ok")})
	for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
		r.withTools(tool(n, true).(*readOnlyTool))
	}
	r.withTools(tool("w", false).(*fakeTool))
	_, run := r.queued("go")

	// Act
	got := r.process(run)

	// Assert
	checkStrictOrder(t, r.events(run.ID))
	wStart, wEnd := index(log.entries, "start w"), index(log.entries, "end w")
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		if index(log.entries, "end "+n) > wStart {
			t.Fatalf("w started before %s ended: %v", n, log.entries)
		}
	}
	if got.Status != store.StatusCompleted || index(log.entries, "start f") < wEnd || log.peak != 4 {
		t.Fatalf("status %s peak %d log %v", got.Status, log.peak, log.entries)
	}
}
