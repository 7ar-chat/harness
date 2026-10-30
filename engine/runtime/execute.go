package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/GhaziBenDahmane/harness/core/agui"
	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/agent"
	"github.com/GhaziBenDahmane/harness/engine/eventlog"
)

func (r *Runtime) execute(parent context.Context, run *store.Run) {
	runCtx, cancel := context.WithCancelCause(context.WithoutCancel(parent))
	defer cancel(nil)
	r.mu.Lock()
	r.inflight[run.ID] = cancel
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.inflight, run.ID)
		r.mu.Unlock()
	}()
	log := slog.With("run_id", run.ID, "thread_id", run.ThreadID)
	log.Info("run started")

	if r.bus != nil {
		unsub, err := r.bus.SubscribeCancel(runCtx, run.ID, func() { cancel(ErrCancelled) })
		if err != nil {
			log.Warn("cancel subscription failed; relying on heartbeat polling", "error", err)
		}
		defer unsub()
	}
	hbDone := make(chan struct{})
	defer close(hbDone)
	go r.heartbeat(run, cancel, hbDone)
	if run.CancelRequested {
		cancel(ErrCancelled)
	}

	w := &ownedWriter{Writer: r.log.Writer(run.ID, run.LastSeq)}
	var res agent.Result
	var err error
	if _, aerr := w.Append(runCtx, agui.RunStartedEvent(run.ThreadID, run.ID)); aerr != nil {
		err = &agent.Error{Code: agui.CodeInternal, Message: agent.MsgInternal, Detail: aerr.Error()}
	} else {
		res, err = r.safeRun(runCtx, agent.Input{Run: run}, w)
	}

	if w.lost.Load() { // another writer (the sweeper) appended to this run's log
		cancel(errLost)
	}
	status, errMsg, final := r.classify(runCtx, run, res, err)
	if final == nil { // ownership lost: the sweeper finished this run
		r.abandon(runCtx, run, w)
		return
	}
	// Stop accepting steers; accepted but undelivered steers stay in the thread and are announced.
	announceSteers(runCtx, w, r.keepUndeliveredSteers(runCtx, run))
	r.recordInterrupt(runCtx, run, status, res)
	finish := &Finish{Run: run, Status: status, Result: res, w: w}
	after := r.runHooks(runCtx, finish)
	final = finish.withUsage(final) // the hooks' model calls count in RUN_FINISHED.usage (harness v3 4.5)
	// Persist, then finish the run, then publish the terminal event: a client that
	// sees RUN_FINISHED and reloads the thread must find the run terminal.
	if ferr := w.Flush(runCtx); ferr != nil {
		log.Error("flushing events failed", "error", ferr)
	}
	// The row turns terminal before the continuations emit their last events:
	// replay waits for the real terminal event meanwhile (never a synthetic one).
	unmark := r.log.Finalizing(runCtx, run.ID, r.opts.FinishRetry+2*time.Minute)
	defer unmark()
	if !r.finishRun(run, status, errorCode(final), errMsg) || w.lost.Load() {
		r.abandon(runCtx, run, w)
		return
	}
	for _, next := range after {
		safeHook(run.ID, func() struct{} { next(runCtx); return struct{}{} })
	}
	if _, aerr := w.Append(runCtx, final); aerr != nil {
		log.Error("writing terminal event failed", "error", aerr)
	}
	cctx, ccancel := r.finishCtx(runCtx)
	defer ccancel()
	if cerr := w.Close(cctx); cerr != nil {
		log.Error("closing event log failed", "error", cerr)
	}
	log.Info("run finished", "status", status)
}

// ownedWriter records that an append found its seq written by another writer
// (the sweeper finished the run): the run's ownership is lost.
type ownedWriter struct {
	eventlog.Writer
	lost atomic.Bool
}

func (o *ownedWriter) Append(ctx context.Context, ev agui.Event) (int64, error) {
	seq, err := o.Writer.Append(ctx, ev)
	if errors.Is(err, eventlog.ErrConflict) {
		o.lost.Store(true)
	}
	return seq, err
}

