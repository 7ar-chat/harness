package learning

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/store"
)

// Metadata keys of a memory entry that the learner maintains.
const (
	MetaUses   = "uses"   // finished tasks the entry was recalled in
	MetaWins   = "wins"   // of those, tasks that succeeded
	MetaLosses = "losses" // of those, tasks that failed
	// MetaScored lists the last tasks counted, so a task is never counted twice.
	MetaScored = "scored_tasks"
)

const scoredKeep = 30

// Options configure a Learner.
type Options struct {
	Store  factory.Store
	Memory store.MemoryStore
	// Skills returns the skills the thread loaded or used (nil: none are recorded).
	Skills func(ctx context.Context, threadID string) []string
	// PromptVersion names the prompt variant of a task (nil: none).
	PromptVersion func(factory.Task) string
	Now           func() time.Time
}

// Learner is a factory.Observer: when a task ends it records the outcome with the context its runs had, and
// credits or debits the memory entries those runs recalled.
type Learner struct {
	o  Options
	mu sync.Mutex
}

var _ factory.Observer = (*Learner)(nil)

// NewLearner returns a Learner. Without a Store it observes nothing.
func NewLearner(o Options) *Learner {
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Learner{o: o}
}

// TaskFinished implements factory.Observer. Errors are logged, never returned.
func (l *Learner) TaskFinished(ctx context.Context, t factory.Task, history []factory.TaskEvent) {
	if l == nil || l.o.Store == nil || t.ID == "" {
		return
	}
	result, ok := ResultOf(t.State)
	if !ok {
		return // cancelled: nothing was learned about the context
	}
	if err := l.finished(ctx, t, result); err != nil {
		slog.Warn("learning: recording an outcome failed", "task", t.ID, "error", err)
	}
}

func (l *Learner) finished(ctx context.Context, t factory.Task, result string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	ev, err := l.o.Store.ListEvents(ctx, t.ID, 0)
	if err != nil {
		return fmt.Errorf("listing events: %w", err)
	}
	for _, e := range ev {
		if r, _ := e.Data["result"].(string); e.Kind == factory.EventOutcome && r == result {
			return nil // this outcome is already recorded
		}
	}
	var skills []string
	if l.o.Skills != nil && t.ThreadID != "" {
		skills = l.o.Skills(ctx, t.ThreadID)
	}
	c := Context{Model: t.Model, Skills: skills}
	if l.o.PromptVersion != nil {
		c.PromptVersion = l.o.PromptVersion(t)
	}
	row, _ := RowOf(t, ev)
	data := toData(c)
	data["result"] = result
	data["attempts"] = t.Attempts
	data["usd"] = t.Spent.USD
	data["tokens"] = t.Spent.Tokens
	data["memory"] = row.Memory
	data["fingerprints"] = t.Fingerprints
	text := fmt.Sprintf("outcome: %s after %d attempt(s), %d memory entries in context", result, t.Attempts, len(row.Memory))
	if err := l.o.Store.AppendEvent(ctx, factory.TaskEvent{TaskID: t.ID, Kind: factory.EventOutcome, Text: text, Data: data}); err != nil {
		return fmt.Errorf("appending the outcome: %w", err)
	}

	// Blocked is provisional (a human may unblock the task); only a verdict scores the memory.
	if result == ResultBlocked || l.o.Memory == nil {
		return nil
	}
	var errs []error
	for id, key := range row.MemoryKeys {
		if key == "" {
			continue
		}
		if err := l.score(ctx, key, id, t.ID, result == ResultSuccess); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// score counts one task for one entry, guarded by the entry's version.
func (l *Learner) score(ctx context.Context, key, id, taskID string, win bool) error {
	for range 4 {
		e, err := l.o.Memory.GetMemoryEntry(ctx, key, id)
		if errors.Is(err, store.ErrNotFound) {
			return nil // deleted since
		}
		if err != nil {
			return fmt.Errorf("reading entry %s: %w", id, err)
		}
		if e.Status != store.MemoryActive {
			return nil
		}
		scored := stringsOf(e.Metadata[MetaScored])
		if slices.Contains(scored, taskID) {
			return nil
		}
		c := e.Clone()
		if c.Metadata == nil {
			c.Metadata = map[string]any{}
		}
		c.Metadata[MetaUses] = num(c.Metadata[MetaUses]) + 1
		if win {
			c.Metadata[MetaWins] = num(c.Metadata[MetaWins]) + 1
		} else {
			c.Metadata[MetaLosses] = num(c.Metadata[MetaLosses]) + 1
		}
		scored = append(scored, taskID)
		if len(scored) > scoredKeep {
			scored = scored[len(scored)-scoredKeep:]
		}
		c.Metadata[MetaScored] = scored
		_, err = l.o.Memory.UpdateMemoryEntry(ctx, c, store.MemoryLimits{})
		if errors.Is(err, store.ErrMemoryConflict) {
			continue
		}
		if err != nil {
			return fmt.Errorf("updating entry %s: %w", id, err)
		}
		return nil
	}
	return fmt.Errorf("entry %s kept changing", id)
}

func num(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	}
	return 0
}

func stringsOf(v any) []string {
	switch x := v.(type) {
	case []string:
		return slices.Clone(x)
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
