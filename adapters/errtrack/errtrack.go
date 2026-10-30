// Package errtrack sends the errors the harness logs to an error tracker that speaks the Sentry protocol
// (Sentry, Rustrak, GlitchTip…).
//
// The engine already reports every internal failure with slog.Error (a failed run, an agent panic, a sweeper or
// storage failure, an HTTP 500). Handler wraps the process's slog handler so that each such record is also sent to
// the tracker, with no change at the call sites. Records below Error level and context cancellations are not sent.
//
// An event is grouped by the log message, not by the dynamic error text, so "run failed" is one issue however many
// runs fail; every event still carries its own error, run id and attributes. Nothing else is attached: no request
// bodies, no headers, no user identity.
package errtrack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
)

// Options configure a Tracker.
type Options struct {
	// DSN is the project's Sentry DSN (required).
	DSN string
	// Environment tags every event ("production", "development"…).
	Environment string
	// Release tags every event with the build that produced it (optional).
	Release string
}

// Tracker sends events to the tracker.
type Tracker struct {
	client *sentry.Client
}

// New connects to the tracker named by o.DSN. It does not touch the network: events are queued and sent in the
// background, so an unreachable tracker never blocks the caller.
func New(o Options) (*Tracker, error) {
	if o.DSN == "" {
		return nil, errors.New("errtrack: empty DSN")
	}
	c, err := sentry.NewClient(sentry.ClientOptions{
		Dsn: o.DSN, Environment: o.Environment, Release: o.Release,
		SendDefaultPII: false,
	})
	if err != nil {
		return nil, fmt.Errorf("errtrack: %w", err)
	}
	return &Tracker{client: c}, nil
}

// Flush waits up to d for queued events to be sent and reports whether all were. Call it before the process exits.
func (t *Tracker) Flush(d time.Duration) bool { return t.client.Flush(d) }

// Handler returns a slog.Handler that passes every record to inner and also reports the records of level Error
// and above to the tracker. It is enabled for those records even when inner is not.
func (t *Tracker) Handler(inner slog.Handler) slog.Handler { return &handler{inner: inner, t: t} }

type handler struct {
	inner slog.Handler
	t     *Tracker
	attrs []slog.Attr // from WithAttrs, group-qualified
	group string      // current group prefix, "a.b."
}

func (h *handler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelError || h.inner.Enabled(ctx, l)
}

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		h.report(r)
	}
	if h.inner.Enabled(ctx, r.Level) {
		return h.inner.Handle(ctx, r)
	}
	return nil
}

func (h *handler) WithAttrs(as []slog.Attr) slog.Handler {
	n := *h
	n.inner = h.inner.WithAttrs(as)
	n.attrs = slices.Clone(h.attrs)
	for _, a := range as {
		n.attrs = flatten(n.attrs, h.group, a)
	}
	return &n
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	n := *h
	n.inner = h.inner.WithGroup(name)
	n.group = h.group + name + "."
	return &n
}

// report builds and queues the event of one record.
func (h *handler) report(r slog.Record) {
	attrs := slices.Clone(h.attrs)
	r.Attrs(func(a slog.Attr) bool {
		attrs = flatten(attrs, h.group, a)
		return true
	})

	exc := sentry.Exception{Type: "log.Error", Value: r.Message}
	extra := map[string]any{}
	tags := map[string]string{}
	for _, a := range attrs {
		v := a.Value.Any()
		switch {
		case a.Key == "error" || strings.HasSuffix(a.Key, ".error"):
			if err, ok := v.(error); ok {
				if errors.Is(err, context.Canceled) {
					return
				}
				exc = sentry.Exception{Type: fmt.Sprintf("%T", err), Value: err.Error()}
				continue
			}
		case a.Key == "panic":
			exc = sentry.Exception{Type: "panic", Value: fmt.Sprint(v)}
			continue
		case a.Key == "run_id" || a.Key == "thread_id":
			tags[a.Key] = fmt.Sprint(v)
		}
		extra[a.Key] = scalar(a.Value)
	}
	exc.Stacktrace = callStack()

	ev := sentry.NewEvent()
	ev.Level = sentry.LevelError
	ev.Logger = "slog"
	ev.Timestamp = r.Time
	ev.Message = r.Message
	ev.Fingerprint = []string{groupKey(r.Message)}
	ev.Exception = []sentry.Exception{exc}
	ev.Tags = tags
	ev.Contexts = map[string]sentry.Context{"log": extra}
	h.t.client.CaptureEvent(ev, nil, nil)
}

// groupKey is what makes two records one issue: the first line of the message with runs of digits replaced, so
// "http: panic serving 10.0.0.7:5432: boom" and the same panic from another client are one issue, and a goroutine
// dump that follows the first line does not split them.
func groupKey(msg string) string {
	first, _, _ := strings.Cut(msg, "\n")
	return digits.ReplaceAllString(first, "#")
}

var digits = regexp.MustCompile(`[0-9]+`)

// flatten appends a to out under prefix, expanding groups into dotted keys.
func flatten(out []slog.Attr, prefix string, a slog.Attr) []slog.Attr {
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindGroup {
		p := prefix
		if a.Key != "" {
			p += a.Key + "."
		}
		for _, g := range a.Value.Group() {
			out = flatten(out, p, g)
		}
		return out
	}
	if a.Equal(slog.Attr{}) {
		return out
	}
	a.Key = prefix + a.Key
	return append(out, a)
}

// scalar keeps numbers, booleans and strings as they are and renders anything else as text, so an attribute can
// never make the event unserialisable.
func scalar(v slog.Value) any {
	switch v.Kind() {
	case slog.KindString, slog.KindInt64, slog.KindUint64, slog.KindFloat64, slog.KindBool:
		return v.Any()
	}
	return fmt.Sprint(v.Any())
}

// callStack is the stack of the goroutine that logged, without the frames of the logging machinery. During a
// recovered panic it still contains the frames that panicked.
func callStack() *sentry.Stacktrace {
	st := sentry.NewStacktrace()
	if st == nil {
		return nil
	}
	n := len(st.Frames)
	for n > 0 && isLogging(st.Frames[n-1]) {
		n--
	}
	st.Frames = st.Frames[:n]
	return st
}

func isLogging(f sentry.Frame) bool {
	return f.Module == "log/slog" || strings.HasSuffix(f.Module, "/adapters/errtrack")
}
