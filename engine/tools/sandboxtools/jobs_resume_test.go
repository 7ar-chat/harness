package sandboxtools_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/cloudwego/eino/components/tool"
	"github.com/redis/go-redis/v9"

	"github.com/GhaziBenDahmane/harness/engine/tools"
	"github.com/GhaziBenDahmane/harness/engine/tools/sandboxtools"
	"github.com/GhaziBenDahmane/harness/redisx"
	"github.com/GhaziBenDahmane/harness/sandbox"
	"github.com/GhaziBenDahmane/harness/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/store/memstore"
)

func TestARestartedRuntimeResumesTheKeepAliveOfRunningJobs(t *testing.T) {
	// A background job started by a runtime process that then restarted keeps its sandbox alive: the new process resumes the keep-alives of the persisted running jobs, until job_kill ends it.
	// Arrange
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	st := memstore.New()
	th, _ := st.CreateThread(context.Background(), "alice", "t")
	backend := sandboxtest.NewBackend()
	var sb *sandboxtest.Sandbox
	backend.OnCreate = func(s *sandboxtest.Sandbox) { sb = s; s.SetJobs(blockingJob(make(chan struct{}))) }
	process := func(rdb *redis.Client) (map[string]tool.InvokableTool, *sandboxtools.Provider, *atomic.Int32) {
		mgr := sandbox.NewManager(backend, st, redisx.NewCoordinator(rdb), sandbox.ManagerOptions{MaxPerUser: 5, Timeout: time.Hour, RenewEvery: 5 * time.Millisecond})
		var live atomic.Int32
		p := sandboxtools.New(mgr, sandboxtools.Options{CommandTimeout: time.Second, JobPollEvery: 10 * time.Millisecond, Jobs: redisx.NewJobs(rdb),
			KeepAlive: func(ctx context.Context, sb sandbox.Sandbox) func() {
				live.Add(1)
				stop := sandbox.KeepAlive(ctx, sb)
				return func() { stop(); live.Add(-1) }
			}})
		ts, _, err := p.Tools(context.Background(), tools.RunContext{ThreadID: th.ID, RunID: "r", Login: "alice"})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]tool.InvokableTool{}
		for _, x := range ts {
			info, _ := x.Info(context.Background())
			out[info.Name] = x.(tool.InvokableTool)
		}
		return out, p, &live
	}
	dead := redis.NewClient(&redis.Options{Addr: mr.Addr()}) // the first process's connection
	first, _, _ := process(dead)
	out, err := first["execute_command"].InvokableRun(context.Background(), `{"command":"python3 server.py","background":true}`)
	if err != nil {
		t.Fatal(err)
	}
	job := decodeExec(t, out).JobID
	_ = dead.Close()          // the first process is gone...
	mr.FastForward(time.Hour) // ...and so is its lease on the job
	restarted, p2, live := process(rdb)

	// Act
	resumed := p2.ResumeJobs(context.Background())

	// Assert
	before := sb.Renewals()
	eventually(t, "renewals by the restarted process", func() bool { return live.Load() == 1 && sb.Renewals() >= before+3 })
	if resumed != 1 {
		t.Fatalf("resumed %d", resumed)
	}
	if _, err := restarted["job_kill"].InvokableRun(context.Background(), `{"jobId":"`+job+`"}`); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the keep-alive to end with job_kill", func() bool { return live.Load() == 0 })
	if n, _ := rdb.HLen(context.Background(), "work:sbx:jobs").Result(); n != 0 {
		t.Fatalf("%d jobs still listed", n)
	}
}

// jobRuntime is one runtime process of the job tests: its tools, its provider
// and the keep-alives it holds.
type jobRuntime struct {
	tools map[string]tool.InvokableTool
	p     *sandboxtools.Provider
	live  *atomic.Int32
}

