package schedules_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/store/schedstore"
	"github.com/7ar-chat/harness/core/schedule"
	"github.com/7ar-chat/harness/features/schedules"
)

var ctx = context.Background()

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)} }

// fakeExec records its calls and runs fn for each.
type fakeExec struct {
	mu    sync.Mutex
	calls []schedule.Schedule
	fn    func(ctx context.Context, s schedule.Schedule, started func(string, string)) (schedules.Outcome, error)
}

func (f *fakeExec) Execute(ctx context.Context, s schedule.Schedule, _ schedule.Run, started func(string, string)) (schedules.Outcome, error) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
	if f.fn == nil {
		started("thread-1", "run-1")
		return schedules.Outcome{Status: schedule.StatusSucceeded, ThreadID: "thread-1", AgentRunID: "run-1", Usage: schedule.RunUsage{InputTokens: 10, CostUSD: 0.5}}, nil
	}
	return f.fn(ctx, s, started)
}

func (f *fakeExec) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type rig struct {
	st   *schedstore.Mem
	clk  *clock
	exec *fakeExec
	svc  *schedules.Service
}

func newRig(t *testing.T, o schedules.Options) *rig {
	t.Helper()
	r := &rig{st: schedstore.NewMem(), clk: newClock(), exec: &fakeExec{}}
	o.Store, o.Exec, o.Now = r.st, r.exec, r.clk.Now
	r.svc = schedules.New(o)
	return r
}

