package tasks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/runtime"
	"github.com/7ar-chat/harness/engine/tools"
)

// waitingPrefix starts the reason of a task blocked on a human answer: the run
// that continues after the answer finds the task by it.
const waitingPrefix = "waiting for "

// noPRFeedback is what a run that ended without a pull request is told.
const noPRFeedback = "no pull request was opened: commit your work and call open_pr, which publishes your branch"

// Hook returns the runtime hook that moves a task on when its run ends.
func (s *Service) Hook() runtime.Hook { return endHook{s} }

type endHook struct{ s *Service }

// Finishing implements runtime.Hook. The work happens in the continuation,
// after the run's status is recorded and its messages are persisted.
func (h endHook) Finishing(_ context.Context, f *runtime.Finish) func(context.Context) {
	if f == nil || f.Run == nil {
		return nil
	}
	run, status, intr, errText := f.Run, f.Status, f.Result.Interrupt, f.Run.Error
	return func(ctx context.Context) {
		if err := h.s.runEnded(ctx, run.ThreadID, run.ID, status, intr, errText); err != nil {
			slog.Warn("tasks: handling the end of a run failed", "run_id", run.ID, "thread_id", run.ThreadID, "error", err)
		}
	}
}

// taskOfThread finds the task a thread works on: by the thread recorded on the
// task, else by the thread's info (the task records its thread only after the
// spawner returned, which a fast run can beat).
func (s *Service) taskOfThread(ctx context.Context, threadID string) (*factory.Task, error) {
	if threadID == "" {
		return nil, nil
	}
	ts, err := s.st.ListTasks(ctx, factory.Filter{ThreadID: threadID, Limit: 1})
	if err != nil {
		return nil, fmt.Errorf("tasks: finding task of thread %s: %w", threadID, err)
	}
	if len(ts) > 0 {
		return &ts[0], nil
	}
	ti, err := s.st.GetThreadInfo(ctx, threadID)
	if errors.Is(err, factory.ErrNotFound) || err == nil && ti.TaskID == "" {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("tasks: thread info of %s: %w", threadID, err)
	}
	t, err := s.st.GetTask(ctx, ti.TaskID)
	if errors.Is(err, factory.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("tasks: loading task %s: %w", ti.TaskID, err)
	}
	return t, nil
}

