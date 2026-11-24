package executors

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/executors"
	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/engine/tools"
)

// fakeSandbox answers commands by their text; the CLI's own command is the one that contains the executor binary
// followed by a space and an option.
type fakeSandbox struct {
	mu       sync.Mutex
	cmds     []string
	envs     []map[string]string
	cwds     []string
	handlers []func(req sandbox.ExecRequest) (stdout, stderr string, code int, block bool, ok bool)
	repo     bool
}

func (f *fakeSandbox) ID() string { return "sb1" }
func (f *fakeSandbox) Exec(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	f.mu.Lock()
	f.cmds = append(f.cmds, req.Command)
	f.envs = append(f.envs, req.Env)
	f.cwds = append(f.cwds, req.Cwd)
	hs := f.handlers
	f.mu.Unlock()
	for _, h := range hs {
		out, errOut, code, block, ok := h(req)
		if !ok {
			continue
		}
		if out != "" && req.OnStdout != nil {
			req.OnStdout(out)
		}
		if errOut != "" && req.OnStderr != nil {
			req.OnStderr(errOut)
		}
		if block {
			<-ctx.Done()
			return sandbox.ExecResult{}, ctx.Err()
		}
		return sandbox.ExecResult{ExitCode: code}, nil
	}
	return sandbox.ExecResult{}, nil
}
func (f *fakeSandbox) ReadFile(context.Context, string, int64) ([]byte, int64, error) {
	return nil, 0, sandbox.ErrNotFound
}
func (f *fakeSandbox) WriteFile(context.Context, string, []byte) error { return nil }
func (f *fakeSandbox) MkdirAll(context.Context, string) error          { return nil }
func (f *fakeSandbox) List(context.Context, string, int) ([]sandbox.FileInfo, error) {
	return nil, nil
}
func (f *fakeSandbox) Stat(_ context.Context, p string) (sandbox.FileInfo, error) {
	if f.repo && (p == "/workspace/repo" || p == "/workspace/repo/sub") || p == "/workspace" {
		return sandbox.FileInfo{Path: p, Type: "directory"}, nil
	}
	return sandbox.FileInfo{}, sandbox.ErrNotFound
}

type fakeManager struct{ sb *fakeSandbox }

func (m fakeManager) Enabled() bool { return true }
func (m fakeManager) Acquire(context.Context, string, string) (sandbox.Sandbox, error) {
	return m.sb, nil
}
func (m fakeManager) Get(context.Context, string) (sandbox.Sandbox, error) { return m.sb, nil }
func (m fakeManager) Kill(context.Context, string) error                   { return nil }
func (m fakeManager) Invalidate(context.Context, string, string)           {}

type fakeStore struct {
	mu     sync.Mutex
	info   *factory.ThreadInfo
	task   *factory.Task
	events []factory.TaskEvent
	spends []factory.Spend
}

func (s *fakeStore) GetThreadInfo(context.Context, string) (*factory.ThreadInfo, error) {
	if s.info == nil {
		return nil, factory.ErrNotFound
	}
	return s.info, nil
}
func (s *fakeStore) GetTask(context.Context, string) (*factory.Task, error) {
	if s.task == nil {
		return nil, factory.ErrNotFound
	}
	return s.task, nil
}
func (s *fakeStore) AddSpend(_ context.Context, _ string, d factory.Spend) (*factory.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.spends = append(s.spends, d)
	return s.task, nil
}
func (s *fakeStore) AppendEvent(_ context.Context, e factory.TaskEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
	return nil
}

type creds map[string]string

func (c creds) Credentials(context.Context, string, string) (map[string]string, error) { return c, nil }

const claudeSample = `{"type":"system","subtype":"init","session_id":"s1"}
{"type":"assistant","message":{"content":[{"type":"text","text":"I will fix the bug."},{"type":"tool_use","name":"Edit","input":{"file_path":"/workspace/repo/a.go"}}]}}
{"type":"result","subtype":"success","is_error":false,"result":"Fixed a.go; tests pass.","total_cost_usd":0.0421,"usage":{"input_tokens":1200,"output_tokens":300,"cache_read_input_tokens":4000}}
`

