// Package executors lets an agent hand a task to an external coding-agent CLI (Claude Code, Codex, OpenCode, or a
// command a project defined) that runs non-interactively inside the run's sandbox. The run_executor tool streams
// the CLI's output as live tool output, enforces a timeout and cancellation, reads the CLI's final summary and the
// files it changed, books the cost it reports against the task's budget, and keeps every credential out of the
// output. The task's policy decides how much the CLI may do, and the factory's review gate still inspects the diff
// whoever wrote it.
package executors

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/core/executors"
	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/engine/tools"
)

// Limits.
const (
	DefaultTimeout = 30 * time.Minute
	MaxTimeout     = 2 * time.Hour
	// maxLive bounds the live output of one call (the tool result carries the summary and the tail).
	maxLive    = 256 << 10
	maxTail    = 6000
	maxSummary = 6000
	maxDiff    = 3000
	// probeTimeout bounds the small commands around a run (the installed check, git).
	probeTimeout = 60 * time.Second
	installTime  = 10 * time.Minute
	repoDir      = sandbox.Workspace + "/repo"
)

// TaskStore is the part of factory.Store the executors use: the task of a thread, its budget and its audit trail.
type TaskStore interface {
	GetThreadInfo(ctx context.Context, threadID string) (*factory.ThreadInfo, error)
	GetTask(ctx context.Context, id string) (*factory.Task, error)
	AddSpend(ctx context.Context, id string, d factory.Spend) (*factory.Task, error)
	AppendEvent(ctx context.Context, e factory.TaskEvent) error
}

// Options configures a Provider.
type Options struct {
	// Sandboxes hands out the thread's sandbox. Required.
	Sandboxes sandbox.Manager
	// Resolver finds the executors in reach (nil: the builtin ones).
	Resolver executors.Resolver
	// Credentials gives the sealed per-user credentials of an executor (nil: none; the CLI uses what its sandbox has).
	Credentials executors.CredentialSource
	// Store links a thread to its task, policy and budget and records the audit events (nil: none).
	Store TaskStore
	// AllowInstall lets a run install a missing CLI with the executor's install command. Off by default: it
	// downloads and runs code from a package registry.
	AllowInstall bool
	// Timeout is the default timeout of a run (0: 30 minutes); MaxTimeout caps what a call may ask (0: 2 hours).
	Timeout    time.Duration
	MaxTimeout time.Duration
	Now        func() time.Time
}

// Provider offers the run_executor tool and runs executors for other features (it is an executors.Runner).
type Provider struct {
	o Options
}