// runEnded maps a finished run to what happens to its task.
func (s *Service) runEnded(ctx context.Context, threadID, runID, status string, intr *tools.Interrupt, errText string) error {
	t, err := s.taskOfThread(ctx, threadID)
	if err != nil || t == nil {
		return err
	}
	switch {
	case t.State == factory.StateBlocked && strings.HasPrefix(t.BlockedReason, waitingPrefix) && t.RunID != runID:
		// The human answered and the run continued on the same thread.
		if t, err = s.move(ctx, t, factory.StateRunning, factory.Patch{RunID: &runID, BlockedReason: new(string)}, "running: answered"); err != nil {
			return err
		}
	case t.State != factory.StateRunning:
		return nil // Review, a task moved on by someone else, a cancelled task
	case t.RunID != "" && t.RunID != runID:
		return nil // a run of an earlier attempt
	}

	switch status {
	case store.StatusCompleted:
		return s.completedRun(ctx, t, intr)
	case store.StatusCancelled:
		return s.fail(ctx, t, factory.StateCancelled, "the run was cancelled")
	case store.StatusFailed, store.StatusInterrupted:
		if errText == "" && s.opts.RunError != nil {
			// The hook runs before the run row carries its error text.
			var err error
			if errText, err = s.opts.RunError(ctx, runID); err != nil {
				slog.Warn("tasks: reading the run's error failed", "run_id", runID, "error", err)
			}
		}
		return s.failedAttempt(ctx, t, firstNonEmpty(errText, "the run "+status))
	}
	return nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (s *Service) completedRun(ctx context.Context, t *factory.Task, intr *tools.Interrupt) error {
	switch {
	case intr != nil:
		reason := waitingPrefix + "an answer"
		switch {
		case intr.Approval != nil:
			reason = waitingPrefix + "approval"
		case intr.Question != "":
			reason += ": " + clip(intr.Question, 200)
		}
		return s.block(ctx, t, reason, factory.Patch{})
	case t.Repo == "":
		var answer string
		if s.opts.Messages != nil {
			var err error
			if answer, err = s.opts.Messages(ctx, t.ThreadID); err != nil {
				slog.Warn("tasks: reading the run's answer failed", "task", t.ID, "error", err)
			}
		}
		return s.complete(ctx, t, answer)
	case t.PR.Number == 0 && t.PR.URL == "":
		return s.resume(ctx, t, noPRFeedback, 0)
	default:
		// A fix pushed to the open pull request: CI and the reviewer judge it again.
		return s.Review(ctx, t.ID, t.PR)
	}
}

func (s *Service) complete(ctx context.Context, t *factory.Task, result string) error {
	empty := ""
	u, err := s.move(ctx, t, factory.StateDone, factory.Patch{Result: &result, Error: &empty, BlockedReason: &empty}, "done")
	if err != nil {
		return err
	}
	s.afterTerminal(ctx, *u)
	return nil
}

// failedAttempt records why the attempt failed, then retries after a backoff,
// or blocks the task when the attempts are used up or it keeps failing the
// same way.
func (s *Service) failedAttempt(ctx context.Context, t *factory.Task, errText string) error {
	fp := factory.Fingerprint(errText)
	u, err := s.st.UpdateTask(ctx, t.ID, factory.Patch{
		IfState: []factory.State{factory.StateRunning}, AddFingerprint: fp, Error: &errText,
	})
	if err != nil {
		return fmt.Errorf("tasks: recording failure of %s: %w", t.ID, err)
	}
	s.event(ctx, t.ID, factory.EventRun, fmt.Sprintf("attempt %d failed: %s", u.Attempts, clip(firstLine(errText), 200)),
		map[string]any{"attempt": u.Attempts, "fingerprint": fp, "error": clip(errText, 2000)})
	if factory.Repeating(u.Fingerprints, repeatLimit) {
		reason := fmt.Sprintf("failed the same way %d times: %s", repeatLimit, clip(firstLine(errText), 200))
		return s.block(ctx, u, reason, factory.Patch{Feedback: &errText})
	}
	return s.resume(ctx, u, "The previous attempt failed: "+errText, s.opts.Backoff(u.Attempts))
}

// Job is the periodic sweep for the runtime's job runner.
func (s *Service) Job() runtime.Job {
	return runtime.Job{Name: "factory-tasks-sweep", Every: time.Minute, Run: s.Sweep}
}

// Sweep recovers what the end-of-run hook missed (a process that died with a
// run in flight) and releases Pending tasks whose dependencies finished.
func (s *Service) Sweep(ctx context.Context) error {
	var errs []error
	if err := s.releasePending(ctx); err != nil {
		errs = append(errs, err)
	}
	running, err := s.st.ListTasks(ctx, factory.Filter{States: []factory.State{factory.StateRunning}, Limit: listLimit})
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("tasks: listing running tasks: %w", err))...)
	}
	now := s.opts.Now()
	for i := range running {
		t := &running[i]
		if err := s.recover(ctx, t, now); err != nil && !errors.Is(err, factory.ErrConflict) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// recover settles a Running task whose run ended without the hook noticing.
func (s *Service) recover(ctx context.Context, t *factory.Task, now time.Time) error {
	if t.RunID == "" {
		if now.Sub(t.UpdatedAt) < s.opts.RecoverAfter {
			return nil // the dispatcher is still spawning it
		}
		return s.failedAttempt(ctx, t, "the run was never started")
	}
	if s.opts.RunStatus == nil {
		return nil
	}
	status, err := s.opts.RunStatus(ctx, t.RunID)
	if err != nil {
		return fmt.Errorf("tasks: status of run %s: %w", t.RunID, err)
	}
	switch status {
	case store.StatusQueued, store.StatusRunning:
		return nil
	case "":
		return s.failedAttempt(ctx, t, "the run is missing")
	}
	// The run's interrupt is not known any more: a run that ended waiting for a
	// human is recovered like a finished one, and its feedback tells the next
	// attempt to check the thread.
	return s.runEnded(ctx, t.ThreadID, t.RunID, status, nil, "")
}