func gitHandlers() []func(sandbox.ExecRequest) (string, string, int, bool, bool) {
	return []func(sandbox.ExecRequest) (string, string, int, bool, bool){
		func(r sandbox.ExecRequest) (string, string, int, bool, bool) {
			switch {
			case strings.HasPrefix(r.Command, "git rev-parse"):
				return "abc1234def\n", "", 0, false, true
			case strings.HasPrefix(r.Command, "git diff --stat"):
				return " a.go | 2 +-\n 1 file changed, 1 insertion(+), 1 deletion(-)\n", "", 0, false, true
			case strings.HasPrefix(r.Command, "{ git diff --name-only"):
				return "a.go\nnew.txt\n", "", 0, false, true
			case strings.HasPrefix(r.Command, "command -v"):
				return "", "", 0, false, true
			}
			return "", "", 0, false, false
		},
	}
}

func newProvider(sb *fakeSandbox, st TaskStore, c executors.CredentialSource) *Provider {
	return New(Options{Sandboxes: fakeManager{sb}, Credentials: c, Store: st})
}

func TestRunClaudeStreamsSummarizesAndBooks(t *testing.T) {
	sb := &fakeSandbox{repo: true}
	sb.handlers = append([]func(sandbox.ExecRequest) (string, string, int, bool, bool){
		func(r sandbox.ExecRequest) (string, string, int, bool, bool) {
			if strings.HasPrefix(r.Command, "claude -p") {
				return claudeSample, "", 0, false, true
			}
			return "", "", 0, false, false
		}}, gitHandlers()...)
	st := &fakeStore{info: &factory.ThreadInfo{ThreadID: "t1", Login: "alice", TaskID: "task1", Policy: factory.PolicySandboxed},
		task: &factory.Task{ID: "task1"}}
	p := newProvider(sb, st, creds{"ANTHROPIC_API_KEY": "sk-ant-secretvalue12345", "EVIL": "x"})
	var live []string
	res, err := p.Run(context.Background(), executors.Request{ThreadID: "t1", Executor: "claude-code", Task: "fix the bug"}, func(s string) { live = append(live, s) })
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || res.Summary != "Fixed a.go; tests pass." || res.Level != executors.LevelEdit {
		t.Fatalf("result %+v", res)
	}
	if res.USD != 0.0421 || res.Tokens != 5500 {
		t.Fatalf("usage %v %v", res.USD, res.Tokens)
	}
	if len(res.Changed) != 2 || !strings.Contains(res.DiffStat, "1 file changed") {
		t.Fatalf("changes %+v %q", res.Changed, res.DiffStat)
	}
	joined := strings.Join(live, "")
	if !strings.Contains(joined, "I will fix the bug.") || !strings.Contains(joined, "[tool Edit] /workspace/repo/a.go") {
		t.Fatalf("live output %q", joined)
	}
	// The command: no task in it, the task in the environment, the policy's flags.
	var cli int = -1
	for i, c := range sb.cmds {
		if strings.HasPrefix(c, "claude -p") {
			cli = i
		}
	}
	if cli < 0 {
		t.Fatalf("CLI never ran: %q", sb.cmds)
	}
	if c := sb.cmds[cli]; strings.Contains(c, "fix the bug") || !strings.Contains(c, `"$HARNESS_TASK"`) || !strings.Contains(c, "--permission-mode acceptEdits") {
		t.Fatalf("command %q", c)
	}
	env := sb.envs[cli]
	if env["HARNESS_TASK"] != "fix the bug" || env["ANTHROPIC_API_KEY"] != "sk-ant-secretvalue12345" {
		t.Fatalf("env %v", env)
	}
	if _, leaked := env["EVIL"]; leaked {
		t.Fatal("a variable the executor did not declare was injected")
	}
	if sb.cwds[cli] != "/workspace/repo" {
		t.Fatalf("cwd %q", sb.cwds[cli])
	}
	// Spend booked, events recorded.
	if len(st.spends) != 1 || st.spends[0].USD != 0.0421 || st.spends[0].Tokens != 5500 {
		t.Fatalf("spends %+v", st.spends)
	}
	var kinds []string
	for _, e := range st.events {
		kinds = append(kinds, e.Kind)
	}
	if strings.Join(kinds, ",") != "policy,executor" {
		t.Fatalf("events %v", kinds)
	}
	if !strings.Contains(Render2(res), "Fixed a.go") {
		t.Fatal("report")
	}
}

