package workflows_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/store/workflowstore"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	core "github.com/7ar-chat/harness/core/workflows"
	wf "github.com/7ar-chat/harness/features/workflows"
)

type execRig struct {
	*rig
	sb     *sandboxtest.HostSandbox
	repos  []string // "repo@branch" of every sandbox request
	judge  func(string) wf.Verdict
	prevOK bool
}

func newExecRig(t *testing.T) *execRig {
	t.Helper()
	x := &execRig{sb: sandboxtest.NewHostSandbox("sb1", t.TempDir())}
	r := &rig{t: t, ctx: context.Background(), store: workflowstore.NewMem(nil), tasks: newFake()}
	r.eng = wf.New(wf.Options{
		Store: r.store, Tasks: r.tasks, FlushEvery: 20 * time.Millisecond,
		Sandbox: func(_ context.Context, _ core.Run, repo, branch string) (sandbox.Sandbox, error) {
			x.repos = append(x.repos, repo+"@"+branch)
			return x.sb, nil
		},
		Judge: func(_ context.Context, _ core.Run, _ core.Node, cmd string) wf.Verdict {
			if x.judge != nil {
				return x.judge(cmd)
			}
			return wf.Verdict{}
		},
		Preview: func(context.Context, core.Run, core.Node, sandbox.Sandbox) (string, string, error) {
			if x.prevOK {
				return "/preview/x/", "app started\n", nil
			}
			return "", "", wf.ErrNoPreview
		},
	})
	x.rig = r
	return x
}

func script(id, cmd string, deps ...string) core.Node {
	return core.Node{ID: id, Kind: core.KindScript, Command: cmd, DependsOn: deps}
}

