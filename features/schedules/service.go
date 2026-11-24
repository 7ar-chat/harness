// Package schedules fires scheduled agent tasks: it parses their cron expressions (in the schedule's time zone), claims
// the ones that are due from the store, runs each through an Executor under the schedule's timeout, and records the
// outcome. It does not know how an agent run is made (the Executor does) or who may create a schedule (the HTTP layer).
//
// Several processes may tick the same store: the store hands each due schedule to exactly one of them (see
// schedule.Store.ClaimDue), and an optional Locker keeps all but one from even asking.
package schedules

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/7ar-chat/harness/core/cron"
	"github.com/7ar-chat/harness/core/schedule"
)

// Outcome is what an Executor reports about a finished run.
type Outcome struct {
	// Status is one of the schedule.Status constants but running.
	Status string
	Error  string
	// ThreadID and AgentRunID name the conversation and the agent run that did the work.
	ThreadID, AgentRunID string
	Usage                schedule.RunUsage
}

// Executor runs the prompt of a schedule as an ordinary agent run.
type Executor interface {
	// Execute starts the run as s.RunAs and blocks until it ends. It calls started once the conversation exists, so
	// a run in progress can be opened from the history. When ctx ends first (the timeout, a shutdown) Execute must
	// cancel the agent run and return ctx's error.
	Execute(ctx context.Context, s schedule.Schedule, r schedule.Run, started func(threadID, agentRunID string)) (Outcome, error)
}

// Locker is the shared key/value lock of the processes of one deployment (Redis SET NX).
type Locker interface {
	SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error)
}

// Options configures a Service.
type Options struct {
	Store schedule.Store
	Exec  Executor
	// Now is the clock (nil: time.Now).
	Now func() time.Time
	// Every is how often Run looks for due schedules (0: 15s).
	Every time.Duration
	// Locker, when set, lets one process per Every window claim; ID names this process in the lock.
	Locker Locker
	ID     string
	// OnRunFinished is called after a run's outcome is recorded (nil: nothing); the host tells its live views.
	OnRunFinished func(s schedule.Schedule, r schedule.Run)
	// Timeout overrides the time limit of a run (nil: the schedule's TimeoutMinutes). For tests.
	Timeout func(schedule.Schedule) time.Duration
}

// Service is the scheduler.
type Service struct {
	o  Options
	wg sync.WaitGroup

	mu   sync.Mutex
	life context.Context // ends when Run does: the runs it started are cancelled with it
}

// New returns a Service. Store and Exec are required.
func New(o Options) *Service {
	if o.Store == nil || o.Exec == nil {
		panic("schedules: Options.Store and Options.Exec are required")
	}
	if o.Every <= 0 {
		o.Every = 15 * time.Second
	}
	if o.ID == "" {
		o.ID = "schedules"
	}
	return &Service{o: o}
}

func (s *Service) now() time.Time {
	if s.o.Now != nil {
		return s.o.Now().UTC()
	}
	return time.Now().UTC()
}

// Parse validates a schedule expression and time zone.
func Parse(expr, tz string) (cron.Schedule, *time.Location, error) {
	sch, err := cron.ParseSchedule(expr)
	if err != nil {
		return cron.Schedule{}, nil, fmt.Errorf("%w: %w", schedule.ErrInvalid, err)
	}
	loc, err := Location(tz)
	if err != nil {
		return cron.Schedule{}, nil, err
	}
	return sch, loc, nil
}

// Location resolves an IANA zone name ("" is UTC).
func Location(tz string) (*time.Location, error) {
	tz = strings.TrimSpace(tz)
	if tz == "" || strings.EqualFold(tz, "UTC") {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("%w: unknown time zone %q", schedule.ErrInvalid, tz)
	}
	return loc, nil
}

// minInterval is the shortest period a schedule may have: an agent run every few seconds is never what anyone meant.
const minInterval = time.Minute

// Prepare validates the schedule's expression and zone and sets NextRunAt (zero when it is disabled).
func (s *Service) Prepare(sc *schedule.Schedule) error {
	if err := sc.Normalize(); err != nil {
		return err
	}
	cs, loc, err := Parse(sc.Cron, sc.Timezone)
	if err != nil {
		return err
	}
	first := cs.NextIn(s.now(), loc)
	if second := cs.NextIn(first, loc); !first.IsZero() && !second.IsZero() && second.Sub(first) < minInterval {
		return fmt.Errorf("%w: a schedule must not fire more often than once a minute", schedule.ErrInvalid)
	}
	sc.NextRunAt = time.Time{}
	if sc.Enabled {
		sc.NextRunAt = first
	}
	return nil
}

