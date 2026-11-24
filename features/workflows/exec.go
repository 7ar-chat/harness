package workflows

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/7ar-chat/harness/core/sandbox"
	core "github.com/7ar-chat/harness/core/workflows"
)

// Direct execution: a script, build or preview step runs its command in the run's sandbox, with no model and no
// spend. The exit code is the result, the output streams into the step's log. Without a sandbox (Options.Sandbox
// nil, or Options.SandboxEnabled false) such a step still runs as a factory task whose agent runs the command.

// Verdict is the policy's answer for a command a step is about to run directly.
type Verdict struct {
	// Deny, when set, refuses the command (the text says why); the step fails.
	Deny string
	// Ask makes a person approve first (the step waits like a gate; the inbox shows it).
	Ask bool
	// Reason says what is asked.
	Reason string
}

// DefaultExecTimeout bounds a command step.
const DefaultExecTimeout = 15 * time.Minute

const (
	execLogMax   = 64 << 10
	execFlushGap = 2 * time.Second
	execMarker   = "exec"
)

// ErrNoPreview is returned by Options.Preview when the sandbox or project cannot show a preview: the step then just
// runs its command.
var ErrNoPreview = errors.New("no preview available")

type execKey struct {
	run, node string
	round     int
}

type execs struct {
	mu     sync.Mutex
	active map[execKey]context.CancelFunc
}

func (x *execs) begin(k execKey, cancel context.CancelFunc) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.active == nil {
		x.active = map[execKey]context.CancelFunc{}
	}
	if _, ok := x.active[k]; ok {
		return false
	}
	x.active[k] = cancel
	return true
}

func (x *execs) end(k execKey) {
	x.mu.Lock()
	delete(x.active, k)
	x.mu.Unlock()
}

func (x *execs) has(k execKey) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	_, ok := x.active[k]
	return ok
}

func (x *execs) cancel(k execKey) {
	x.mu.Lock()
	c := x.active[k]
	x.mu.Unlock()
	if c != nil {
		c()
	}
}

func (e *Engine) directExec() bool {
	return e.o.Sandbox != nil && (e.o.SandboxEnabled == nil || e.o.SandboxEnabled())
}

// startDirect begins a command step in the run's sandbox. It reports false when the step must run as a task.
func (e *Engine) startDirect(p *pass, n core.Node, s *core.NodeRun) bool {
	if !e.directExec() {
		return false
	}
	cmd := strings.TrimSpace(core.Render(n.Command, e.lookup(p.r, n)))
	if e.o.Judge != nil && !s.Approved && cmd != "" {
		v := e.o.Judge(p.ctx, *p.r, n, cmd)
		switch {
		case v.Deny != "":
			e.fail(p, s, "not run: "+v.Deny)
			return true
		case v.Ask:
			s.State = core.NodeWaiting
			s.Note = firstNonEmpty(v.Reason, "a person must approve running this command")
			p.log("gate", n.ID, "", "waiting for approval to run: "+firstLine(cmd))
			p.note("gate", n.ID)
			return true
		}
	}
	s.State, s.TaskState, s.Direct = core.NodeRunning, execMarker, true
	p.log("step", n.ID, "", "started in the run's sandbox")
	p.note("step", n.ID)
	e.launchExec(p.r.ID, n.ID, s.Round)
	return true
}

// resumeDirect restarts a direct step this process does not run (after a restart).
func (e *Engine) resumeDirect(p *pass, s *core.NodeRun) {
	k := execKey{p.r.ID, s.NodeID, s.Round}
	if !e.execs.has(k) {
		e.launchExec(k.run, k.node, k.round)
	}
}

func (e *Engine) launchExec(runID, nodeID string, round int) {
	k := execKey{runID, nodeID, round}
	timeout := e.o.ExecTimeout
	if timeout <= 0 {
		timeout = DefaultExecTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout+2*time.Minute)
	if !e.execs.begin(k, cancel) {
		cancel()
		return
	}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer cancel()
		defer e.execs.end(k)
		e.runExec(ctx, k, timeout)
	}()
}

type outBuf struct {
	mu    sync.Mutex
	b     []byte
	fresh bool
}

func (o *outBuf) add(t string) {
	o.mu.Lock()
	o.b = append(o.b, t...)
	if len(o.b) > execLogMax*2 {
		o.b = append([]byte(nil), o.b[len(o.b)-execLogMax:]...)
	}
	o.fresh = true
	o.mu.Unlock()
}

func (o *outBuf) text() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	s := string(o.b)
	if len(s) > execLogMax {
		s = "..." + s[len(s)-execLogMax:]
	}
	return s
}

