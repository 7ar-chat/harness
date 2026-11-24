// Package schedule is the domain of scheduled agent tasks: a prompt an agent runs on a cron or interval schedule in a
// project (or for one person), the history of its runs, and the store port. The scheduler that fires them is
// features/schedules; the HTTP routes are in app/product.
package schedule

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrNotFound means there is no such schedule or run.
	ErrNotFound = errors.New("schedule: not found")
	// ErrInvalid wraps a refused value.
	ErrInvalid = errors.New("schedule: invalid")
	// ErrBusy means the schedule already has a run in progress.
	ErrBusy = errors.New("schedule: a run is already in progress")
)

// Policies of a scheduled run (Schedule.Policy).
const (
	// PolicyDefault runs unattended with the harness's normal permissions.
	PolicyDefault = "default"
	// PolicyReadOnly runs in plan mode: the agent reads and reports, it changes nothing.
	PolicyReadOnly = "readonly"
)

// Run statuses (Run.Status).
const (
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusTimeout   = "timeout"
	StatusCancelled = "cancelled"
	// StatusSkipped marks a fire that was not started: the previous run was still going, or the account may no longer write.
	StatusSkipped = "skipped"
)

// Triggers of a run (Run.Trigger).
const (
	TriggerSchedule = "schedule"
	TriggerManual   = "manual"
)

// Limits of Schedule.TimeoutMinutes.
const (
	MinTimeoutMinutes     = 1
	MaxTimeoutMinutes     = 240
	DefaultTimeoutMinutes = 30
)

// DeadlineGrace is how long after its timeout a run still marked running is declared lost.
const DeadlineGrace = 5 * time.Minute

// Schedule is an agent task that runs by itself.
type Schedule struct {
	ID string `json:"id"`
	// ProjectID is the project the runs belong to; empty for a personal schedule of Owner.
	ProjectID string `json:"projectId,omitempty"`
	// Owner is who created the schedule.
	Owner string `json:"owner"`
	// RunAs is the account the runs execute as: whoever saved the schedule last. The server sets it; a client cannot.
	RunAs string `json:"runAs"`
	Name  string `json:"name"`
	// Prompt is the message the agent receives.
	Prompt string `json:"prompt"`
	// Cron is a five-field cron expression, an "@every <duration>" interval or a macro (@daily...).
	Cron string `json:"cron"`
	// Timezone is the IANA zone the cron fields are read in ("" or "UTC": UTC).
	Timezone string `json:"timezone"`
	// Model is the model of the runs ("": the server's default).
	Model string `json:"model,omitempty"`
	// Policy is PolicyDefault ("" too) or PolicyReadOnly.
	Policy string `json:"policy,omitempty"`
	// TimeoutMinutes cancels a run that is still going after that long (1 to 240, default 30).
	TimeoutMinutes int `json:"timeoutMinutes"`
	// ReuseSession runs every fire in the same conversation instead of a fresh one.
	ReuseSession bool `json:"reuseSession,omitempty"`
	// ThreadID is the conversation of the last run when ReuseSession is on.
	ThreadID string `json:"threadId,omitempty"`
	Enabled  bool   `json:"enabled"`
	// NextRunAt is the next fire time (zero when disabled or the schedule never fires again); LastRunAt the last fire.
	NextRunAt time.Time `json:"nextRunAt,omitzero"`
	LastRunAt time.Time `json:"lastRunAt,omitzero"`
	// LastStatus is the status of the last run ("" before the first).
	LastStatus string    `json:"lastStatus,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// Timeout is the run's time limit.
func (s Schedule) Timeout() time.Duration {
	m := s.TimeoutMinutes
	if m <= 0 {
		m = DefaultTimeoutMinutes
	}
	return time.Duration(m) * time.Minute
}

// RunUsage is what a run consumed.
type RunUsage struct {
	InputTokens      int64   `json:"inputTokens"`
	OutputTokens     int64   `json:"outputTokens"`
	CacheReadTokens  int64   `json:"cacheReadTokens"`
	CacheWriteTokens int64   `json:"cacheWriteTokens"`
	CostUSD          float64 `json:"costUsd"`
	// Unpriced is how many of the run's models have no price: CostUSD is then a lower bound.
	Unpriced int `json:"unpriced,omitempty"`
}

// Run is one execution of a schedule.
type Run struct {
	ID         string `json:"id"`
	ScheduleID string `json:"scheduleId"`
	// FireAt is the scheduled time the run is for (the start time for a manual run). A schedule has at most one run
	// per FireAt: several replicas cannot fire the same tick twice.
	FireAt   time.Time `json:"fireAt"`
	Trigger  string    `json:"trigger"`
	Status   string    `json:"status"`
	ThreadID string    `json:"threadId,omitempty"`
	AgentRun string    `json:"agentRunId,omitempty"`
	Error    string    `json:"error,omitempty"`
	// StartedBy is who started the run: the schedule's RunAs for a scheduled run, the person who pressed "Run now" for a manual one. Either way the run executes as the schedule's RunAs.
	StartedBy  string    `json:"startedBy"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
	// Deadline is when a run still marked running is considered lost (the schedule's timeout plus a grace).
	Deadline time.Time `json:"deadline"`
	Usage    RunUsage  `json:"usage"`
}