// Preview returns the next n fire times of an expression after from, in UTC.
func Preview(expr, tz string, n int, from time.Time) ([]time.Time, error) {
	cs, loc, err := Parse(expr, tz)
	if err != nil {
		return nil, err
	}
	out := []time.Time{}
	at := from
	for len(out) < n {
		at = cs.NextIn(at, loc)
		if at.IsZero() {
			break
		}
		out = append(out, at)
	}
	return out, nil
}

// next is the store's function for moving a claimed schedule to its next fire time.
func (s *Service) next(sc schedule.Schedule, now time.Time) time.Time {
	cs, loc, err := Parse(sc.Cron, sc.Timezone)
	if err != nil {
		slog.Warn("schedules: a stored schedule does not parse; it will not fire again", "schedule", sc.ID, "error", err)
		return time.Time{}
	}
	return cs.NextIn(now, loc)
}

// Run ticks until ctx ends, then waits for the runs in progress to be recorded.
func (s *Service) Run(ctx context.Context) error {
	s.mu.Lock()
	s.life = ctx
	s.mu.Unlock()
	tk := time.NewTicker(s.o.Every)
	defer tk.Stop()
	defer s.Wait()
	for {
		if _, err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("schedules: tick failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tk.C:
		}
	}
}

// Wait blocks until every run started by this Service has been recorded.
func (s *Service) Wait() { s.wg.Wait() }

// Tick claims the schedules that are due and starts their runs; it returns the claims it started.
func (s *Service) Tick(ctx context.Context) ([]schedule.Claim, error) {
	if s.o.Locker != nil {
		ok, err := s.o.Locker.SetNX(ctx, "work:schedules:tick", s.o.ID, s.o.Every)
		if err != nil {
			return nil, fmt.Errorf("schedules: tick lock: %w", err)
		}
		if !ok {
			return nil, nil // another replica ticks this window
		}
	}
	claims, err := s.o.Store.ClaimDue(ctx, s.now(), s.next)
	if err != nil {
		return nil, fmt.Errorf("schedules: claiming due schedules: %w", err)
	}
	for _, c := range claims {
		s.launch(ctx, c)
	}
	return claims, nil
}

// RunNow starts a manual run of the schedule as by (whoever asked), ErrBusy while one is in progress.
func (s *Service) RunNow(ctx context.Context, id, by string) (*schedule.Run, error) {
	c, err := s.o.Store.StartManual(ctx, id, by, s.now())
	if err != nil {
		return nil, err
	}
	s.launch(ctx, *c)
	return &c.Run, nil
}

func (s *Service) timeout(sc schedule.Schedule) time.Duration {
	if s.o.Timeout != nil {
		return s.o.Timeout(sc)
	}
	return sc.Timeout()
}

// launch executes a claimed run in the background. The run outlives the request that started it (RunNow), so it
// follows the service's lifetime (Run's context), not the request's.
func (s *Service) launch(ctx context.Context, c schedule.Claim) {
	s.mu.Lock()
	life := s.life
	s.mu.Unlock()
	if life == nil {
		life = context.Background()
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.execute(context.WithoutCancel(ctx), life, c)
	}()
}

func (s *Service) execute(base, lifetime context.Context, c schedule.Claim) {
	limit := s.timeout(c.Schedule)
	runCtx, cancel := context.WithTimeout(base, limit)
	defer cancel()
	stop := context.AfterFunc(lifetime, cancel) // a shutdown of the service cancels the runs it started on its tick
	defer stop()
	started := func(threadID, agentRunID string) {
		if err := s.o.Store.SetThread(base, c.Run.ID, threadID, agentRunID); err != nil {
			slog.Warn("schedules: linking a run to its conversation failed", "run", c.Run.ID, "error", err)
		}
	}
	out, err := s.o.Exec.Execute(runCtx, c.Schedule, c.Run, started)
	switch {
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		out.Status, out.Error = schedule.StatusTimeout, fmt.Sprintf("the run was cancelled after %s", limit.Round(time.Second))
	case runCtx.Err() != nil:
		out.Status, out.Error = schedule.StatusCancelled, "the server stopped while the run was in progress"
	case err != nil:
		out.Status, out.Error = schedule.StatusFailed, err.Error()
	}
	if out.Status == "" || out.Status == schedule.StatusRunning {
		out.Status = schedule.StatusSucceeded
	}
	fctx, fcancel := context.WithTimeout(base, 15*time.Second)
	defer fcancel()
	f := schedule.Finish{Status: out.Status, Error: out.Error, ThreadID: out.ThreadID, AgentRun: out.AgentRunID, Usage: out.Usage}
	if err := s.o.Store.FinishRun(fctx, c.Run.ID, f, s.now()); err != nil {
		slog.Error("schedules: recording a run failed", "run", c.Run.ID, "schedule", c.Schedule.ID, "error", err)
	}
	if s.o.OnRunFinished != nil {
		s.o.OnRunFinished(c.Schedule, c.Run)
	}
}