func (r *rig) create(t *testing.T, mutate func(*schedule.Schedule)) *schedule.Schedule {
	t.Helper()
	sc := schedule.Schedule{ProjectID: "p1", Owner: "ann", RunAs: "bob", Name: "nightly", Prompt: "summarise", Cron: "*/10 * * * *", Enabled: true}
	if mutate != nil {
		mutate(&sc)
	}
	if err := r.svc.Prepare(&sc); err != nil {
		t.Fatal(err)
	}
	out, err := r.st.Create(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestADueScheduleRunsAsTheAccountThatSavedItAndIsRecorded(t *testing.T) {
	// Arrange
	r := newRig(t, schedules.Options{})
	sc := r.create(t, nil)
	r.clk.Advance(10 * time.Minute)

	// Act
	claims, err := r.svc.Tick(ctx)
	r.svc.Wait()

	// Assert
	if err != nil || len(claims) != 1 {
		t.Fatalf("claims %v, err %v", claims, err)
	}
	if r.exec.count() != 1 || r.exec.calls[0].RunAs != "bob" || r.exec.calls[0].Prompt != "summarise" {
		t.Fatalf("executed: %+v", r.exec.calls)
	}
	runs, _ := r.st.Runs(ctx, sc.ID, 5)
	if len(runs) != 1 || runs[0].Status != schedule.StatusSucceeded || runs[0].ThreadID != "thread-1" || runs[0].Usage.CostUSD != 0.5 || runs[0].StartedBy != "bob" {
		t.Fatalf("history: %+v", runs)
	}
	got, _ := r.st.Get(ctx, sc.ID)
	want := time.Date(2026, 11, 3, 12, 20, 0, 0, time.UTC)
	if !got.NextRunAt.Equal(want) || got.LastStatus != schedule.StatusSucceeded {
		t.Fatalf("schedule: next %v status %q", got.NextRunAt, got.LastStatus)
	}
	if again, _ := r.svc.Tick(ctx); len(again) != 0 {
		t.Fatalf("fired twice for one tick: %+v", again)
	}
}

func TestMissedRunsAreSkippedNotReplayed(t *testing.T) {
	r := newRig(t, schedules.Options{})
	sc := r.create(t, nil)
	r.clk.Advance(5*time.Hour + 3*time.Minute) // the server was down for 30 ticks

	claims, _ := r.svc.Tick(ctx)
	r.svc.Wait()

	if len(claims) != 1 || r.exec.count() != 1 {
		t.Fatalf("claims %d, executions %d", len(claims), r.exec.count())
	}
	got, _ := r.st.Get(ctx, sc.ID)
	if want := time.Date(2026, 11, 3, 17, 10, 0, 0, time.UTC); !got.NextRunAt.Equal(want) {
		t.Fatalf("next = %v, want %v", got.NextRunAt, want)
	}
}

func TestADisabledScheduleNeverFiresButCanRunNow(t *testing.T) {
	r := newRig(t, schedules.Options{})
	sc := r.create(t, func(s *schedule.Schedule) { s.Enabled = false })
	if !sc.NextRunAt.IsZero() {
		t.Fatalf("a disabled schedule has a next run: %v", sc.NextRunAt)
	}
	r.clk.Advance(24 * time.Hour)

	claims, _ := r.svc.Tick(ctx)
	run, err := r.svc.RunNow(ctx, sc.ID, "cy")
	r.svc.Wait()

	if len(claims) != 0 || err != nil || run.Trigger != schedule.TriggerManual || r.exec.count() != 1 {
		t.Fatalf("claims %d, run %+v, err %v, executions %d", len(claims), run, err, r.exec.count())
	}
}

func TestARunThatOutlastsItsTimeoutIsCancelledAndRecorded(t *testing.T) {
	r := newRig(t, schedules.Options{Timeout: func(schedule.Schedule) time.Duration { return 30 * time.Millisecond }})
	sc := r.create(t, nil)
	var sawCancel bool
	r.exec.fn = func(ctx context.Context, _ schedule.Schedule, started func(string, string)) (schedules.Outcome, error) {
		started("thread-9", "run-9")
		<-ctx.Done()
		sawCancel = true
		return schedules.Outcome{ThreadID: "thread-9", AgentRunID: "run-9"}, ctx.Err()
	}
	r.clk.Advance(10 * time.Minute)

	_, _ = r.svc.Tick(ctx)
	r.svc.Wait()

	runs, _ := r.st.Runs(ctx, sc.ID, 5)
	if !sawCancel || len(runs) != 1 || runs[0].Status != schedule.StatusTimeout || runs[0].ThreadID != "thread-9" || runs[0].FinishedAt.IsZero() {
		t.Fatalf("cancel %v, history %+v", sawCancel, runs)
	}
	if got, _ := r.st.Get(ctx, sc.ID); got.LastStatus != schedule.StatusTimeout {
		t.Fatalf("last status %q", got.LastStatus)
	}
}

func TestAnExecutorErrorFailsTheRunAndAnOutcomeStatusPasses(t *testing.T) {
	r := newRig(t, schedules.Options{})
	sc := r.create(t, nil)
	n := 0
	r.exec.fn = func(context.Context, schedule.Schedule, func(string, string)) (schedules.Outcome, error) {
		n++
		if n == 1 {
			return schedules.Outcome{}, errors.New("model unavailable")
		}
		return schedules.Outcome{Status: schedule.StatusSkipped, Error: "bob can no longer write to the project"}, nil
	}

	r.clk.Advance(10 * time.Minute)
	_, _ = r.svc.Tick(ctx)
	r.svc.Wait()
	r.clk.Advance(10 * time.Minute)
	_, _ = r.svc.Tick(ctx)
	r.svc.Wait()

	runs, _ := r.st.Runs(ctx, sc.ID, 5)
	if len(runs) != 2 || runs[1].Status != schedule.StatusFailed || runs[1].Error != "model unavailable" || runs[0].Status != schedule.StatusSkipped {
		t.Fatalf("history: %+v", runs)
	}
}

func TestOnlyOneRunAtATimePerSchedule(t *testing.T) {
	r := newRig(t, schedules.Options{})
	sc := r.create(t, nil)
	release := make(chan struct{})
	r.exec.fn = func(ctx context.Context, _ schedule.Schedule, _ func(string, string)) (schedules.Outcome, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return schedules.Outcome{}, nil
	}
	r.clk.Advance(10 * time.Minute)
	if c, _ := r.svc.Tick(ctx); len(c) != 1 {
		t.Fatalf("first tick: %+v", c)
	}

	r.clk.Advance(10 * time.Minute) // the next tick, the first run is still going
	second, _ := r.svc.Tick(ctx)
	_, busy := r.svc.RunNow(ctx, sc.ID, "ann")

	if len(second) != 0 || !errors.Is(busy, schedule.ErrBusy) {
		t.Fatalf("second claims %+v, run now err %v", second, busy)
	}
	close(release)
	r.svc.Wait()
	runs, _ := r.st.Runs(ctx, sc.ID, 5)
	if len(runs) != 2 || runs[0].Status != schedule.StatusSkipped || r.exec.count() != 1 {
		t.Fatalf("history %+v, executions %d", runs, r.exec.count())
	}
}

// memLocker is a Redis SET NX over one process.
type memLocker struct {
	mu   sync.Mutex
	held map[string]time.Time
	now  func() time.Time
}

func (l *memLocker) SetNX(_ context.Context, key, _ string, ttl time.Duration) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if until, ok := l.held[key]; ok && l.now().Before(until) {
		return false, nil
	}
	l.held[key] = l.now().Add(ttl)
	return true, nil
}

func TestReplicasSharingAStoreRunADueScheduleOnce(t *testing.T) {
	// Arrange: four replicas over one store, half with the shared lock, half relying on the store alone.
	clk := newClock()
	st := schedstore.NewMem()
	lock := &memLocker{held: map[string]time.Time{}, now: clk.Now}
	exec := &fakeExec{}
	var svcs []*schedules.Service
	for i := range 4 {
		o := schedules.Options{Store: st, Exec: exec, Now: clk.Now, Every: time.Minute}
		if i%2 == 0 {
			o.Locker = lock
		}
		svcs = append(svcs, schedules.New(o))
	}
	sc := schedule.Schedule{Owner: "ann", RunAs: "ann", Name: "n", Prompt: "p", Cron: "*/10 * * * *", Enabled: true}
	if err := svcs[0].Prepare(&sc); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(ctx, sc); err != nil {
		t.Fatal(err)
	}
	clk.Advance(10 * time.Minute)

	// Act
	var wg sync.WaitGroup
	for _, s := range svcs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = s.Tick(ctx)
		}()
	}
	wg.Wait()
	for _, s := range svcs {
		s.Wait()
	}

	// Assert
	if exec.count() != 1 {
		t.Fatalf("the schedule ran %d times", exec.count())
	}
}