func Render2(r *executors.Result) string { return executors.Render(r) }

func TestRedactsCredentialsEverywhere(t *testing.T) {
	const key = "topsecretvalue-12345"
	sb := &fakeSandbox{}
	sb.handlers = append([]func(sandbox.ExecRequest) (string, string, int, bool, bool){
		func(r sandbox.ExecRequest) (string, string, int, bool, bool) {
			if strings.HasPrefix(r.Command, "opencode run") {
				return "using key " + key + "\ndone with " + key + "\n", "warning " + key + "\n", 0, false, true
			}
			return "", "", 0, false, false
		}}, gitHandlers()...)
	p := newProvider(sb, nil, creds{"OPENROUTER_API_KEY": key})
	var live []string
	res, err := p.Run(context.Background(), executors.Request{Login: "alice", ThreadID: "t", Executor: "opencode", Task: "x"}, func(s string) { live = append(live, s) })
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(live, "") + res.Summary + res.Output + res.DiffStat + executors.Render(res)
	if strings.Contains(all, key) {
		t.Fatalf("the credential leaked: %q", all)
	}
	if !strings.Contains(all, "***") {
		t.Fatalf("expected a mask in %q", all)
	}
}

func TestPolicyLevels(t *testing.T) {
	cases := []struct {
		policy   string
		readOnly bool
		exec     string
		want     string
	}{
		{factory.PolicyAutonomous, false, "claude-code", "--dangerously-skip-permissions"},
		{factory.PolicySupervised, false, "claude-code", "--permission-mode acceptEdits"},
		{factory.PolicySandboxed, true, "claude-code", "--permission-mode plan"},
		{factory.PolicySandboxed, true, "codex", "--sandbox read-only"},
		{factory.PolicySandboxed, false, "codex", "--sandbox workspace-write"},
		{factory.PolicyAutonomous, false, "codex", "--dangerously-bypass-approvals-and-sandbox"},
	}
	for _, c := range cases {
		sb := &fakeSandbox{}
		sb.handlers = gitHandlers()
		st := &fakeStore{info: &factory.ThreadInfo{ThreadID: "t", Login: "a", TaskID: "k", Policy: c.policy, ReadOnly: c.readOnly}, task: &factory.Task{ID: "k"}}
		if _, err := newProvider(sb, st, nil).Run(context.Background(), executors.Request{ThreadID: "t", Executor: c.exec, Task: "x"}, nil); err != nil {
			t.Fatalf("%v: %v", c, err)
		}
		found := false
		for _, cmd := range sb.cmds {
			found = found || strings.HasPrefix(cmd, strings.Fields(c.exec)[0]) && strings.Contains(cmd, c.want) || c.exec == "claude-code" && strings.HasPrefix(cmd, "claude") && strings.Contains(cmd, c.want)
		}
		if !found {
			t.Errorf("%v: no command with %q in %q", c, c.want, sb.cmds)
		}
		if st.events[0].Kind != factory.EventPolicy || st.events[0].Data["level"] != string(executors.LevelFor(c.policy, c.readOnly)) {
			t.Errorf("%v: policy event %+v", c, st.events[0])
		}
	}
}

func TestReadOnlyRefusedForOpenCode(t *testing.T) {
	sb := &fakeSandbox{}
	_, err := newProvider(sb, nil, nil).Run(context.Background(), executors.Request{Login: "a", ThreadID: "t", Executor: "opencode", Task: "x", ReadOnly: true}, nil)
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("err %v", err)
	}
	if len(sb.cmds) != 0 {
		t.Fatalf("nothing should run: %q", sb.cmds)
	}
}

