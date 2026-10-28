//go:build unix

// Package longtest drives the real harness (local sandbox, coding profile)
// through long scripted sessions: a fake model that behaves like an agent doing
// 50-200 steps of read_file / search_files / edit_file / execute_command in a
// generated repository, with realistic tool-output sizes (some 20-60 KB, which
// trigger spill and masking). It exists to check what only long sessions
// break: compaction, masking, prompt-prefix stability, steering, memory and
// goroutines, and the stop paths (retry, loop guard, step budget).
//
// Nothing here needs a network or a key. Live-ready long tasks are in
// bench/tasks_long.go.
package longtest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness"
	"github.com/GhaziBenDahmane/harness/agui"
	"github.com/GhaziBenDahmane/harness/compaction"
	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/store"
	"github.com/GhaziBenDahmane/harness/tools"
)

// Markers the assertions look for after compaction.
const (
	AgentsMarker = "LONGTEST-AGENTS-MARKER-7731"
	Request      = "Tune every module in pkg/ (replace each TODO marker with DONE), keeping the notes in AGENTS.md. LONGTEST-REQUEST-MARKER-4419"
	PlanTitle    = "Tune all modules LONGTEST-PLAN-MARKER-2208"
)

const (
	Modules  = 40
	BigFiles = 6
)

// Repo returns the generated repository: AGENTS.md, Modules packages of one
// ~3 KB file each (with one TODO marker to edit), and BigFiles 60 KB logs.
func Repo() map[string]string {
	files := map[string]string{
		"AGENTS.md": "# Agent notes\n\nRun the tests with `make test`. " + AgentsMarker + "\nKeep functions short; no globals.\n",
		"go.mod":    "module example.com/sim\n\ngo 1.22\n",
	}
	for k := 0; k < Modules; k++ {
		var b strings.Builder
		fmt.Fprintf(&b, "package m%02d\n\n// TODO(m%02d): tune\n", k, k)
		for i := 0; i < 40; i++ {
			fmt.Fprintf(&b, "// Compute%d_%d returns a deterministic value for module %d.\nfunc Compute%d_%d(x int) int { return x*%d + %d }\n\n", k, i, k, k, i, i+1, k)
		}
		files[fmt.Sprintf("pkg/m%02d/f%02d.go", k, k)] = b.String()
	}
	for k := 0; k < BigFiles; k++ {
		var b strings.Builder
		for i := 0; b.Len() < 60000; i++ {
			fmt.Fprintf(&b, "2026-01-%02d 12:%02d:%02d INFO worker-%d processed batch %d in %dms status=ok\n", k+1, i%60, i%60, i%7, i, 10+i%90)
		}
		files[fmt.Sprintf("data/big_%d.log", k)] = b.String()
	}
	return files
}

// Scenario describes one long run.
type Scenario struct {
	// Steps is the number of model steps of the agent; the last one answers (default 150).
	Steps int
	// Window is the model's context window in tokens (default 60000).
	Window int
	// Threshold is COMPACTION_THRESHOLD (default 0.7).
	Threshold float64
	// MaxSteps is AGENT_MAX_STEPS (default Steps+20).
	MaxSteps int
	// StepRetries is LLM_STEP_RETRIES (default 3).
	StepRetries int
	// Steers maps an agent step to a steer sent during that step's model call.
	Steers map[int]string
	// Action returns the tool call (name, JSON arguments) of an agent step before
	// the last; "" ends the step with a plain answer. Default: DefaultAction.
	Action func(step int) (name, args string)
	// Wrap lets a test change the turn of an agent step (failure injection).
	Wrap func(step int, t llmtest.Turn) llmtest.Turn
	// Tools are extra tool providers.
	Tools []tools.Provider
	// Sample records the heap at every model call (a brief stop-the-world each);
	// leave it off to measure the harness's own speed.
	Sample bool
	// Timeout bounds the run (default 3 minutes).
	Timeout time.Duration
	// ModelDelay is slept in every model call (simulated latency).
	ModelDelay time.Duration
	// NoMask turns tool-output masking off (COMPACTION_KEEP_RECENT_TOOL_RESULTS=0), so
	// the context grows until compaction has to cut it.
	NoMask bool
	// NoFinish makes the model never answer: it keeps calling tools (step budget tests).
	NoFinish bool
}

