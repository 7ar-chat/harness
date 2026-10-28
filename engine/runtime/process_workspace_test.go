package runtime_test

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/engine/agent"
	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/engine/runtime"
	"github.com/GhaziBenDahmane/harness/engine/tools"
	"github.com/GhaziBenDahmane/harness/engine/tools/sandboxtools"
	"github.com/GhaziBenDahmane/harness/features/plan"
	"github.com/GhaziBenDahmane/harness/features/workspace"
	"github.com/GhaziBenDahmane/harness/redisx"
	"github.com/GhaziBenDahmane/harness/sandbox"
	"github.com/GhaziBenDahmane/harness/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/store"
)

// wsRig is the v2 rig with the workspace checkpoints wired like cmd/work
// (real git / tar scripts on the in-memory sandboxes).
type wsRig struct {
	*v2rig
	ws *workspace.Service
}

func newWSRig(t *testing.T, turns ...llmtest.Turn) *wsRig {
	t.Helper()
	for _, tool := range []string{"bash", "git", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	r := &wsRig{v2rig: newV2Rig(t, false, turns...)}
	r.backend.OnCreate = func(sb *sandboxtest.Sandbox) { sb.SetExec(sandboxtest.FindExec(sb, sandboxtest.ShellExec(sb))) }
	r.ws = workspace.New(nil, r.st, r.st, r.arts, workspace.Options{})
	h := &r.agent.Harness
	h.Sections = append(h.Sections, r.ws.PromptSection)
	h.RunStart = []agent.RunStartHook{r.ws}
	// Like cmd/work: the artifacts snapshot, then the history archive.
	r.opts.Hooks = append([]runtime.Hook{r.opts.Hooks[0], workspace.EndOfRunHook{Svc: r.ws}}, r.opts.Hooks[1:]...)
	r.wireWS()
	return r
}

// wireWS (re)creates the manager and tools like a runtime process start.
func (r *wsRig) wireWS() {
	r.mgr = sandbox.NewManager(r.backend, r.st, redisx.NewCoordinator(r.rdb), sandbox.ManagerOptions{MaxPerUser: 5, Timeout: time.Hour,
		OnCreate: func(ctx context.Context, threadID string, sb sandbox.Sandbox, recreated bool) string {
			r.ws.OnCreate(ctx, threadID, sb)
			return r.arts.RestoreWorkspace(ctx, threadID, sb, recreated)
		}})
	r.arts.Sandbox, r.ws.Sandbox = r.mgr, r.mgr
	r.agent.Tools = tools.NewRegistry(
		sandboxtools.New(r.mgr, sandboxtools.Options{CommandTimeout: 5 * time.Second, MaxSandboxesPerUser: 5,
			OnAcquire: func(ctx context.Context, rc tools.RunContext, sb sandbox.Sandbox) {
				r.inst.EnsureRunSkills(ctx, rc, sb)
				r.ws.OnAcquire(ctx, rc, sb)
			}}),
		harnesstools.New(r.st),
	)
	r.rt = nil
}

// archived waits for the background history archives of finished runs.
func (r *wsRig) archived() {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !r.ws.WaitArchives(ctx) {
		r.t.Fatal("history archives still running")
	}
}

func (r *wsRig) history(threadID string) []string {
	r.t.Helper()
	list, err := r.ws.List(context.Background(), threadID, "")
	if err != nil {
		r.t.Fatal(err)
	}
	out := make([]string, 0, len(list))
	for _, c := range list {
		out = append(out, c.Label+" ["+strings.Join(c.ChangedFiles, ",")+"]")
	}
	return out
}

func TestRunsCheckpointTheWorkspaceBeforeTheAITurn(t *testing.T) {
	// The first run's sandbox is created mid-run: its first tool acquisition takes the initial checkpoint; the next run, with a live sandbox, checkpoints the previous run's files before its first step; the history is archived after each run.
	// Arrange
	r := newWSRig(t,
		llmtest.Turn{Chunks: streamedCall("call_1", "write_file", `{"path":"report.md","content":"v1"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
		llmtest.Turn{Chunks: streamedCall("call_2", "write_file", `{"path":"report.md","content":"v2"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Updated")},
	)
	th, first := r.queued("write the report")

	// Act
	r.process(first)
	r.archived()
	afterFirst := r.history(th.ID)
	arch1, _ := r.st.WorkspaceGit(context.Background(), th.ID)
	second := r.queuedOn(th, "update it")
	r.process(second)
	r.archived()
	afterSecond := r.history(th.ID)
	arch2, _ := r.st.WorkspaceGit(context.Background(), th.ID)

	// Assert
	if len(afterFirst) != 1 || afterFirst[0] != "Initial workspace checkpoint []" || arch1 == nil {
		t.Fatalf("after the first run %v archive %+v", afterFirst, arch1)
	}
	if len(afterSecond) != 2 || afterSecond[0] != "Before AI turn [/workspace/report.md]" || arch2 == nil || arch2.Head == arch1.Head {
		t.Fatalf("after the second run %v archive %+v", afterSecond, arch2)
	}
}

func TestCheckpointHistoryFollowsTheThreadIntoANewSandbox(t *testing.T) {
	// The sandbox expires between runs: the new one gets the archived history, and its first acquisition checkpoints the restored files.
	// Arrange
	r := newWSRig(t,
		llmtest.Turn{Chunks: streamedCall("call_1", "write_file", `{"path":"a.md","content":"a"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
		llmtest.Turn{Chunks: streamedCall("call_2", "write_file", `{"path":"b.md","content":"b"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
	)
	th, first := r.queued("a")
	r.process(first)
	r.archived()
	old := r.sandbox()
	r.backend.Forget(old.ID())
	r.wireWS()

	// Act
	second := r.queuedOn(th, "b")
	got := r.process(second)

	// Assert
	fresh := r.sandbox()
	history := r.history(th.ID)
	if got.Status != store.StatusCompleted || fresh.ID() == old.ID() || len(history) != 2 ||
		history[0] != "Before AI turn [/workspace/a.md]" || history[1] != "Initial workspace checkpoint []" {
		t.Fatalf("status %s sandboxes %s/%s history %v", got.Status, old.ID(), fresh.ID(), history)
	}
}

func TestRunCompletionDoesNotWaitForTheHistoryArchive(t *testing.T) {
	// The .git archive is uploaded in the background: the run is terminal (and its RUN_FINISHED written) while the archive still runs; it lands afterwards.
	// Arrange
	r := newWSRig(t,
		llmtest.Turn{Chunks: streamedCall("call_1", "write_file", `{"path":"a.md","content":"a"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done")},
	)
	release := make(chan struct{})
	r.backend.OnCreate = func(sb *sandboxtest.Sandbox) {
		shell := sandboxtest.FindExec(sb, sandboxtest.ShellExec(sb))
		sb.SetExec(func(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
			if strings.Contains(req.Command, "tar -czf") {
				<-release // a slow upload
			}
			return shell(ctx, req)
		})
	}
	th, run := r.queued("a")

	// Act
	done := make(chan *store.Run, 1)
	go func() { done <- r.process(run) }()
	var finished *store.Run
	select {
	case finished = <-done:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("the run waited for the archive")
	}
	_, beforeErr := r.st.WorkspaceGit(context.Background(), th.ID)
	close(release)
	r.archived()
	arch, afterErr := r.st.WorkspaceGit(context.Background(), th.ID)

	// Assert
	if finished.Status != store.StatusCompleted || beforeErr == nil {
		t.Fatalf("status %s archive before %v", finished.Status, beforeErr)
	}
	if afterErr != nil || arch == nil {
		t.Fatalf("archive after %+v %v", arch, afterErr)
	}
}
