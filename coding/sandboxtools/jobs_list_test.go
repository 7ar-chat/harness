package sandboxtools_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/7ar-chat/harness/coding/sandboxtools"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
)

// fakeJobs is an in-memory sandbox.JobList that records what the provider does to it.
type fakeJobs struct {
	mu        sync.Mutex
	jobs      []sandbox.ListedJob
	listErr   error
	addErr    error
	claimErr  error
	claimOK   map[string]bool // by job id; absent means the claim succeeds
	removed   []string
	released  []string
	claims    map[string]int
	claimDeny atomic.Bool // refuses every claim from now on
}

func (l *fakeJobs) Add(_ context.Context, thread, job string) (time.Time, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.addErr != nil {
		return time.Time{}, l.addErr
	}
	for _, j := range l.jobs {
		if j.ThreadID == thread && j.JobID == job {
			return j.Started, nil
		}
	}
	now := time.Now()
	l.jobs = append(l.jobs, sandbox.ListedJob{ThreadID: thread, JobID: job, Started: now})
	return now, nil
}

func (l *fakeJobs) Remove(_ context.Context, _, job string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.removed = append(l.removed, job)
	return nil
}

func (l *fakeJobs) List(context.Context) ([]sandbox.ListedJob, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]sandbox.ListedJob(nil), l.jobs...), l.listErr
}

func (l *fakeJobs) Claim(_ context.Context, _, job, _ string, _ time.Duration) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.claims == nil {
		l.claims = map[string]int{}
	}
	l.claims[job]++
	if l.claimErr != nil {
		return false, l.claimErr
	}
	if l.claimDeny.Load() {
		return false, nil
	}
	if ok, set := l.claimOK[job]; set {
		return ok, nil
	}
	return true, nil
}

func (l *fakeJobs) Release(_ context.Context, _, job, _ string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.released = append(l.released, job)
	return nil
}

func (l *fakeJobs) claimCount(job string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.claims[job]
}

func (l *fakeJobs) snapshot() (removed, released []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.removed...), append([]string(nil), l.released...)
}

func statusScript(status string) map[string]func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error) {
	return map[string]func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error){
		"# work:job-status": printing("status "+status+"\ncode \nstdout 0 - -\nstderr 0 - -\n", 0)}
}

// resumeProvider is a provider over a list and a manager whose Get gives sb (or get's error).
func resumeProvider(sb sandbox.Sandbox, getErr error, list sandbox.JobList, keepMax time.Duration, keepAlive func(context.Context, sandbox.Sandbox) func()) *sandboxtools.Provider {
	m := stubManager{
		acquire: func(context.Context, string, string) (sandbox.Sandbox, error) { return sb, nil },
		get: func(context.Context, string) (sandbox.Sandbox, error) {
			if getErr != nil {
				return nil, getErr
			}
			return sb, nil
		}}
	return sandboxtools.New(m, sandboxtools.Options{CommandTimeout: time.Second, Jobs: list, JobKeepAliveMax: keepMax, JobPollEvery: 10 * time.Millisecond, KeepAlive: keepAlive})
}

func TestResumeJobsDoesNothingWithoutAJobList(t *testing.T) {
	// Arrange
	p := resumeProvider(newScripted(nil), nil, nil, time.Hour, nil)

	// Act / Assert
	if n := p.ResumeJobs(context.Background()); n != 0 {
		t.Fatalf("resumed %d", n)
	}
}

func TestResumeJobsGivesUpQuietlyWhenTheListIsUnavailable(t *testing.T) {
	// Arrange
	list := &fakeJobs{listErr: errors.New("redis down"), jobs: []sandbox.ListedJob{{ThreadID: "t", JobID: "job_aaaaaaaaaaaa", Started: time.Now()}}}
	p := resumeProvider(newScripted(statusScript("running")), nil, list, time.Hour, nil)

	// Act
	n := p.ResumeJobs(context.Background())

	// Assert
	if removed, _ := list.snapshot(); n != 0 || len(removed) != 0 {
		t.Fatalf("resumed %d, removed %v", n, removed)
	}
}