// New returns a Provider.
func New(o Options) *Provider {
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.MaxTimeout <= 0 {
		o.MaxTimeout = MaxTimeout
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Provider{o}
}

var (
	_ tools.Provider   = (*Provider)(nil)
	_ executors.Runner = (*Provider)(nil)
)

// Name implements tools.Provider.
func (p *Provider) Name() string { return "executors" }

type input struct {
	Executor       string `json:"executor" jsonschema_description:"The executor to run (see the tool description)"`
	Task           string `json:"task" jsonschema_description:"A self-contained task: the CLI does not see this conversation, so name the goal, the files, the constraints and what to verify"`
	Workdir        string `json:"workdir,omitempty" jsonschema_description:"Directory under /workspace to run in; default is the repository (/workspace/repo) when there is one, else /workspace"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty" jsonschema_description:"Seconds before the CLI is killed (default 1800, at most 7200)"`
	Install        bool   `json:"install,omitempty" jsonschema_description:"Install the CLI first when it is missing from the sandbox (only if the server allows installs)"`
}

// Tools implements tools.Provider.
func (p *Provider) Tools(ctx context.Context, rc tools.RunContext) ([]ai.BaseTool, func(), error) {
	if p.o.Sandboxes == nil || !p.o.Sandboxes.Enabled() || rc.Sandbox == "off" {
		return nil, nil, nil
	}
	list, err := p.available(ctx, executors.Query{Login: rc.Login, ThreadID: rc.ThreadID})
	if err != nil {
		slog.Warn("executors: listing the executors failed", "error", err)
		return nil, nil, nil
	}
	if len(list) == 0 {
		return nil, nil, nil
	}
	var roster strings.Builder
	for _, e := range list {
		fmt.Fprintf(&roster, "- %s", e.Name)
		if !e.SupportsReadOnly() {
			roster.WriteString(" (cannot run read-only)")
		}
		if e.Description != "" {
			roster.WriteString(": " + e.Description)
		}
		roster.WriteByte('\n')
	}
	desc := "Run an external coding-agent CLI non-interactively inside this sandbox on a task and wait for it. Its output streams as it works; " +
		"you get its final summary, the files it changed and the diff stat. It runs under this task's policy (read-only for a read-only role), " +
		"with the credentials its owner sealed for it, and its changes are reviewed like yours. It does not see this conversation.\n" +
		"Executors:\n" + strings.TrimRight(roster.String(), "\n") + "\n" +
		"If the CLI is not installed the result says so; install=true installs it when the server allows."
	tool, err := ai.InferTool("run_executor", desc, func(ctx context.Context, in input) (string, error) {
		return p.call(ctx, rc, in)
	})
	if err != nil {
		return nil, nil, err
	}
	return []ai.BaseTool{tool}, nil, nil
}

func (p *Provider) available(ctx context.Context, q executors.Query) ([]executors.Executor, error) {
	if p.o.Resolver == nil {
		return executors.Builtins(), nil
	}
	return p.o.Resolver.Available(ctx, q)
}

func (p *Provider) resolve(ctx context.Context, q executors.Query, name string) (*executors.Executor, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if p.o.Resolver == nil {
		if e, ok := executors.BuiltinNamed(name); ok {
			return &e, nil
		}
		return nil, executors.ErrNotFound
	}
	return p.o.Resolver.Resolve(ctx, q, name)
}

// call is the tool handler: it runs the executor with the live output streamed to the call and renders the result.
func (p *Provider) call(ctx context.Context, rc tools.RunContext, in input) (string, error) {
	callID, em := tools.CallFromContext(ctx)
	var sent int
	live := func(text string) {
		if em == nil || sent >= maxLive {
			return
		}
		sent += len(text)
		em.ToolOutput(callID, "stdout", text)
	}
	req := executors.Request{Login: rc.Login, ThreadID: rc.ThreadID, Executor: in.Executor, Task: in.Task, Workdir: in.Workdir,
		Timeout: time.Duration(in.TimeoutSeconds) * time.Second, Install: in.Install}
	res, err := p.Run(ctx, req, live)
	if err != nil {
		var ue userError
		if errors.As(err, &ue) {
			return "", tools.Errorf("%s", ue.msg)
		}
		return "", err
	}
	return executors.Render(res), nil
}

// userError is a failure the model can act on (a wrong name, a missing CLI), told to it as the tool's error.
type userError struct{ msg string }

func (e userError) Error() string { return e.msg }

func userErrf(format string, a ...any) error { return userError{fmt.Sprintf(format, a...)} }

// Run implements executors.Runner. It returns an error for what stops a run from starting (an unknown executor, a
// missing CLI, an exhausted budget) and a Result, failed or not, once the CLI ran.
func (p *Provider) Run(ctx context.Context, req executors.Request, onOutput func(string)) (*executors.Result, error) {
	if strings.TrimSpace(req.Task) == "" {
		return nil, userErrf("task must not be empty")
	}
	if len(req.Task) > executors.MaxTask {
		return nil, userErrf("the task is too long (at most %d characters)", executors.MaxTask)
	}
	var ti *factory.ThreadInfo
	if p.o.Store != nil && req.ThreadID != "" {
		got, err := p.o.Store.GetThreadInfo(ctx, req.ThreadID)
		switch {
		case err == nil:
			ti = got
		case !errors.Is(err, factory.ErrNotFound):
			return nil, fmt.Errorf("executors: thread info of %s: %w", req.ThreadID, err)
		}
	}
	if ti != nil {
		if req.Login == "" {
			req.Login = ti.Login
		}
		if req.TaskID == "" {
			req.TaskID = ti.TaskID
		}
		if req.Policy == "" {
			req.Policy = ti.Policy
		}
		req.ReadOnly = req.ReadOnly || ti.ReadOnly
	}
	ex, err := p.resolve(ctx, executors.Query{Login: req.Login, ThreadID: req.ThreadID}, req.Executor)
	switch {
	case errors.Is(err, executors.ErrNotFound):
		return nil, userErrf("unknown executor %q", req.Executor)
	case err != nil:
		return nil, fmt.Errorf("executors: resolving %q: %w", req.Executor, err)
	}
	level := executors.LevelFor(req.Policy, req.ReadOnly)
	command, err := ex.Render(level)
	if errors.Is(err, executors.ErrReadOnlyUnsupported) {
		return nil, userErrf("%s has no read-only mode, so it cannot serve a read-only run; use another executor or do it yourself", ex.Name)
	}
	if err != nil {
		return nil, err
	}
	if req.TaskID != "" && p.o.Store != nil {
		if t, err := p.o.Store.GetTask(ctx, req.TaskID); err == nil && t.Budget.Over(t.Spent) {
			p.event(ctx, req.TaskID, factory.EventBudget, "executor refused: budget exhausted", map[string]any{"reason": "budget exhausted", "executor": ex.Name})
			return nil, userErrf("the task's budget is exhausted: finish with what you have")
		}
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = p.o.Timeout
	}
	timeout = min(timeout, p.o.MaxTimeout)

	if p.o.Sandboxes == nil || !p.o.Sandboxes.Enabled() {
		return nil, userErrf("there is no sandbox to run an executor in")
	}
	sb, err := p.o.Sandboxes.Acquire(ctx, req.ThreadID, req.Login)
	if err != nil {
		return nil, fmt.Errorf("executors: acquiring the sandbox: %w", err)
	}
	workdir, err := p.workdir(ctx, sb, req.Workdir)
	if err != nil {
		return nil, err
	}
	env := map[string]string{"HARNESS_TASK": req.Task, "CI": "1", "NO_COLOR": "1", "TERM": "dumb"}
	var secrets []string
	if p.o.Credentials != nil && req.Login != "" {
		creds, err := p.o.Credentials.Credentials(ctx, req.Login, ex.Name)
		if err != nil {
			return nil, fmt.Errorf("executors: reading the credentials of %s: %w", ex.Name, err)
		}
		for _, name := range ex.CredentialEnv { // only what the executor declared
			if v := creds[name]; v != "" {
				env[name] = v
				secrets = append(secrets, v)
			}
		}
	}
	redact := func(s string) string { return executors.Redact(s, secrets...) }

	if err := p.ensureInstalled(ctx, sb, *ex, req.Install, redact, onOutput); err != nil {
		return nil, err
	}

	p.event(ctx, req.TaskID, factory.EventPolicy, fmt.Sprintf("executor %s runs at level %s", ex.Name, level),
		map[string]any{"tool": "run_executor", "executor": ex.Name, "level": string(level), "policy": req.Policy, "rule": "executor"})

	before := p.gitHead(ctx, sb, workdir)
	start := p.o.Now()
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	parser := executors.NewParser(ex.Parser)
	var tail tailBuf
	emit := func(shown string) {
		shown = redact(executors.StripANSI(shown))
		if strings.TrimSpace(shown) == "" {
			return
		}
		tail.add(shown)
		if onOutput != nil {
			onOutput(shown + "\n")
		}
	}
	out, errOut := newLines(func(l string) { emit(parser.Line(l)) }), newLines(func(l string) { emit("! " + l) })
	er, execErr := sb.Exec(rctx, sandbox.ExecRequest{Command: command, Cwd: workdir, Timeout: timeout + 10*time.Second, Env: env,
		OnStdout: out.write, OnStderr: errOut.write})
	out.flush()
	errOut.flush()
	res := &executors.Result{Executor: ex.Name, Level: level, ExitCode: er.ExitCode, Duration: p.o.Now().Sub(start)}
	if execErr != nil {
		switch {
		case ctx.Err() != nil: // the run was cancelled: the sandbox killed the process with the call
			p.finish(context.WithoutCancel(ctx), req, res, nil)
			return nil, ctx.Err()
		case errors.Is(rctx.Err(), context.DeadlineExceeded):
			res.TimedOut, res.ExitCode = true, -1
		default:
			return nil, fmt.Errorf("executors: running %s: %s", ex.Name, redact(execErr.Error()))
		}
	}
	sum := parser.Summary()
	res.Summary = clip(redact(strings.TrimSpace(sum.Text)), maxSummary)
	if sum.Failed && res.ExitCode == 0 {
		res.ExitCode = 1
	}
	if sum.Failed && sum.Err != "" && res.Summary == "" {
		res.Summary = clip(redact(sum.Err), maxSummary)
	}
	res.Tokens, res.USD = sum.Tokens, sum.USD
	res.Output = tail.String()
	// What changed is read even when the run failed or timed out: the work may be partly done.
	gctx, gcancel := context.WithTimeout(context.WithoutCancel(ctx), probeTimeout)
	defer gcancel()
	res.DiffStat, res.Changed = p.gitChanges(gctx, sb, workdir, before)
	res.DiffStat = clip(redact(res.DiffStat), maxDiff)
	p.finish(context.WithoutCancel(ctx), req, res, ex)
	return res, nil
}

// finish books what the run reported against the task and records the audit event.
func (p *Provider) finish(ctx context.Context, req executors.Request, res *executors.Result, ex *executors.Executor) {
	if req.TaskID == "" {
		return
	}
	data := map[string]any{"executor": res.Executor, "level": string(res.Level), "exitCode": res.ExitCode, "timedOut": res.TimedOut,
		"seconds": int(res.Duration.Seconds()), "changed": len(res.Changed)}
	text := fmt.Sprintf("executor %s finished with exit code %d", res.Executor, res.ExitCode)
	switch {
	case res.TimedOut:
		text = fmt.Sprintf("executor %s timed out", res.Executor)
	case ex == nil:
		text = fmt.Sprintf("executor %s was cancelled", res.Executor)
	}
	if res.Tokens > 0 || res.USD > 0 {
		data["tokens"], data["usd"] = res.Tokens, res.USD
	}
	p.event(ctx, req.TaskID, factory.EventExecutor, text, data)
	if p.o.Store == nil {
		return
	}
	spend := factory.Spend{USD: res.USD, Tokens: res.Tokens}
	if res.Tokens == 0 && res.USD == 0 {
		spend.Unpriced = 1 // the CLI did not say what it used: not free, unknown
	}
	if _, err := p.o.Store.AddSpend(ctx, req.TaskID, spend); err != nil && !errors.Is(err, factory.ErrNotFound) {
		slog.Warn("executors: booking the spend failed", "task_id", req.TaskID, "error", err)
	}
}

func (p *Provider) event(ctx context.Context, taskID, kind, text string, data map[string]any) {
	if taskID == "" || p.o.Store == nil {
		return
	}
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := p.o.Store.AppendEvent(actx, factory.TaskEvent{TaskID: taskID, Kind: kind, Text: text, Data: data, At: p.o.Now()}); err != nil {
		slog.Warn("executors: recording an event failed", "task_id", taskID, "kind", kind, "error", err)
	}
}

// workdir resolves where the CLI runs: under /workspace, the repository by default.
func (p *Provider) workdir(ctx context.Context, sb sandbox.Sandbox, asked string) (string, error) {
	asked = strings.TrimSpace(asked)
	if asked == "" {
		if fi, err := sb.Stat(ctx, repoDir); err == nil && fi.Type == "directory" {
			return repoDir, nil
		}
		return sandbox.Workspace, nil
	}
	if !path.IsAbs(asked) {
		asked = path.Join(sandbox.Workspace, asked)
	}
	asked = path.Clean(asked)
	if asked != sandbox.Workspace && !strings.HasPrefix(asked, sandbox.Workspace+"/") {
		return "", userErrf("workdir must be under %s", sandbox.Workspace)
	}
	if fi, err := sb.Stat(ctx, asked); err != nil || fi.Type != "directory" {
		return "", userErrf("workdir %s is not a directory in the sandbox", asked)
	}
	return asked, nil
}

// ensureInstalled checks that the CLI is in the sandbox and installs it when the run asked and the server allows.
func (p *Provider) ensureInstalled(ctx context.Context, sb sandbox.Sandbox, ex executors.Executor, install bool, redact func(string) string, onOutput func(string)) error {
	probe := func() (bool, error) {
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		r, err := sb.Exec(pctx, sandbox.ExecRequest{Command: "command -v " + shQuote(ex.Binary) + " >/dev/null 2>&1", Cwd: sandbox.Workspace})
		if err != nil {
			return false, fmt.Errorf("executors: checking for %s: %w", ex.Binary, err)
		}
		return r.ExitCode == 0, nil
	}
	ok, err := probe()
	if err != nil || ok {
		return err
	}
	switch {
	case ex.InstallCommand == "":
		return userErrf("%s is not installed in this sandbox and %s has no install command: ask an admin to add it to the sandbox image", ex.Binary, ex.Name)
	case !install:
		return userErrf("%s is not installed in this sandbox. Call run_executor again with install=true to run `%s` first (if the server allows installs)", ex.Binary, ex.InstallCommand)
	case !p.o.AllowInstall:
		return userErrf("%s is not installed in this sandbox and this server does not allow installing it: ask an admin to add it to the sandbox image or to enable executor installs", ex.Binary)
	}
	if onOutput != nil {
		onOutput("installing " + ex.Binary + ": " + ex.InstallCommand + "\n")
	}
	ictx, cancel := context.WithTimeout(ctx, installTime)
	defer cancel()
	var tail tailBuf
	l := newLines(func(s string) { tail.add(redact(s)) })
	r, err := sb.Exec(ictx, sandbox.ExecRequest{Command: ex.InstallCommand, Cwd: sandbox.Workspace, Timeout: installTime, OnStdout: l.write, OnStderr: l.write})
	l.flush()
	if err != nil {
		return userErrf("installing %s failed: %s", ex.Binary, redact(err.Error()))
	}
	if r.ExitCode != 0 {
		return userErrf("installing %s failed (exit %d):\n%s", ex.Binary, r.ExitCode, tail.String())
	}
	if ok, err := probe(); err != nil || !ok {
		if err != nil {
			return err
		}
		return userErrf("%s is still not found after its install command ran", ex.Binary)
	}
	return nil
}

// gitHead is the commit of workdir before the run ("" when it is not a repository).
func (p *Provider) gitHead(ctx context.Context, sb sandbox.Sandbox, workdir string) string {
	out, code := p.git(ctx, sb, workdir, "git rev-parse HEAD 2>/dev/null")
	if code != 0 {
		return ""
	}
	return strings.TrimSpace(out)
}

// gitChanges reads the diff stat and the changed files of workdir since the commit `before`: commits the CLI made,
// uncommitted edits and new files.
func (p *Provider) gitChanges(ctx context.Context, sb sandbox.Sandbox, workdir, before string) (stat string, files []string) {
	if before == "" {
		return "", nil
	}
	if !validSHA(before) {
		return "", nil
	}
	stat, _ = p.git(ctx, sb, workdir, "git diff --stat "+before+" 2>/dev/null")
	names, _ := p.git(ctx, sb, workdir, "{ git diff --name-only "+before+"; git ls-files --others --exclude-standard; } 2>/dev/null | sort -u")
	for _, l := range strings.Split(names, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			files = append(files, l)
		}
	}
	return strings.TrimSpace(stat), files
}