// Claim is a schedule that was due and the run created for it.
type Claim struct {
	Schedule Schedule
	Run      Run
}

// Finish is the outcome recorded on a run.
type Finish struct {
	Status   string
	Error    string
	ThreadID string
	AgentRun string
	Usage    RunUsage
}

// Store persists schedules and their runs. Every method is safe for concurrent use, and ClaimDue is atomic across
// processes sharing the store.
type Store interface {
	// Create stores s (a fresh id when empty) and returns it.
	Create(ctx context.Context, s Schedule) (*Schedule, error)
	Get(ctx context.Context, id string) (*Schedule, error)
	// Update replaces the editable fields of the schedule, keeping its id, owner, creation time and history.
	Update(ctx context.Context, s Schedule) (*Schedule, error)
	// Delete removes the schedule and its runs.
	Delete(ctx context.Context, id string) error
	// List returns the schedules of a project, or the personal schedules of owner when projectID is empty.
	List(ctx context.Context, projectID, owner string) ([]Schedule, error)

	// ClaimDue starts a run for every enabled schedule whose NextRunAt is at or before now: it records the run as
	// running, sets LastRunAt, and moves NextRunAt to next(schedule, now) (a missed fire is skipped, never replayed).
	// A schedule that still has a run in progress gets a skipped run instead. Runs that outlived their Deadline are
	// first marked failed. Callers on several replicas never receive the same claim.
	ClaimDue(ctx context.Context, now time.Time, next func(Schedule, time.Time) time.Time) ([]Claim, error)
	// StartManual records a manual run of the schedule, ErrBusy while another run is in progress.
	StartManual(ctx context.Context, id, by string, now time.Time) (*Claim, error)
	// FinishRun records the outcome of a run, and the last status and conversation on its schedule.
	FinishRun(ctx context.Context, runID string, f Finish, now time.Time) error
	// SetThread records the conversation a run executes in as soon as it exists, so a live run is linked to its session.
	SetThread(ctx context.Context, runID, threadID, agentRunID string) error
	// ClearThread forgets threadID as the reused conversation of any schedule (the conversation was deleted): the
	// next fire starts a fresh one. The runs keep their history.
	ClearThread(ctx context.Context, threadID string) error
	// Runs returns the newest runs of a schedule first.
	Runs(ctx context.Context, scheduleID string, limit int) ([]Run, error)
	// RunOfThread returns the newest run that executed in the thread, ErrNotFound when none did.
	RunOfThread(ctx context.Context, threadID string) (*Run, error)
}

// ValidStatus reports whether s is one of the Status constants.
func ValidStatus(s string) bool {
	switch s {
	case StatusRunning, StatusSucceeded, StatusFailed, StatusTimeout, StatusCancelled, StatusSkipped:
		return true
	}
	return false
}

// Normalize applies the defaults and checks the fields every store requires; whether Cron parses and Timezone exists is
// the scheduler's to check (it owns the cron library).
func (s *Schedule) Normalize() error {
	s.Name, s.Prompt, s.Cron = strings.TrimSpace(s.Name), strings.TrimSpace(s.Prompt), strings.TrimSpace(s.Cron)
	switch {
	case s.Name == "":
		return fmt.Errorf("%w: name is required", ErrInvalid)
	case len(s.Name) > 200:
		return fmt.Errorf("%w: name is at most 200 characters", ErrInvalid)
	case s.Prompt == "":
		return fmt.Errorf("%w: prompt is required", ErrInvalid)
	case len(s.Prompt) > 100_000:
		return fmt.Errorf("%w: prompt is at most 100000 characters", ErrInvalid)
	case s.Cron == "":
		return fmt.Errorf("%w: schedule is required", ErrInvalid)
	case s.Owner == "" || s.RunAs == "":
		return fmt.Errorf("%w: owner and runAs are required", ErrInvalid)
	}
	if strings.TrimSpace(s.Timezone) == "" {
		s.Timezone = "UTC"
	}
	switch s.Policy {
	case "":
		s.Policy = PolicyDefault
	case PolicyDefault, PolicyReadOnly:
	default:
		return fmt.Errorf("%w: policy must be %q or %q", ErrInvalid, PolicyDefault, PolicyReadOnly)
	}
	if s.TimeoutMinutes == 0 {
		s.TimeoutMinutes = DefaultTimeoutMinutes
	}
	if s.TimeoutMinutes < MinTimeoutMinutes || s.TimeoutMinutes > MaxTimeoutMinutes {
		return fmt.Errorf("%w: timeoutMinutes must be between %d and %d", ErrInvalid, MinTimeoutMinutes, MaxTimeoutMinutes)
	}
	return nil
}