// Outcome is what a run left behind.
// CompactionInfo is a completed work.compaction event.
type CompactionInfo struct{ Before, After int }

type Outcome struct {
	// Status is "finished" or the RUN_ERROR code / outcome type.
	Status string
	Dir    string
	// Calls are the agent's own model requests, in order (summarizer and title calls excluded).
	Calls []llmtest.Call
	// Replies[i] is the turn answered to Calls[i].
	Replies []llmtest.Turn
	// Compactions are the indexes into Calls of the first request after a compaction that changed the
	// history (summarized, truncated or dropped), from the work.compaction events. It assumes one model
	// call per step, i.e. a run without step retries.
	Compactions []int
	// SummarizerCalls is the number of summarizer requests.
	SummarizerCalls int
	// CompactionInfo[i] describes Compactions[i] (token estimates from the event).
	CompactionInfo []CompactionInfo
	Events         []store.Event
	Steps          int
	Wall           time.Duration
	ModelTime      time.Duration
	// Heap samples (bytes in use) at each model call when Sample is set.
	Heap                              []uint64
	HeapBefore, HeapAfter             uint64
	GoroutinesBefore, GoroutinesAfter int
	RunID                             string
}

// DefaultAction is the 4-step cycle of the simulated agent: read a module,
// search for its marker, edit it, then run a command (every third group one
// that prints 45 KB, which spills and is later masked).
func DefaultAction(step int) (string, string) {
	g, phase := (step-2)/4, (step-2)%4
	k := g % Modules
	switch phase {
	case 0:
		return "read_file", fmt.Sprintf(`{"path":"/workspace/pkg/m%02d/f%02d.go"}`, k, k)
	case 1:
		if g%3 == 1 {
			return "search_files", `{"pattern":"Compute[0-9]+_[0-9]+","path":"/workspace/pkg","glob":"*.go"}`
		}
		return "search_files", fmt.Sprintf(`{"pattern":"TODO\\(m%02d\\)","path":"/workspace/pkg"}`, k)
	case 2:
		return "edit_file", fmt.Sprintf(`{"path":"/workspace/pkg/m%02d/f%02d.go","oldText":"// TODO(m%02d): tune","newText":"// DONE(m%02d): tuned in group %d"}`, k, k, k, k, g)
	default:
		if g%3 == 0 {
			return "execute_command", fmt.Sprintf(`{"command":"head -c 45000 data/big_%d.log; echo group %d"}`, g%BigFiles, g)
		}
		return "execute_command", fmt.Sprintf(`{"command":"wc -l pkg/m%02d/*.go; echo group %d"}`, k, g)
	}
}

func (s *Scenario) defaults() {
	if s.Steps == 0 {
		s.Steps = 150
	}
	if s.Window == 0 {
		s.Window = 60000
	}
	if s.Threshold == 0 {
		s.Threshold = 0.7
	}
	if s.MaxSteps == 0 {
		s.MaxSteps = s.Steps + 20
	}
	if s.StepRetries == 0 {
		s.StepRetries = 3
	}
	if s.Action == nil {
		s.Action = DefaultAction
	}
	if s.Timeout == 0 {
		s.Timeout = 3 * time.Minute
	}
}

