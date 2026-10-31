package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/GhaziBenDahmane/harness"
	"github.com/GhaziBenDahmane/harness/ai/llm"
	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/core/agui"
)

// Missing returns the programs of t that are not on PATH.
func Missing(t Task) []string {
	var out []string
	for _, p := range t.Needs {
		if _, err := exec.LookPath(p); err != nil {
			out = append(out, p)
		}
	}
	return out
}

// Materialize writes files under dir.
func Materialize(dir string, files ...map[string]string) error {
	for _, set := range files {
		for name, content := range set {
			p := filepath.Join(dir, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

// Check applies t's hidden files in dir and reports whether the task's checks hold for answer.
func Check(ctx context.Context, t Task, dir, answer string) (ok bool, output string) {
	if err := Materialize(dir, t.Hidden); err != nil {
		return false, err.Error()
	}
	for _, expr := range t.Answer {
		if !regexp.MustCompile(expr).MatchString(answer) {
			return false, fmt.Sprintf("the answer does not match %s", expr)
		}
	}
	if t.Verify == "" {
		return true, ""
	}
	vctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(vctx, "bash", "-c", t.Verify)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return err == nil, tail(out.String(), 800)
}

// Run executes t through the harness with the local sandbox and the coding profile and returns the
// result. base supplies the model configuration (config.Load); models, when non-nil, replaces the
// real model client (offline tests). The run's working directory is a fresh temporary one.
func Run(ctx context.Context, base *config.Config, models llm.Factory, t Task, o Options) Result {
	res := Result{Task: t.Name, Model: base.LLMDefaultModel, Status: "not started", Tools: map[string]int{}}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Minute
	}
	dir, err := os.MkdirTemp("", "bench-"+t.Name+"-")
	if err != nil {
		res.Status = "setup: " + err.Error()
		return res
	}
	if o.Keep {
		res.Dir = dir
	} else {
		defer os.RemoveAll(dir)
	}
	state, _ := os.MkdirTemp("", "bench-state-")
	defer os.RemoveAll(state)
	if err := Materialize(dir, t.Files); err != nil {
		res.Status = "setup: " + err.Error()
		return res
	}

	cfg := *base
	cfg.SandboxBackend, cfg.LocalWorkspace, cfg.LocalStateDir = "local", dir, state
	cfg.AgentProfile = "coding"
	cfg.AgentPermissions = "yolo" // unattended: nobody can answer an approval prompt (the hard denies still apply)
	cfg.WorkspaceCheckpoints = false
	if o.MaxSteps > 0 {
		cfg.AgentMaxSteps = o.MaxSteps
	}
	if o.NoCacheKey {
		cfg.LLMPromptCacheKey = false
	}
	defer quietLogs()()

	h, err := harness.New(harness.Options{Config: &cfg, Models: models, Version: "bench"})
	if err != nil {
		res.Status = "setup: " + err.Error()
		return res
	}
	rctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	go func() { _ = h.Run(rctx) }()

	start := time.Now()
	th, err := h.NewThread(rctx, "bench", t.Name)
	if err != nil {
		res.Status = "setup: " + err.Error()
		return res
	}
	run, err := h.Send(rctx, "bench", th.ID, t.Prompt, harness.RunOptions{Model: cfg.LLMDefaultModel})
	if err != nil {
		res.Status = "send: " + err.Error()
		return res
	}
	var answer strings.Builder
	res.Status = "no terminal event"
	for ev := range h.Events(rctx, run.ID, 0) {
		observe(&res, &answer, ev.Type, ev.Data)
	}
	if rctx.Err() != nil && res.Status == "no terminal event" {
		res.Status = "timeout"
	}
	res.Detail = takeDetail(run.ID)
	res.WallSeconds = time.Since(start).Seconds()
	res.FinalAnswer = tail(strings.TrimSpace(answer.String()), 600)

	if res.Status == "finished" {
		res.Passed, res.VerifyOutput = Check(ctx, t, dir, answer.String())
	}
	if res.InputTokens > 0 {
		res.CacheHitPct = 100 * float64(res.CachedTokens) / float64(res.InputTokens)
	}
	res.UncachedTokens, res.UncachedPerStep = uncached(res.InputTokens, res.CachedTokens, res.Steps)
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	res.HeapMiB, res.SysMiB, res.MaxRSSMiB = mib(ms.HeapAlloc), mib(ms.Sys), maxRSSMiB()
	return res
}

// observe folds one AG-UI event into the result and the final answer text.
func observe(res *Result, answer *strings.Builder, typ string, data json.RawMessage) {
	var e map[string]any
	if json.Unmarshal(data, &e) != nil {
		return
	}
	switch typ {
	case agui.StepStarted:
		res.Steps++
		answer.Reset() // only the last step's text is the answer
	case agui.TextMessageContent:
		if d, ok := e["delta"].(string); ok {
			answer.WriteString(d)
		}
	case agui.ToolCallStart:
		res.ToolCalls++
		if n, ok := e["toolCallName"].(string); ok {
			res.Tools[n]++
		}
	case agui.ToolCallResult:
		if md, ok := e["metadata"].(map[string]any); ok && md["isError"] == true {
			res.ToolErrors++
		}
	case agui.RunFinished:
		res.Status = "finished"
		if list, ok := e["usage"].([]any); ok {
			for _, u := range list {
				m, _ := u.(map[string]any)
				res.InputTokens += num(m["inputTokens"])
				res.OutputTokens += num(m["outputTokens"])
				res.CachedTokens += num(m["cachedInputTokens"])
			}
		}
		if oc, ok := e["outcome"].(map[string]any); ok && oc["type"] != "success" {
			res.Status = fmt.Sprint(oc["type"])
		}
	case agui.RunError:
		res.Status = fmt.Sprint(e["code"])
	}
}

func num(v any) int { f, _ := v.(float64); return int(f) }

func mib(b uint64) float64 { return float64(b) / (1 << 20) }

func maxRSSMiB() float64 {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) != nil {
		return 0
	}
	if runtime.GOOS == "darwin" {
		return float64(ru.Maxrss) / (1 << 20) // bytes
	}
	return float64(ru.Maxrss) / 1024 // KiB
}

var (
	logMu    sync.Mutex
	logUsers int
	logPrev  *slog.Logger
)

// quietLogs silences the default logger until the returned func is called; it is reference counted so
// runs in parallel (Suite) restore the original logger only when the last one finishes.
func quietLogs() func() {
	logMu.Lock()
	defer logMu.Unlock()
	if logUsers == 0 {
		logPrev = slog.Default()
		slog.SetDefault(slog.New(&detailHandler{}))
	}
	logUsers++
	return func() {
		logMu.Lock()
		defer logMu.Unlock()
		if logUsers--; logUsers == 0 {
			slog.SetDefault(logPrev)
		}
	}
}

// detailHandler drops every log record but remembers the detail of the warnings and errors of each run
// (the RUN_ERROR event carries only a generic message), so a failed Result can say why.
type detailHandler struct{}

var (
	detailMu sync.Mutex
	details  = map[string]string{}
)

func (*detailHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *detailHandler) WithAttrs([]slog.Attr) slog.Handler     { return h }
func (h *detailHandler) WithGroup(string) slog.Handler          { return h }

func (*detailHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level < slog.LevelWarn {
		return nil
	}
	var runID, detail string
	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "run_id":
			runID = a.Value.String()
		case "detail", "error":
			detail = a.Value.String()
		}
		return true
	})
	if runID != "" && detail != "" {
		detailMu.Lock()
		details[runID] = r.Message + ": " + detail
		detailMu.Unlock()
	}
	return nil
}

// takeDetail returns and forgets the last warning detail of a run.
func takeDetail(runID string) string {
	detailMu.Lock()
	defer detailMu.Unlock()
	d := details[runID]
	delete(details, runID)
	return tail(d, 400)
}

// uncached returns the input tokens not served from the prompt cache, in total and per step.
func uncached(input, cached, steps int) (int, float64) {
	u := max(input-cached, 0)
	if steps <= 0 {
		return u, 0
	}
	return u, float64(u) / float64(steps)
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