func (o *outBuf) dirty() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	d := o.fresh
	o.fresh = false
	return d
}

// setStep applies fn to the step if it is still the running round k.
func (e *Engine) setStep(k execKey, fn func(p *pass, s *core.NodeRun)) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := e.mutate(ctx, k.run, func(p *pass) error {
		s := p.r.Step(k.node)
		if s == nil || s.State != core.NodeRunning || s.Round != k.round || !s.Direct {
			return nil
		}
		fn(p, s)
		return nil
	})
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		slog.Warn("workflows: saving a command step failed", "run", k.run, "node", k.node, "error", err)
	}
}

func (e *Engine) runExec(ctx context.Context, k execKey, timeout time.Duration) {
	r, err := e.o.Store.GetRun(ctx, k.run)
	if err != nil {
		return
	}
	n, ok := r.Node(k.node)
	st := r.Step(k.node)
	if !ok || st == nil || st.State != core.NodeRunning || st.Round != k.round {
		return
	}
	cmd := core.Render(n.Command, e.lookup(r, n))
	out := &outBuf{}
	fail := func(msg string) {
		txt := out.text()
		e.setStep(k, func(p *pass, s *core.NodeRun) {
			s.Log = txt
			e.failWith(p, s, msg, tail(txt, 3000))
		})
	}
	repo := core.RepoOf(*r, n)
	sb, err := e.o.Sandbox(ctx, *r, repo, core.BranchFor(*r, repo))
	if err != nil {
		if ctx.Err() == nil {
			fail("could not get the run's sandbox: " + err.Error())
		}
		return
	}
	stopFlush := make(chan struct{})
	flushed := make(chan struct{})
	go func() {
		defer close(flushed)
		t := time.NewTicker(e.flushEvery())
		defer t.Stop()
		for {
			select {
			case <-stopFlush:
				return
			case <-t.C:
				if out.dirty() {
					txt := out.text()
					e.setStep(k, func(p *pass, s *core.NodeRun) {
						s.Log = txt
						p.note("step", s.NodeID)
					})
				}
			}
		}
	}()
	var (
		code int
		url  string
	)
	if n.Kind == core.KindPreview && e.o.Preview != nil {
		var plog string
		url, plog, err = e.o.Preview(ctx, *r, n, sb)
		out.add(plog)
		switch {
		case errors.Is(err, ErrNoPreview):
			url = ""
			code, err = e.execCommand(ctx, sb, cmd, repo, timeout, out)
		case err != nil:
			code = 1
		}
	} else {
		code, err = e.execCommand(ctx, sb, cmd, repo, timeout, out)
	}
	close(stopFlush)
	<-flushed
	txt := out.text()
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		fail(fmt.Sprintf("the command timed out after %s:\n%s", timeout.Round(time.Second), tail(txt, 3000)))
	case ctx.Err() != nil:
		// the step was cancelled: its state is already settled
	case err != nil && n.Kind == core.KindPreview && url == "" && code == 1 && e.o.Preview != nil:
		fail("the preview did not start: " + err.Error())
	case err != nil:
		fail("the command could not run: " + err.Error())
	case code != 0:
		e.setStep(k, func(p *pass, s *core.NodeRun) {
			s.Log, s.ExitCode = txt, &code
			e.failWith(p, s, fmt.Sprintf("the command exited with status %d:\n%s", code, tail(txt, 3000)), tail(txt, 3000))
		})
	default:
		e.setStep(k, func(p *pass, s *core.NodeRun) {
			zero := 0
			s.Log, s.ExitCode, s.URL = txt, &zero, url
			res := tail(txt, 3000)
			if url != "" {
				res = "Preview: " + url + "\n" + res
			}
			e.complete(p, s, res, nil, "")
		})
	}
}

func (e *Engine) flushEvery() time.Duration {
	if e.o.FlushEvery > 0 {
		return e.o.FlushEvery
	}
	return execFlushGap
}

func (e *Engine) execCommand(ctx context.Context, sb sandbox.Sandbox, cmd, repo string, timeout time.Duration, out *outBuf) (int, error) {
	cwd := sandbox.Workspace
	if repo != "" {
		cwd = e.repoDir()
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := sb.Exec(cctx, sandbox.ExecRequest{Command: cmd, Cwd: cwd, Timeout: timeout, OnStdout: out.add, OnStderr: out.add})
	if cctx.Err() != nil && ctx.Err() == nil && (err != nil || res.ExitCode != 0) {
		return 0, context.DeadlineExceeded
	}
	return res.ExitCode, err
}

func (e *Engine) repoDir() string {
	if e.o.RepoDir != "" {
		return e.o.RepoDir
	}
	return sandbox.Workspace + "/repo"
}