// Run drives the scenario through harness.New with the local backend and returns the outcome.
func Run(tb testing.TB, sc Scenario) *Outcome {
	tb.Helper()
	sc.defaults()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	tb.Cleanup(func() { slog.SetDefault(prev) })

	ws, state := tb.TempDir(), tb.TempDir()
	for name, content := range Repo() {
		p := filepath.Join(ws, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			tb.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			tb.Fatal(err)
		}
	}
	for k, v := range map[string]string{
		"LLM_PROVIDER": "openai", "LLM_MODELS": "sim", "LLM_DEFAULT_MODEL": "sim",
		"SANDBOX_BACKEND": "local", "LOCAL_WORKSPACE": ws, "LOCAL_STATE_DIR": state,
		"AGENT_PROFILE": "coding", "WORK_WORKSPACE_CHECKPOINTS": "false",
		"LLM_CONTEXT_WINDOWS": "sim=" + strconv.Itoa(sc.Window), "COMPACTION_THRESHOLD": strconv.FormatFloat(sc.Threshold, 'f', -1, 64),
		"AGENT_MAX_STEPS": strconv.Itoa(sc.MaxSteps), "LLM_STEP_RETRIES": strconv.Itoa(sc.StepRetries),
	} {
		tb.Setenv(k, v)
	}
	if sc.NoMask {
		tb.Setenv("COMPACTION_KEEP_RECENT_TOOL_RESULTS", "0")
	}
	cfg, err := config.Load()
	if err != nil {
		tb.Fatal(err)
	}

	out := &Outcome{Dir: ws}
	var (
		mu       sync.Mutex
		agentN   int
		runIDc   = make(chan string, 1)
		runID    string
		hv       *harness.Harness
		modelDur time.Duration
	)
	ctx, cancel := context.WithTimeout(context.Background(), sc.Timeout)
	defer cancel()

	fn := func(call llmtest.Call) llmtest.Turn {
		begin := time.Now()
		defer func() {
			mu.Lock()
			modelDur += time.Since(begin)
			mu.Unlock()
		}()
		if len(call.Tools) == 0 { // the summarizer and the title run without tools
			if len(call.Messages) > 0 && call.Messages[0].Content == compaction.Instructions {
				mu.Lock()
				out.SummarizerCalls++
				mu.Unlock()
				return llmtest.Turn{Chunks: llmtest.Text("## Goal\n" + Request + "\n## Progress\nEdited some modules; see the plan.")}
			}
			return llmtest.Turn{Chunks: llmtest.Text("Long session")}
		}
		mu.Lock()
		agentN++
		n := agentN
		out.Calls = append(out.Calls, call)
		if sc.Sample {
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			out.Heap = append(out.Heap, ms.HeapAlloc)
		}
		mu.Unlock()
		if sc.ModelDelay > 0 {
			time.Sleep(sc.ModelDelay)
		}
		if text, ok := sc.Steers[n]; ok {
			if runID == "" {
				runID = <-runIDc
			}
			if err := hv.Steer(ctx, "sim", runID, text); err != nil {
				tb.Errorf("steer at step %d: %v", n, err)
			}
		}
		usage := compaction.EstimateMessages(call.Messages) + compaction.EstimateTools(call.Tools)
		var t llmtest.Turn
		switch {
		case n == 1:
			t = llmtest.Turn{Chunks: []*schema.Message{llmtest.WithUsage(llmtest.Finish(llmtest.ToolCall(0, "c1", "update_plan",
				`{"items":[{"id":"1","title":"`+PlanTitle+`","status":"in_progress"},{"id":"2","title":"Verify","status":"pending"}]}`), "tool_calls"), usage, 20)}}
		case n == sc.Steps-1 && !sc.NoFinish: // close the plan, or the harness asks the model to continue
			t = llmtest.Turn{Chunks: []*schema.Message{llmtest.WithUsage(llmtest.Finish(llmtest.ToolCall(0, "plan-done", "update_plan",
				`{"items":[{"id":"1","title":"`+PlanTitle+`","status":"completed"},{"id":"2","title":"Verify","status":"completed"}]}`), "tool_calls"), usage, 20)}}
		case n >= sc.Steps && !sc.NoFinish:
			t = llmtest.Turn{Chunks: []*schema.Message{llmtest.WithUsage(llmtest.Finish(&schema.Message{Role: schema.Assistant, Content: "All modules tuned."}, "stop"), usage, 20)}}
		default:
			name, args := sc.Action(n)
			if name == "" {
				t = llmtest.Turn{Chunks: []*schema.Message{llmtest.WithUsage(llmtest.Finish(&schema.Message{Role: schema.Assistant, Content: "Nothing more to do."}, "stop"), usage, 20)}}
			} else {
				t = llmtest.Turn{Chunks: []*schema.Message{llmtest.WithUsage(llmtest.Finish(llmtest.ToolCall(0, "c"+strconv.Itoa(n), name, args), "tool_calls"), usage, 30)}}
			}
		}
		if sc.Wrap != nil {
			t = sc.Wrap(n, t)
		}
		mu.Lock()
		out.Replies = append(out.Replies, t)
		mu.Unlock()
		return t
	}

	h, err := harness.New(harness.Options{Config: cfg, Models: llmtest.Factory{M: llmtest.NewFunc(fn)}, Tools: sc.Tools})
	if err != nil {
		tb.Fatal(err)
	}
	hv = h

	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	out.HeapBefore, out.GoroutinesBefore = ms.HeapAlloc, runtime.NumGoroutine()

	done := make(chan struct{})
	go func() { defer close(done); _ = h.Run(ctx) }()
	th, err := h.NewThread(ctx, "sim", "long")
	if err != nil {
		tb.Fatal(err)
	}
	start := time.Now()
	run, err := h.Send(ctx, "sim", th.ID, Request, harness.RunOptions{Model: "sim"})
	if err != nil {
		tb.Fatal(err)
	}
	out.RunID = run.ID
	runIDc <- run.ID
	out.Status = "no terminal event"
	for ev := range h.Events(ctx, run.ID, 0) {
		out.Events = append(out.Events, ev)
		switch ev.Type {
		case agui.Custom:
			var c struct {
				Name  string `json:"name"`
				Value struct {
					Phase  string  `json:"phase"`
					Before float64 `json:"tokensBefore"`
					After  float64 `json:"tokensAfter"`
				} `json:"value"`
			}
			if jsonUnmarshal(ev.Data, &c) == nil && c.Name == agui.CustomCompaction && c.Value.Phase == "completed" {
				out.Compactions = append(out.Compactions, out.Steps-1)
				out.CompactionInfo = append(out.CompactionInfo, CompactionInfo{int(c.Value.Before), int(c.Value.After)})
			}
		case agui.StepStarted:
			out.Steps++
		case agui.RunFinished:
			out.Status = "finished"
		case agui.RunError:
			out.Status = errorCode(ev)
		}
		if ev.Type == agui.RunFinished && out.Status == "finished" {
			if c := outcomeType(ev); c != "" && c != "success" {
				out.Status = c
			}
		}
	}
	out.Wall = time.Since(start)
	mu.Lock()
	out.ModelTime = modelDur
	mu.Unlock()
	cancel()
	<-done

	runtime.GC()
	runtime.ReadMemStats(&ms)
	out.HeapAfter = ms.HeapAlloc
	out.GoroutinesAfter = SettledGoroutines(out.GoroutinesBefore, 5*time.Second)
	return out
}

// SettledGoroutines waits (retrying) until the goroutine count is at most want
// and returns the last count: goroutines that are winding down are not leaks.
func SettledGoroutines(want int, wait time.Duration) int {
	deadline := time.Now().Add(wait)
	for {
		n := runtime.NumGoroutine()
		if n <= want || time.Now().After(deadline) {
			return n
		}
		time.Sleep(50 * time.Millisecond)
		runtime.GC()
	}
}

func errorCode(ev store.Event) string {
	var d struct {
		Code string `json:"code"`
	}
	_ = jsonUnmarshal(ev.Data, &d)
	return d.Code
}

func outcomeType(ev store.Event) string {
	var d struct {
		Outcome struct {
			Type string `json:"type"`
		} `json:"outcome"`
	}
	_ = jsonUnmarshal(ev.Data, &d)
	return d.Outcome.Type
}

// CallsText returns the concatenated text of a request's messages.
func CallsText(c llmtest.Call) string {
	var b strings.Builder
	for _, m := range c.Messages {
		b.WriteString(m.Content)
		b.WriteByte('\n')
	}
	return b.String()
}

// Tokens estimates the tokens of a request (messages and tool definitions).
func Tokens(c llmtest.Call) int {
	return compaction.EstimateMessages(c.Messages) + compaction.EstimateTools(c.Tools)
}

// ExtraTool wraps a func as a tools.Provider with one tool.
func ExtraTool(name string, run func(ctx context.Context, args string) (string, error)) tools.Provider {
	return funcProvider{name: name, run: run}
}

type funcProvider struct {
	name string
	run  func(ctx context.Context, args string) (string, error)
}

func (p funcProvider) Name() string { return p.name }
func (p funcProvider) Tools(context.Context, tools.RunContext) ([]tool.BaseTool, func(), error) {
	return []tool.BaseTool{funcTool(p)}, func() {}, nil
}

type funcTool funcProvider

func (f funcTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: f.name, Desc: "scripted " + f.name}, nil
}
func (f funcTool) InvokableRun(ctx context.Context, args string, _ ...tool.Option) (string, error) {
	return f.run(ctx, args)
}