func TestResumeJobsDecidesPerJobWhatToKeepAndWhatToForget(t *testing.T) {
	cases := map[string]struct {
		job      sandbox.ListedJob
		getErr   error
		status   string
		claimOK  *bool
		claimErr error
		resumed  int
		removed  bool
		released bool
		claimed  bool
	}{
		"running job is resumed":             {job: job(time.Minute), status: "running", resumed: 1, claimed: true},
		"budget spent is dropped unclaimed":  {job: job(2 * time.Hour), status: "running", removed: true},
		"lease held elsewhere is left alone": {job: job(time.Minute), status: "running", claimOK: ptr(false), claimed: true},
		"claim error is left for later":      {job: job(time.Minute), status: "running", claimErr: errors.New("redis down"), claimed: true},
		"sandbox gone drops the job":         {job: job(time.Minute), getErr: sandbox.ErrNoSandbox, removed: true, released: true, claimed: true},
		"sandbox unavailable retries later":  {job: job(time.Minute), getErr: errors.New("timeout"), released: true, claimed: true},
		"finished job is dropped":            {job: job(time.Minute), status: "finished", removed: true, released: true, claimed: true},
		"missing job is dropped":             {job: job(time.Minute), status: "missing", removed: true, released: true, claimed: true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			// Arrange
			list := &fakeJobs{jobs: []sandbox.ListedJob{c.job}, claimErr: c.claimErr}
			if c.claimOK != nil {
				list.claimOK = map[string]bool{c.job.JobID: *c.claimOK}
			}
			p := resumeProvider(newScripted(statusScript(c.status)), c.getErr, list, time.Hour, func(context.Context, sandbox.Sandbox) func() { return func() {} })

			// Act
			n := p.ResumeJobs(context.Background())

			// Assert
			removed, released := list.snapshot()
			if n != c.resumed || (len(removed) > 0) != c.removed || (len(released) > 0) != c.released || (list.claimCount(c.job.JobID) > 0) != c.claimed {
				t.Fatalf("resumed %d removed %v released %v claims %d", n, removed, released, list.claimCount(c.job.JobID))
			}
		})
	}
}

func job(age time.Duration) sandbox.ListedJob {
	return sandbox.ListedJob{ThreadID: "t", JobID: "job_aaaaaaaaaaaa", Started: time.Now().Add(-age)}
}

func ptr[T any](v T) *T { return &v }

func TestResumeJobsKeepsAJobAliveOnceWhateverHowOftenItRuns(t *testing.T) {
	// Arrange
	var live atomic.Int32
	list := &fakeJobs{jobs: []sandbox.ListedJob{job(time.Minute)}}
	p := resumeProvider(newScripted(statusScript("running")), nil, list, time.Hour, func(context.Context, sandbox.Sandbox) func() {
		live.Add(1)
		return func() { live.Add(-1) }
	})

	// Act
	first := p.ResumeJobs(context.Background())
	second := p.ResumeJobs(context.Background())

	// Assert
	if first != 1 || second != 0 || live.Load() != 1 {
		t.Fatalf("resumed %d then %d, %d keep-alives", first, second, live.Load())
	}
}

func TestAResumedJobStopsBeingKeptAliveWhenItEndsAndIsForgotten(t *testing.T) {
	// Arrange
	var status atomic.Value
	status.Store("running")
	sb := newScripted(map[string]func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error){
		"# work:job-status": func(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
			return printing("status "+status.Load().(string)+"\ncode \nstdout 0 - -\nstderr 0 - -\n", 0)(ctx, req)
		}})
	var live atomic.Int32
	list := &fakeJobs{jobs: []sandbox.ListedJob{job(time.Minute)}}
	p := resumeProvider(sb, nil, list, time.Hour, func(context.Context, sandbox.Sandbox) func() {
		live.Add(1)
		return func() { live.Add(-1) }
	})
	p.ResumeJobs(context.Background())

	// Act
	status.Store("finished")

	// Assert
	eventually(t, "the keep-alive to end", func() bool { return live.Load() == 0 })
	eventually(t, "the job to be forgotten", func() bool { removed, released := list.snapshot(); return len(removed) > 0 && len(released) > 0 })
}

func TestAJobWhoseLeaseIsLostStaysListedForItsNewOwner(t *testing.T) {
	// Arrange
	var live atomic.Int32
	list := &fakeJobs{jobs: []sandbox.ListedJob{job(time.Minute)}}
	p := resumeProvider(newScripted(statusScript("running")), nil, list, time.Hour, func(context.Context, sandbox.Sandbox) func() {
		live.Add(1)
		return func() { live.Add(-1) }
	})
	p.ResumeJobs(context.Background())

	// Act: another process takes the lease over.
	list.claimDeny.Store(true)

	// Assert: this process stops renewing but does not forget (Remove) or release the job.
	eventually(t, "the keep-alive to end", func() bool { return live.Load() == 0 })
	if removed, released := list.snapshot(); len(removed) != 0 || len(released) != 0 {
		t.Fatalf("removed %v released %v", removed, released)
	}
}