func (x *execRig) settle(run *core.Run, node string, want core.NodeState) *core.Run {
	x.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		got, err := x.eng.Advance(x.ctx, run.ID)
		if err != nil {
			x.t.Fatal(err)
		}
		if got.Step(node).State == want {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, _ := x.store.GetRun(x.ctx, run.ID)
	x.t.Fatalf("step %s is %s, want %s", node, got.Step(node).State, want)
	return nil
}

func TestScriptStepRunsInTheSandboxWithoutAModel(t *testing.T) {
	// Arrange
	x := newExecRig(t)
	w := x.publish("direct", script("s", "echo hello-$(( 40 + 2 ))"), agent("next", "s"))

	// Act
	run := x.start(w)
	run = x.settle(run, "s", core.NodeDone)

	// Assert
	s := run.Step("s")
	if !s.Direct || s.TaskID != "" || s.ExitCode == nil || *s.ExitCode != 0 {
		t.Fatalf("step = %+v", s)
	}
	if !strings.Contains(s.Log, "hello-42") || !strings.Contains(s.Result, "hello-42") {
		t.Fatalf("log %q result %q", s.Log, s.Result)
	}
	if got := x.tasks.find(run.ID, "s"); got != nil {
		t.Fatalf("a direct step must not create a task: %+v", got)
	}
	if x.tasks.find(run.ID, "next") == nil {
		t.Fatal("the dependent step did not start")
	}
}

func TestScriptStepFailsOnANonZeroExitAndStreamsItsOutput(t *testing.T) {
	// Arrange
	x := newExecRig(t)
	w := x.publish("fails", script("s", "echo first; sleep 0.3; echo boom >&2; exit 3"))

	// Act
	run := x.start(w)
	var streamed bool
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !streamed {
		got, _ := x.store.GetRun(x.ctx, run.ID)
		streamed = strings.Contains(got.Step("s").Log, "first") && got.Step("s").State == core.NodeRunning
		time.Sleep(10 * time.Millisecond)
	}
	run = x.settle(run, "s", core.NodeFailed)

	// Assert
	if !streamed {
		t.Error("the output was not streamed into the step while it ran")
	}
	s := run.Step("s")
	if s.ExitCode == nil || *s.ExitCode != 3 || !strings.Contains(s.Error, "status 3") || !strings.Contains(s.Log, "boom") {
		t.Fatalf("step = %+v", s)
	}
	if run.State != core.RunFailed {
		t.Fatalf("run = %s", run.State)
	}
}

func TestScriptStepTimesOut(t *testing.T) {
	// Arrange
	x := newExecRig(t)
	x.eng = wf.New(wf.Options{Store: x.store, Tasks: x.tasks, ExecTimeout: 200 * time.Millisecond,
		Sandbox: func(context.Context, core.Run, string, string) (sandbox.Sandbox, error) { return x.sb, nil }})
	w := x.publish("slow", script("s", "sleep 30"))

	// Act
	run := x.start(w)
	run = x.settle(run, "s", core.NodeFailed)

	// Assert
	if !strings.Contains(run.Step("s").Error, "timed out") {
		t.Fatalf("error = %q", run.Step("s").Error)
	}
}

func TestSupervisedScriptWaitsForApprovalAndDenialFails(t *testing.T) {
	// Arrange
	x := newExecRig(t)
	x.judge = func(cmd string) wf.Verdict {
		if strings.Contains(cmd, "sudo") {
			return wf.Verdict{Deny: "sudo is blocked"}
		}
		return wf.Verdict{Ask: true, Reason: "supervised: asks before running a command"}
	}
	w := x.publish("ask", script("s", "echo approved-run"))

	// Act
	run := x.start(w)
	if state(run, "s") != core.NodeWaiting || x.sb.Commands() != 0 {
		t.Fatalf("the step must wait before running anything: %s, %d commands", state(run, "s"), x.sb.Commands())
	}
	run, err := x.eng.Approve(x.ctx, run.ID, "s", "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	run = x.settle(run, "s", core.NodeDone)

	// Assert
	if !strings.Contains(run.Step("s").Log, "approved-run") {
		t.Fatalf("log = %q", run.Step("s").Log)
	}
	w2 := x.publish("deny", script("s", "sudo rm -rf /"))
	run2 := x.start(w2)
	if state(run2, "s") != core.NodeFailed || !strings.Contains(run2.Step("s").Error, "sudo is blocked") {
		t.Fatalf("denied step = %+v", run2.Step("s"))
	}
}

func TestDeclineingAScriptApprovalFailsTheStep(t *testing.T) {
	// Arrange
	x := newExecRig(t)
	x.judge = func(string) wf.Verdict { return wf.Verdict{Ask: true} }
	w := x.publish("decline", script("s", "echo no"))
	run := x.start(w)

	// Act
	run, err := x.eng.SendBack(x.ctx, run.ID, "s", "bob", "not this")
	if err != nil {
		t.Fatal(err)
	}

	// Assert
	if !strings.Contains(run.Step("s").Error, "declined by bob") {
		t.Fatalf("step = %+v", run.Step("s"))
	}
}

func TestPreviewStepRecordsTheURLAndFallsBackToItsCommand(t *testing.T) {
	// Arrange
	x := newExecRig(t)
	x.prevOK = true
	w := x.publish("prev", core.Node{ID: "p", Kind: core.KindPreview, Command: "echo should-not-run"})

	// Act
	run := x.start(w)
	run = x.settle(run, "p", core.NodeDone)

	// Assert
	if run.Step("p").URL != "/preview/x/" || strings.Contains(run.Step("p").Log, "should-not-run") {
		t.Fatalf("step = %+v", run.Step("p"))
	}
	x.prevOK = false
	run = x.start(x.publish("prev2", core.Node{ID: "p", Kind: core.KindPreview, Command: "echo ran-as-script"}))
	run = x.settle(run, "p", core.NodeDone)
	if !strings.Contains(run.Step("p").Log, "ran-as-script") {
		t.Fatalf("fallback log = %q", run.Step("p").Log)
	}
}

func TestWithoutASandboxTheCommandRunsAsATask(t *testing.T) {
	// Arrange
	r := newRig(t)
	r.eng = wf.New(wf.Options{Store: r.store, Tasks: r.tasks,
		Sandbox:        func(context.Context, core.Run, string, string) (sandbox.Sandbox, error) { return nil, errors.New("unused") },
		SandboxEnabled: func() bool { return false }})
	w := r.publish("fallback", script("s", "make test"))

	// Act
	run := r.start(w)

	// Assert
	if r.tasks.find(run.ID, "s") == nil || run.Step("s").Direct {
		t.Fatalf("with no sandbox the step must run as a task: %+v", run.Step("s"))
	}
}

func TestEachRepositoryOfARunHasItsOwnBranch(t *testing.T) {
	// Arrange
	x := newExecRig(t)
	_ = x.sb.MkdirAll(context.Background(), "/workspace/repo")
	a := core.Node{ID: "a", Kind: core.KindAgent, Prompt: "x", UseRepo: true}
	b := core.Node{ID: "b", Kind: core.KindAgent, Prompt: "y", UseRepo: true, Repo: "acme/other", DependsOn: []string{"a"}}
	c := core.Node{ID: "c", Kind: core.KindScript, Command: "pwd", UseRepo: true, Repo: "acme/other", DependsOn: []string{"b"}}
	w := x.publish("multi", a, b, c)
	run, err := x.eng.Start(x.ctx, core.StartRequest{ProjectID: "p1", WorkflowID: w.ID, Owner: "alice", Repo: "acme/main", BaseBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}

	// Act
	run = x.done(run, "a", "A")
	run = x.done(run, "b", "B")
	ta, tb := x.tasks.find(run.ID, "a"), x.tasks.find(run.ID, "b")

	// Assert
	if ta.Repo != "acme/main" || ta.Branch != run.Branch || ta.BaseBranch != "main" {
		t.Fatalf("a = %+v", ta)
	}
	if tb.Repo != "acme/other" || tb.Branch == run.Branch || !strings.HasPrefix(tb.Branch, "workflow/") || tb.BaseBranch != "" {
		t.Fatalf("b must work on its own branch: %+v (run branch %s)", tb, run.Branch)
	}
	run = x.settle(run, "c", core.NodeDone)
	if len(x.repos) != 1 || x.repos[0] != "acme/other@"+tb.Branch {
		t.Fatalf("sandbox requests = %v", x.repos)
	}
}
