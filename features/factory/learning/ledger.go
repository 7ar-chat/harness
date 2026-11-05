// Package learning closes the loop of the agent factory: it records what every run was given (recalled memory,
// skills, prompt variant, model) and how its task ended, scores the memory entries by the outcomes they were
// present for, retires the ones that hurt and promotes the ones that help, mines recurring failures into proposals,
// and, under guardrails, turns those proposals into factory tasks that change prompts, skills or lessons behind an
// eval gate (bench.Gate) and a pull request.
//
// Nothing here changes the schema: attribution is stored as task events (factory.EventContext, EventOutcome) and
// the per-entry counters in the memory entry's metadata.
package learning

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/7ar-chat/harness/core/factory"
)

// Context is what one run was given, as recorded in a factory.EventContext event.
type Context struct {
	RunID string `json:"runId,omitempty"`
	// Memory are the ids of the recalled memory entries; Keys maps each to the memory key it lives in.
	Memory        []string          `json:"memory,omitempty"`
	Keys          map[string]string `json:"keys,omitempty"`
	Skills        []string          `json:"skills,omitempty"`
	PromptVersion string            `json:"promptVersion,omitempty"`
	Model         string            `json:"model,omitempty"`
}

// toData and fromData go through JSON because the Postgres store returns event data as decoded JSON ([]any, not
// []string).
func toData(v any) map[string]any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func fromData(m map[string]any, v any) bool {
	if len(m) == 0 {
		return false
	}
	b, err := json.Marshal(m)
	if err != nil {
		return false
	}
	return json.Unmarshal(b, v) == nil
}

// Recorder writes a task's Context events. Its Recall method is the memory service's OnRecall hook (through a
// small adapter in the facade, because a feature may not import another).
type Recorder struct {
	Store factory.Store
	// PromptVersion names the prompt variant a task runs with (nil: none).
	PromptVersion func(factory.Task) string

	mu   sync.Mutex
	seen map[string]string // run id -> hash of what was recorded for it
}

const seenCap = 4096

// Recall records that run runID of the thread was given the memory entries ids (key by id). Recalls of threads
// that are not a factory task's are ignored, and so is a repeat of what the run already recorded.
func (r *Recorder) Recall(ctx context.Context, runID, threadID string, ids []string, keys map[string]string) {
	if r == nil || r.Store == nil || threadID == "" || len(ids) == 0 {
		return
	}
	ids = uniq(ids)
	h := hashOf(ids)
	r.mu.Lock()
	if r.seen == nil || len(r.seen) >= seenCap {
		r.seen = map[string]string{}
	}
	if r.seen[runID] == h {
		r.mu.Unlock()
		return
	}
	r.seen[runID] = h
	r.mu.Unlock()

	t, err := TaskOfThread(ctx, r.Store, threadID)
	if err != nil {
		slog.Warn("learning: finding the task of a recall failed", "thread", threadID, "error", err)
		return
	}
	if t == nil {
		return
	}
	c := Context{RunID: runID, Memory: ids, Keys: keys, Model: t.Model}
	if r.PromptVersion != nil {
		c.PromptVersion = r.PromptVersion(*t)
	}
	err = r.Store.AppendEvent(ctx, factory.TaskEvent{TaskID: t.ID, Kind: factory.EventContext,
		Text: "context: " + itoa(len(ids)) + " memory entries recalled", Data: toData(c)})
	if err != nil {
		slog.Warn("learning: recording a run's context failed", "task", t.ID, "error", err)
	}
}

// TaskOfThread finds the task a thread works on: by the thread recorded on the task, else by the thread's info.
// It is nil, nil for a thread that is no task's.
func TaskOfThread(ctx context.Context, st factory.Store, threadID string) (*factory.Task, error) {
	if threadID == "" {
		return nil, nil
	}
	ts, err := st.ListTasks(ctx, factory.Filter{ThreadID: threadID, Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(ts) > 0 {
		return &ts[0], nil
	}
	ti, err := st.GetThreadInfo(ctx, threadID)
	if errors.Is(err, factory.ErrNotFound) || err == nil && ti.TaskID == "" {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t, err := st.GetTask(ctx, ti.TaskID)
	if errors.Is(err, factory.ErrNotFound) {
		return nil, nil
	}
	return t, err
}

// contextsOf decodes the Context events of a history, oldest first.
func contextsOf(ev []factory.TaskEvent) []Context {
	var out []Context
	for _, e := range ev {
		if e.Kind != factory.EventContext {
			continue
		}
		var c Context
		if fromData(e.Data, &c) {
			out = append(out, c)
		}
	}
	return out
}

func uniq(in []string) []string {
	out := slices.Clone(in)
	sort.Strings(out)
	return slices.Compact(out)
}

func hashOf(ids []string) string {
	sum := sha256.Sum256([]byte(strings.Join(ids, "\x00")))
	return hex.EncodeToString(sum[:6])
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }
