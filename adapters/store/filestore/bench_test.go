package filestore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/core/agui"
	"github.com/GhaziBenDahmane/harness/core/store"
)

// runSteps plays a long agent thread: each step is an assistant message with a
// tool call, the tool result (~2 KB), and the ~8 AG-UI events of the step.
func runSteps(tb testing.TB, s *Store, steps int) (threadID string) {
	ctx := context.Background()
	th, err := s.CreateThread(ctx, "alice", "long")
	if err != nil {
		tb.Fatal(err)
	}
	run, _, err := s.CreateRun(ctx, store.NewRun{RunID: "run", ThreadID: th.ID, UserLogin: "alice", Model: "m", Message: store.Message{Role: store.RoleUser, Content: "work"}})
	if err != nil {
		tb.Fatal(err)
	}
	if _, err := s.ClaimRun(ctx, run.ID, "rt"); err != nil {
		tb.Fatal(err)
	}
	output := strings.Repeat("x", 2000)
	var seq int64
	for i := 0; i < steps; i++ {
		call := fmt.Sprintf("call-%d", i)
		err := s.AppendMessages(ctx,
			&store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleAssistant, Content: "thinking about step",
				ToolCalls: []store.ToolCall{{ID: call, Type: "function", Function: store.ToolCallFunction{Name: "bash", Arguments: `{"command":"ls -la /workspace"}`}}}},
			&store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleTool, ToolCallID: call, Content: output})
		if err != nil {
			tb.Fatal(err)
		}
		evs := make([]store.Event, 8)
		for k := range evs {
			seq++
			evs[k] = store.Event{Seq: seq, Type: agui.TextMessageContent, Data: json.RawMessage(`{"type":"TEXT_MESSAGE_CONTENT","messageId":"m","delta":"some streamed text delta","timestamp":1700000000000}`)}
		}
		if err := s.AppendEvents(ctx, run.ID, evs); err != nil {
			tb.Fatal(err)
		}
	}
	if err := s.FinishRun(ctx, run.ID, "rt", store.StatusCompleted, "", ""); err != nil {
		tb.Fatal(err)
	}
	return th.ID
}

func heap() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// TestReportThread2000Steps prints memory, disk and timing for a 2,000-step
// thread (go test -run Report -v ./store/filestore).
func TestReportThread2000Steps(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	dir := t.TempDir()
	base := heap()
	s := open(t, dir)
	t0 := time.Now()
	id := runSteps(t, s, 2000)
	write := time.Since(t0)
	ok(t, s.Close())
	disk := must(os.Stat(filepath.Join(dir, journalName))).Size()
	grown := heap()
	t0 = time.Now()
	s2 := open(t, dir)
	reopen := time.Since(t0)
	live := heap()
	n := len(must(s2.ListMessages(context.Background(), id)))
	evs := len(must(s2.ListEvents(context.Background(), "run", 0, 1<<30)))
	t.Logf("2000 steps: %d messages, %d events", n, evs)
	t.Logf("write: %v total (%.0f us/step incl. journaling)", write, float64(write.Microseconds())/2000)
	t.Logf("journal on disk: %.2f MB", float64(disk)/1e6)
	t.Logf("reopen (replay): %v", reopen)
	t.Logf("heap held by the reopened store: %.2f MB (baseline %.2f MB)", float64(live-min(live, base))/1e6, float64(base)/1e6)
	_ = grown
	s2.Close()
	if n != 4001 || evs != 16000 {
		t.Fatalf("counts %d %d", n, evs)
	}
}

func BenchmarkThread2000Steps(b *testing.B) {
	for i := 0; i < b.N; i++ {
		dir := b.TempDir()
		s, err := Open(dir)
		if err != nil {
			b.Fatal(err)
		}
		runSteps(b, s, 2000)
		_ = s.Close()
		if i == 0 {
			b.ReportMetric(float64(must(os.Stat(filepath.Join(dir, journalName))).Size())/1e6, "MB-journal")
		}
	}
}