func newJobRuntime(t *testing.T, backend *sandboxtest.Backend, st *memstore.Store, rdb *redis.Client, threadID string, keepMax time.Duration) jobRuntime {
	t.Helper()
	mgr := sandbox.NewManager(backend, st, redisx.NewCoordinator(rdb), sandbox.ManagerOptions{MaxPerUser: 5, Timeout: time.Hour, RenewEvery: 5 * time.Millisecond})
	live := &atomic.Int32{}
	p := sandboxtools.New(mgr, sandboxtools.Options{CommandTimeout: time.Second, JobPollEvery: 10 * time.Millisecond, JobKeepAliveMax: keepMax,
		Jobs: redisx.NewJobs(rdb),
		KeepAlive: func(ctx context.Context, sb sandbox.Sandbox) func() {
			live.Add(1)
			stop := sandbox.KeepAlive(ctx, sb)
			return func() { stop(); live.Add(-1) }
		}})
	ts, _, err := p.Tools(context.Background(), tools.RunContext{ThreadID: threadID, RunID: "r", Login: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]tool.InvokableTool{}
	for _, x := range ts {
		info, _ := x.Info(context.Background())
		out[info.Name] = x.(tool.InvokableTool)
	}
	return jobRuntime{tools: out, p: p, live: live}
}

func TestARestartedRuntimeKeepsAJobAliveOnlyForWhatIsLeftOfItsBudget(t *testing.T) {
	// A process start resumes a running job's keep-alive with the rest of its JobKeepAliveMax counted from the job's start, not a fresh budget (N4).
	// Arrange
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	st := memstore.New()
	th, _ := st.CreateThread(context.Background(), "alice", "t")
	backend := sandboxtest.NewBackend()
	backend.OnCreate = func(s *sandboxtest.Sandbox) { s.SetJobs(blockingJob(make(chan struct{}))) }
	dead := redis.NewClient(&redis.Options{Addr: mr.Addr()}) // the first process's connection
	first := newJobRuntime(t, backend, st, dead, th.ID, 700*time.Millisecond)
	if _, err := first.tools["execute_command"].InvokableRun(context.Background(), `{"command":"python3 server.py","background":true}`); err != nil {
		t.Fatal(err)
	}
	time.Sleep(450 * time.Millisecond)
	_ = dead.Close()          // the first process died...
	mr.FastForward(time.Hour) // ...and its lease expired
	restarted := newJobRuntime(t, backend, st, rdb, th.ID, 700*time.Millisecond)

	// Act
	resumed := restarted.p.ResumeJobs(context.Background())

	// Assert
	at := time.Now()
	eventually(t, "the resumed keep-alive to end", func() bool { return restarted.live.Load() == 0 })
	if resumed != 1 || time.Since(at) > 500*time.Millisecond {
		t.Fatalf("resumed %d, kept alive %v after the restart", resumed, time.Since(at))
	}
}

func TestOnlyOneRuntimeResumesTheKeepAliveOfAJob(t *testing.T) {
	// Every replica resumes the listed jobs at its start, but a job's keep-alive has one owner (a Redis lease): two processes starting together keep it alive once (N4).
	// Arrange
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	st := memstore.New()
	th, _ := st.CreateThread(context.Background(), "alice", "t")
	backend := sandboxtest.NewBackend()
	backend.OnCreate = func(s *sandboxtest.Sandbox) { s.SetJobs(blockingJob(make(chan struct{}))) }
	dead := redis.NewClient(&redis.Options{Addr: mr.Addr()}) // the first process's connection
	first := newJobRuntime(t, backend, st, dead, th.ID, time.Hour)
	if _, err := first.tools["execute_command"].InvokableRun(context.Background(), `{"command":"python3 server.py","background":true}`); err != nil {
		t.Fatal(err)
	}
	_ = dead.Close()          // the first process died...
	mr.FastForward(time.Hour) // ...and its lease expired
	a := newJobRuntime(t, backend, st, rdb, th.ID, time.Hour)
	b := newJobRuntime(t, backend, st, rdb, th.ID, time.Hour)

	// Act
	resumed := a.p.ResumeJobs(context.Background()) + b.p.ResumeJobs(context.Background())

	// Assert
	time.Sleep(50 * time.Millisecond)
	if resumed != 1 || a.live.Load()+b.live.Load() != 1 {
		t.Fatalf("resumed %d, keep-alives %d + %d", resumed, a.live.Load(), b.live.Load())
	}
}
