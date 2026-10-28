package sandboxtools_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/cloudwego/eino/components/tool"
	"github.com/redis/go-redis/v9"

	"github.com/GhaziBenDahmane/harness/sandbox"
	"github.com/GhaziBenDahmane/harness/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/store/memstore"
	"github.com/GhaziBenDahmane/harness/tools"
	"github.com/GhaziBenDahmane/harness/tools/sandboxtools"
)

// newKeepAliveFixture renews leases every 5 ms and counts the live job keep-alives.
func newKeepAliveFixture(t *testing.T, job sandboxtest.JobFunc) (*fixture, *atomic.Int32) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	st := memstore.New()
	th, _ := st.CreateThread(context.Background(), "alice", "t")
	f := &fixture{backend: sandboxtest.NewBackend(), tools: map[string]tool.InvokableTool{}}
	f.backend.OnCreate = func(sb *sandboxtest.Sandbox) { f.sb = sb; sb.SetJobs(job) }
	mgr := sandbox.NewManager(f.backend, st, rdb, sandbox.ManagerOptions{MaxPerUser: 5, Timeout: time.Hour, RenewEvery: 5 * time.Millisecond})
	var live atomic.Int32
	p := sandboxtools.New(mgr, sandboxtools.Options{CommandTimeout: time.Second, JobPollEvery: 10 * time.Millisecond,
		KeepAlive: func(ctx context.Context, sb sandbox.Sandbox) func() {
			live.Add(1)
			stop := sandbox.KeepAlive(ctx, sb)
			return func() { stop(); live.Add(-1) }
		}})
	ts, _, err := p.Tools(context.Background(), tools.RunContext{ThreadID: th.ID, RunID: "r", Login: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range ts {
		info, _ := x.Info(context.Background())
		f.tools[info.Name] = x.(tool.InvokableTool)
	}
	return f, &live
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestABackgroundJobKeepsItsSandboxAliveUntilJobKill(t *testing.T) {
	// A job still running after execute_command returned renews its sandbox (sandbox.KeepAlive) until job_kill stops it.
	// Arrange
	f, live := newKeepAliveFixture(t, blockingJob(make(chan struct{})))
	ctx, cancel := context.WithCancel(context.Background())

	// Act
	out, err := f.call(ctx, "execute_command", `{"command":"python3 server.py","background":true}`)
	cancel() // the tool call (and its run) is over: the job keeps the sandbox anyway
	r := decodeExec(t, out)
	before := f.sb.Renewals()
	eventually(t, "renewals while the job runs", func() bool { return f.sb.Renewals() >= before+3 })
	kept := live.Load()
	_, kerr := f.call(context.Background(), "job_kill", `{"jobId":"`+r.JobID+`"}`)

	// Assert
	if err != nil || r.Status != "running" || kept != 1 || kerr != nil {
		t.Fatalf("err %v out %s kept %d kill %v", err, out, kept, kerr)
	}
	if n := live.Load(); n != 0 {
		t.Fatalf("keep-alive still running after job_kill: %d", n)
	}
}

func TestABackgroundJobStopsKeepingItsSandboxWhenItEnds(t *testing.T) {
	// The keep-alive polls the job: once it finished, the sandbox is no longer renewed for it, without any job_output call.
	// Arrange
	release := make(chan struct{})
	f, live := newKeepAliveFixture(t, blockingJob(release))

	// Act
	out, err := f.call(context.Background(), "execute_command", `{"command":"make all","timeoutSeconds":1}`)
	r := decodeExec(t, out)
	kept := live.Load()
	close(release)
	eventually(t, "the keep-alive to end with the job", func() bool { return live.Load() == 0 })
	after := f.sb.Renewals()
	time.Sleep(50 * time.Millisecond)

	// Assert
	if err != nil || r.Status != "running" || kept != 1 {
		t.Fatalf("err %v out %s kept %d", err, out, kept)
	}
	if n := f.sb.Renewals(); n != after {
		t.Fatalf("renewed after the job ended: %d → %d", after, n)
	}
}

func TestAFinishedCommandKeepsNoSandboxAlive(t *testing.T) {
	// A command that finished within its timeout starts no keep-alive.
	// Arrange
	f, live := newKeepAliveFixture(t, func(context.Context, string, func(string, string)) int { return 0 })

	// Act
	out, err := f.call(context.Background(), "execute_command", `{"command":"true"}`)

	// Assert
	if err != nil || decodeExec(t, out).JobID != "" || live.Load() != 0 {
		t.Fatalf("err %v out %s live %d", err, out, live.Load())
	}
}