func TestAKeepAliveEndsWhenItsSandboxIsGone(t *testing.T) {
	// Arrange
	var gone atomic.Bool
	sb := newScripted(map[string]func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error){
		"# work:job-status": func(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
			if gone.Load() {
				return sandbox.ExecResult{}, sandbox.ErrGone
			}
			return printing("status running\ncode \nstdout 0 - -\nstderr 0 - -\n", 0)(ctx, req)
		}})
	var live atomic.Int32
	list := &fakeJobs{jobs: []sandbox.ListedJob{job(time.Minute)}}
	p := resumeProvider(sb, nil, list, time.Hour, func(context.Context, sandbox.Sandbox) func() {
		live.Add(1)
		return func() { live.Add(-1) }
	})
	p.ResumeJobs(context.Background())

	// Act
	gone.Store(true)

	// Assert
	eventually(t, "the keep-alive to end", func() bool { return live.Load() == 0 })
}

func TestAStatusErrorThatIsNotGoneKeepsTheJobAlive(t *testing.T) {
	// Arrange: the status check fails transiently from the second poll on.
	var polls atomic.Int32
	sb := newScripted(map[string]func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error){
		"# work:job-status": func(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
			if polls.Add(1) > 1 {
				return sandbox.ExecResult{}, errors.New("connection reset")
			}
			return printing("status running\ncode \nstdout 0 - -\nstderr 0 - -\n", 0)(ctx, req)
		}})
	var live atomic.Int32
	list := &fakeJobs{jobs: []sandbox.ListedJob{job(time.Minute)}}
	p := resumeProvider(sb, nil, list, time.Hour, func(context.Context, sandbox.Sandbox) func() {
		live.Add(1)
		return func() { live.Add(-1) }
	})
	p.ResumeJobs(context.Background())

	// Act
	eventually(t, "several failing polls", func() bool { return polls.Load() >= 4 })

	// Assert
	if live.Load() != 1 {
		t.Fatalf("keep-alives %d", live.Load())
	}
}

func TestStartingABackgroundJobSurvivesAFailingJobList(t *testing.T) {
	cases := map[string]*fakeJobs{
		"add fails":               {addErr: errors.New("redis down")},
		"another process owns it": {claimOK: map[string]bool{}},
	}
	for name, list := range cases {
		t.Run(name, func(t *testing.T) {
			// Arrange
			var live atomic.Int32
			f := newFixtureWith(t, func(sb *sandboxtest.Sandbox) { sb.SetJobs(blockingJob(make(chan struct{}))) }, func(o *sandboxtools.Options, _ *sandbox.ManagerOptions) {
				o.Jobs = list
				o.JobPollEvery = 10 * time.Millisecond
				o.KeepAlive = func(context.Context, sandbox.Sandbox) func() { live.Add(1); return func() { live.Add(-1) } }
			})

			if name == "another process owns it" {
				list.claimDeny.Store(true)
			}

			// Act
			out, err := f.call(context.Background(), "execute_command", `{"command":"x","background":true}`)

			// Assert: the job runs; it is kept alive here only when nobody else owns it.
			want := int32(1)
			if name == "another process owns it" {
				want = 0
			}
			if err != nil || decodeExec(t, out).Status != "running" || live.Load() != want {
				t.Fatalf("err %v out %s keep-alives %d", err, out, live.Load())
			}
		})
	}
}

func TestKeepResumingJobsRunsNowAndThenPeriodicallyUntilCancelled(t *testing.T) {
	// Arrange
	list := &fakeJobs{}
	p := resumeProvider(newScripted(nil), nil, list, time.Hour, nil)
	ctx, cancel := context.WithCancel(context.Background())
	rounds := make(chan int, 16)
	finished := make(chan struct{})

	// Act
	go func() {
		p.KeepResumingJobs(ctx, 5*time.Millisecond, func(n int) { rounds <- n })
		close(finished)
	}()
	for i := 0; i < 3; i++ {
		<-rounds
	}
	cancel()

	// Assert
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("KeepResumingJobs did not return after cancellation")
	}
}

func TestKeepResumingJobsTakesOverAJobWhoseOwnerDied(t *testing.T) {
	// Arrange: the lease is held by a dead process at the first round, free at the next.
	var live atomic.Int32
	list := &fakeJobs{jobs: []sandbox.ListedJob{job(time.Minute)}, claimOK: map[string]bool{"job_aaaaaaaaaaaa": false}}
	p := resumeProvider(newScripted(statusScript("running")), nil, list, time.Hour, func(context.Context, sandbox.Sandbox) func() {
		live.Add(1)
		return func() { live.Add(-1) }
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan int, 64)
	go p.KeepResumingJobs(ctx, 5*time.Millisecond, func(n int) { results <- n })
	if n := <-results; n != 0 || live.Load() != 0 {
		t.Fatalf("first round resumed %d", n)
	}

	// Act: the old owner's lease expires.
	list.mu.Lock()
	list.claimOK["job_aaaaaaaaaaaa"] = true
	list.mu.Unlock()

	// Assert
	eventually(t, "the job to be taken over", func() bool { return live.Load() == 1 })
}
