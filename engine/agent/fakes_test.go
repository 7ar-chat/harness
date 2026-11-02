package agent

import (
	"context"
	"sync"

	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/store"
)

// fakeChecker is a Checker for the verification state machine: names maps a
// file extension to its checker, results the verdict per path.
type fakeChecker struct {
	names    map[string]string
	commands map[string]string
	results  map[string]CheckResult
	panics   bool
	checked  []string
}

func (c *fakeChecker) CheckerFor(path string) string {
	for ext, name := range c.names {
		if len(path) > len(ext) && path[len(path)-len(ext):] == ext {
			return name
		}
	}
	return ""
}

func (c *fakeChecker) Check(_ context.Context, _ *store.Run, path string) CheckResult {
	c.checked = append(c.checked, path)
	if c.panics {
		panic("checker bug")
	}
	if r, ok := c.results[path]; ok {
		return r
	}
	return CheckResult{Path: path, Checker: c.CheckerFor(path), OK: true, Summary: "all good"}
}

func (c *fakeChecker) CheckCommand(path string) string { return c.commands[path] }

// recordingWriter is an eventlog.Writer that keeps the events it is given.
type recordingWriter struct {
	mu      sync.Mutex
	events  []agui.Event
	failOn  string // event type or custom name whose Append fails
	flushes int
}

func (w *recordingWriter) Append(_ context.Context, ev agui.Event) (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failOn != "" && (ev.Type() == w.failOn || ev["name"] == w.failOn) {
		return 0, errWriter
	}
	w.events = append(w.events, ev)
	return int64(len(w.events)), nil
}

func (w *recordingWriter) Flush(context.Context) error   { w.flushes++; return nil }
func (w *recordingWriter) Close(context.Context) error   { return nil }
func (w *recordingWriter) LastSeq() int64                { return int64(len(w.events)) }
func (w *recordingWriter) Terminal() (store.Event, bool) { return store.Event{}, false }

var errWriter = errString("event log down")

type errString string

func (e errString) Error() string { return string(e) }