func TestPrepareValidatesTheScheduleAndTheZone(t *testing.T) {
	r := newRig(t, schedules.Options{})
	base := schedule.Schedule{Owner: "ann", RunAs: "ann", Name: "n", Prompt: "p", Cron: "0 9 * * 1-5", Enabled: true}
	for name, mutate := range map[string]func(*schedule.Schedule){
		"bad cron":      func(s *schedule.Schedule) { s.Cron = "every day" },
		"six fields":    func(s *schedule.Schedule) { s.Cron = "0 0 9 * * *" },
		"bad zone":      func(s *schedule.Schedule) { s.Timezone = "Mars/Olympus" },
		"too frequent":  func(s *schedule.Schedule) { s.Cron = "@every 10s" },
		"never fires":   func(s *schedule.Schedule) { s.Cron = "0 0 31 2 *" },
		"timeout range": func(s *schedule.Schedule) { s.TimeoutMinutes = 500 },
	} {
		s := base
		mutate(&s)
		if err := r.svc.Prepare(&s); !errors.Is(err, schedule.ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	ok := base
	ok.Timezone = "Europe/Paris"
	if err := r.svc.Prepare(&ok); err != nil || ok.NextRunAt.IsZero() || ok.TimeoutMinutes != 30 {
		t.Fatalf("valid schedule: %+v, %v", ok, err)
	}
}

func TestCronFieldsAreReadInTheScheduleTimeZone(t *testing.T) {
	// 09:00 in Paris is 08:00 UTC in November (CET) and 07:00 UTC in July (CEST).
	winter, err := schedules.Preview("0 9 * * *", "Europe/Paris", 2, time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC))
	if err != nil || len(winter) != 2 || !winter[0].Equal(time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("winter: %v %v", winter, err)
	}
	summer, _ := schedules.Preview("0 9 * * *", "Europe/Paris", 1, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
	if !summer[0].Equal(time.Date(2026, 7, 1, 7, 0, 0, 0, time.UTC)) {
		t.Fatalf("summer: %v", summer)
	}
	// Across the end of daylight saving time (25 October 2026) the local hour stays 09:00.
	across, _ := schedules.Preview("0 9 * * *", "Europe/Paris", 3, time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC))
	want := []time.Time{time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC), time.Date(2026, 10, 26, 8, 0, 0, 0, time.UTC), time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)}
	for i := range want {
		if !across[i].Equal(want[i]) {
			t.Fatalf("across DST: %v", across)
		}
	}
	// A half-hour zone.
	india, _ := schedules.Preview("30 8 * * *", "Asia/Kolkata", 1, time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC))
	if !india[0].Equal(time.Date(2026, 11, 3, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("kolkata: %v", india)
	}
	every, _ := schedules.Preview("@every 90m", "Asia/Tokyo", 2, time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC))
	if !every[1].Equal(time.Date(2026, 11, 3, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("every: %v", every)
	}
	if _, err := schedules.Preview("nope", "UTC", 5, time.Now()); err == nil {
		t.Fatal("a bad expression was previewed")
	}
}