func TestTimeoutKillsAndReportsPartialWork(t *testing.T) {
	sb := &fakeSandbox{}
	sb.handlers = append([]func(sandbox.ExecRequest) (string, string, int, bool, bool){
		func(r sandbox.ExecRequest) (string, string, int, bool, bool) {
			if strings.HasPrefix(r.Command, "codex exec") {
				return `{"type":"item.completed","item":{"type":"agent_message","text":"working"}}` + "\n", "", 0, true, true
			}
			return "", "", 0, false, false
		}}, gitHandlers()...)
	p := New(Options{Sandboxes: fakeManager{sb}, Timeout: 50 * time.Millisecond})
	res, err := p.Run(context.Background(), executors.Request{Login: "a", ThreadID: "t", Executor: "codex", Task: "x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || !res.Failed() || len(res.Changed) == 0 {
		t.Fatalf("%+v", res)
	}
	if !strings.Contains(executors.Render(res), "killed after its timeout") {
		t.Fatal(executors.Render(res))
	}
}

func TestCancelStopsTheRunAndRecordsIt(t *testing.T) {
	sb := &fakeSandbox{}
	sb.handlers = append([]func(sandbox.ExecRequest) (string, string, int, bool, bool){
		func(r sandbox.ExecRequest) (string, string, int, bool, bool) {
			if strings.HasPrefix(r.Command, "opencode run") {
				return "", "", 0, true, true
			}
			return "", "", 0, false, false
		}}, gitHandlers()...)
	st := &fakeStore{info: &factory.ThreadInfo{ThreadID: "t", Login: "a", TaskID: "k"}, task: &factory.Task{ID: "k"}}
	p := newProvider(sb, st, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := p.Run(ctx, executors.Request{ThreadID: "t", Executor: "opencode", Task: "x"}, nil)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the run did not stop")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	last := st.events[len(st.events)-1]
	if last.Kind != factory.EventExecutor || !strings.Contains(last.Text, "cancelled") {
		t.Fatalf("events %+v", st.events)
	}
}

func TestMissingCLIAndInstall(t *testing.T) {
	missing := func(installed *bool) func(sandbox.ExecRequest) (string, string, int, bool, bool) {
		return func(r sandbox.ExecRequest) (string, string, int, bool, bool) {
			switch {
			case strings.HasPrefix(r.Command, "command -v"):
				if *installed {
					return "", "", 0, false, true
				}
				return "", "", 1, false, true
			case strings.HasPrefix(r.Command, "npm install"):
				*installed = true
				return "added 1 package\n", "", 0, false, true
			}
			return "", "", 0, false, false
		}
	}
	// Not asked: the error offers the install command.
	var inst bool
	sb := &fakeSandbox{handlers: []func(sandbox.ExecRequest) (string, string, int, bool, bool){missing(&inst)}}
	_, err := New(Options{Sandboxes: fakeManager{sb}, AllowInstall: true}).Run(context.Background(), executors.Request{Login: "a", ThreadID: "t", Executor: "codex", Task: "x"}, nil)
	if err == nil || !strings.Contains(err.Error(), "install=true") || !strings.Contains(err.Error(), "npm install -g @openai/codex") {
		t.Fatalf("err %v", err)
	}
	// Asked but the server does not allow it: nothing is installed.
	inst = false
	sb = &fakeSandbox{handlers: []func(sandbox.ExecRequest) (string, string, int, bool, bool){missing(&inst)}}
	_, err = New(Options{Sandboxes: fakeManager{sb}}).Run(context.Background(), executors.Request{Login: "a", ThreadID: "t", Executor: "codex", Task: "x", Install: true}, nil)
	if err == nil || !strings.Contains(err.Error(), "does not allow") || inst {
		t.Fatalf("err %v installed %v", err, inst)
	}
	// Asked and allowed: installed, then run.
	inst = false
	sb = &fakeSandbox{handlers: append([]func(sandbox.ExecRequest) (string, string, int, bool, bool){missing(&inst)}, gitHandlers()...)}
	res, err := New(Options{Sandboxes: fakeManager{sb}, AllowInstall: true}).Run(context.Background(), executors.Request{Login: "a", ThreadID: "t", Executor: "codex", Task: "x", Install: true}, nil)
	if err != nil || !inst || res.ExitCode != 0 {
		t.Fatalf("res %+v err %v installed %v", res, err, inst)
	}
}

func TestBudgetExhaustedRefusesToRun(t *testing.T) {
	sb := &fakeSandbox{}
	st := &fakeStore{info: &factory.ThreadInfo{ThreadID: "t", Login: "a", TaskID: "k"},
		task: &factory.Task{ID: "k", Budget: factory.Budget{MaxUSD: 1}, Spent: factory.Spend{USD: 2}}}
	_, err := newProvider(sb, st, nil).Run(context.Background(), executors.Request{ThreadID: "t", Executor: "codex", Task: "x"}, nil)
	if err == nil || !strings.Contains(err.Error(), "budget") || len(sb.cmds) != 0 {
		t.Fatalf("err %v cmds %q", err, sb.cmds)
	}
}

func TestWorkdirMustStayInWorkspace(t *testing.T) {
	sb := &fakeSandbox{repo: true, handlers: gitHandlers()}
	p := newProvider(sb, nil, nil)
	for _, w := range []string{"/etc", "../etc", "/workspace/../etc"} {
		if _, err := p.Run(context.Background(), executors.Request{Login: "a", ThreadID: "t", Executor: "codex", Task: "x", Workdir: w}, nil); err == nil {
			t.Errorf("workdir %q accepted", w)
		}
	}
	if _, err := p.Run(context.Background(), executors.Request{Login: "a", ThreadID: "t", Executor: "codex", Task: "x", Workdir: "repo/sub"}, nil); err != nil {
		t.Fatal(err)
	}
	if sb.cwds[len(sb.cwds)-1] == "" {
		t.Fatal("no cwd")
	}
}

func TestFailedExitCodeIsAReportNotAnError(t *testing.T) {
	sb := &fakeSandbox{}
	sb.handlers = append([]func(sandbox.ExecRequest) (string, string, int, bool, bool){
		func(r sandbox.ExecRequest) (string, string, int, bool, bool) {
			if strings.HasPrefix(r.Command, "opencode run") {
				return "compile error\n", "boom\n", 2, false, true
			}
			return "", "", 0, false, false
		}}, gitHandlers()...)
	res, err := newProvider(sb, nil, nil).Run(context.Background(), executors.Request{Login: "a", ThreadID: "t", Executor: "opencode", Task: "x"}, nil)
	if err != nil || res.ExitCode != 2 || !strings.HasPrefix(executors.Render(res), "EXECUTOR FAILED") {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestToolIsOfferedAndRuns(t *testing.T) {
	sb := &fakeSandbox{handlers: gitHandlers()}
	p := newProvider(sb, nil, nil)
	ts, _, err := p.Tools(context.Background(), tools.RunContext{ThreadID: "t", Login: "a"})
	if err != nil || len(ts) != 1 {
		t.Fatalf("tools %v %v", ts, err)
	}
	if ts, _, _ := p.Tools(context.Background(), tools.RunContext{ThreadID: "t", Login: "a", Sandbox: "off"}); len(ts) != 0 {
		t.Fatal("the tool must not be offered without a sandbox")
	}
	if ts, _, _ := New(Options{}).Tools(context.Background(), tools.RunContext{ThreadID: "t"}); len(ts) != 0 {
		t.Fatal("no sandbox manager, no tool")
	}
}

func TestUnknownExecutorAndEmptyTask(t *testing.T) {
	p := newProvider(&fakeSandbox{}, nil, nil)
	if _, err := p.Run(context.Background(), executors.Request{Login: "a", ThreadID: "t", Executor: "nope", Task: "x"}, nil); err == nil || !strings.Contains(err.Error(), "unknown executor") {
		t.Fatalf("%v", err)
	}
	if _, err := p.Run(context.Background(), executors.Request{Login: "a", ThreadID: "t", Executor: "codex", Task: "  "}, nil); err == nil {
		t.Fatal("empty task accepted")
	}
}

// The text omp (v19) really prints when it starts without a key, captured by running the CLI with no credentials.
func TestDiagnoseOmpWithoutAKey(t *testing.T) {
	omp, _ := executors.BuiltinNamed(executors.OMP)
	out := "No default model selected. Use /login, set an API key environment variable, or select a local model with /model or --model.\n\nSet an API key environment variable:\n  ANTHROPIC_API_KEY, OPENAI_API_KEY, GEMINI_API_KEY, etc.\n"
	got := Diagnose(omp, out)
	if !strings.Contains(got, "could not authenticate") || !strings.Contains(got, "Settings > Executors") || !strings.Contains(got, "ANTHROPIC_API_KEY") {
		t.Fatalf("Diagnose = %q", got)
	}
}