func (p *Provider) git(ctx context.Context, sb sandbox.Sandbox, workdir, cmd string) (string, int) {
	var b strings.Builder
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	r, err := sb.Exec(pctx, sandbox.ExecRequest{Command: cmd, Cwd: workdir, OnStdout: func(s string) { b.WriteString(s) }})
	if err != nil {
		return "", -1
	}
	return b.String(), r.ExitCode
}

func validSHA(s string) bool {
	if len(s) < 7 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// tailBuf keeps the end of what was shown.
type tailBuf struct {
	mu sync.Mutex
	s  []string
	n  int
}

func (t *tailBuf) add(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.s = append(t.s, line)
	t.n += len(line) + 1
	for t.n > 4*maxTail && len(t.s) > 1 {
		t.n -= len(t.s[0]) + 1
		t.s = t.s[1:]
	}
}

func (t *tailBuf) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := strings.Join(t.s, "\n")
	if r := []rune(s); len(r) > maxTail {
		return "…" + string(r[len(r)-maxTail:])
	}
	return s
}

// lines splits streamed chunks into lines.
type lines struct {
	mu  sync.Mutex
	buf strings.Builder
	fn  func(string)
}

func newLines(fn func(string)) *lines { return &lines{fn: fn} }

func (l *lines) write(text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf.WriteString(text)
	s := l.buf.String()
	for {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			break
		}
		l.fn(strings.TrimRight(s[:i], "\r"))
		s = s[i+1:]
	}
	l.buf.Reset()
	if len(s) > 1<<20 { // a line that never ends: do not hold it all
		l.fn(s)
		s = ""
	}
	l.buf.WriteString(s)
}

func (l *lines) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s := l.buf.String(); s != "" {
		l.buf.Reset()
		l.fn(strings.TrimRight(s, "\r"))
	}
}