// abandon stops a run another runtime (the sweeper) finished: no hooks'
// continuations, no terminal event; what was written is persisted.
func (r *Runtime) abandon(ctx context.Context, run *store.Run, w eventlog.Writer) {
	slog.Warn("run ownership lost; stopping without writing a terminal event", "run_id", run.ID)
	ctx, cancel := r.finishCtx(ctx)
	defer cancel()
	if cerr := w.Close(ctx); cerr != nil { // persist what we have, expire the stream
		slog.Warn("closing event log failed", "run_id", run.ID, "error", cerr)
	}
}

// finishRun records the final status within FinishRetry (survives short
// Postgres outages); false when the run is no longer this runtime's.
func (r *Runtime) finishRun(run *store.Run, status, code, errMsg string) bool {
	deadline := r.finishDeadline(r.opts.FinishRetry)
	for backoff := 100 * time.Millisecond; ; backoff = min(backoff*2, 2*time.Second) {
		actx, acancel := context.WithTimeout(context.Background(), 5*time.Second)
		ferr := r.store.FinishRun(actx, run.ID, r.opts.ID, status, code, errMsg)
		if errors.Is(ferr, store.ErrNotOwned) {
			// A retry of an update whose reply was lost finds its own result.
			cur, gerr := r.store.GetRun(actx, run.ID)
			ferr = gerr
			if gerr == nil {
				acancel()
				return cur.RuntimeID == r.opts.ID && cur.Status == status
			}
		}
		acancel()
		if ferr == nil {
			return true
		}
		if time.Now().Add(backoff).After(deadline) {
			slog.Error("finishing run failed; the sweeper will reconcile it from the log", "run_id", run.ID, "error", ferr)
			return true
		}
		time.Sleep(backoff)
	}
}

func (r *Runtime) safeRun(ctx context.Context, in agent.Input, w eventlog.Writer) (res agent.Result, err error) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("agent panic", "run_id", in.Run.ID, "panic", p)
			if r.opts.Reporter != nil {
				r.opts.Reporter.CapturePanic(p)
			}
			err = &agent.Error{Code: agui.CodeInternal, Message: agent.MsgInternal, Detail: fmt.Sprint(p)}
		}
	}()
	return r.exec.Run(ctx, in, w)
}

// outcomeOf maps a terminal event to the run status it implies.
func outcomeOf(e store.Event) (status, code, msg string) {
	var ev struct {
		Outcome struct {
			Type string `json:"type"`
		} `json:"outcome"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(e.Data, &ev)
	switch {
	case e.Type == agui.RunFinished && ev.Outcome.Type == "cancelled":
		return store.StatusCancelled, "", ""
	case e.Type == agui.RunFinished:
		return store.StatusCompleted, "", ""
	case ev.Code == agui.CodeInterrupted:
		return store.StatusInterrupted, ev.Code, ev.Message
	default:
		return store.StatusFailed, ev.Code, ev.Message
	}
}

// errorCode is the RUN_ERROR code of a terminal event ("" for RUN_FINISHED).
func errorCode(final agui.Event) string {
	code, _ := final["code"].(string)
	return code
}

func (r *Runtime) classify(runCtx context.Context, run *store.Run, res agent.Result, err error) (string, string, agui.Event) {
	if cause := context.Cause(runCtx); runCtx.Err() != nil {
		switch {
		case errors.Is(cause, errLost):
			return "", "", nil
		case errors.Is(cause, ErrCancelled):
			return store.StatusCancelled, "", agui.RunFinishedEvent(run.ThreadID, run.ID, "cancelled", res.Usage)
		default:
			return store.StatusInterrupted, InterruptedMessage, agui.RunErrorEvent(agui.CodeInterrupted, InterruptedMessage)
		}
	}
	if err == nil {
		return store.StatusCompleted, "", successEvent(run, res)
	}
	var ae *agent.Error
	if errors.As(err, &ae) {
		slog.Warn("run failed", "run_id", run.ID, "code", ae.Code, "detail", ae.Detail)
		return store.StatusFailed, ae.Message, agui.RunErrorEvent(ae.Code, ae.Message)
	}
	slog.Error("run failed", "run_id", run.ID, "error", err)
	if r.opts.Reporter != nil {
		r.opts.Reporter.CaptureError(err)
	}
	return store.StatusFailed, agent.MsgInternal, agui.RunErrorEvent(agui.CodeInternal, agent.MsgInternal)
}
