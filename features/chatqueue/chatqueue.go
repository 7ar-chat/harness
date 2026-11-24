// Package chatqueue is the queue of messages a user sends while a reply is still running. The queue of a
// thread is one document of the thread-state store, so it survives a reload and is the same for every replica;
// the harness sends the head of the queue as a new run when the running one ends (harness_queue.go).
package chatqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/7ar-chat/harness/core/store"
)

// Key is the thread-state key of the queue.
const Key = "queue"

// Limits of a queue.
const (
	MaxItems = 50
	MaxChars = 100_000
)

// ErrFull and ErrEmpty are the validation errors of Add.
var (
	ErrFull  = fmt.Errorf("the queue is full (at most %d messages)", MaxItems)
	ErrEmpty = errors.New("message is required")
)

// Options are the run options a queued message is sent with: those of the composer when it was queued.
type Options struct {
	Model           string   `json:"model,omitempty"`
	ReasoningEffort string   `json:"reasoningEffort,omitempty"`
	Skills          []string `json:"skills,omitempty"`
	Interactive     bool     `json:"interactive,omitempty"`
	Mode            string   `json:"mode,omitempty"`
	Sandbox         string   `json:"sandbox,omitempty"`
	Role            string   `json:"role,omitempty"`
	Author          string   `json:"author,omitempty"`
	AuthorKind      string   `json:"authorKind,omitempty"`
	OnBehalfOf      string   `json:"onBehalfOf,omitempty"`
	// Attachments are the files sent with the message, already stored in the thread's workspace when it was queued.
	Attachments []store.Attachment `json:"attachments,omitempty"`
}

// Item is a queued message. Its id is the id of the user message it becomes, which makes sending it idempotent.
type Item struct {
	ID        string    `json:"id"`
	Text      string    `json:"text"`
	Options   Options   `json:"options"`
	CreatedAt time.Time `json:"createdAt"`
}

// Queue reads and writes the queues of threads.
type Queue struct{ St store.ThreadStateStore }

func decode(raw json.RawMessage) []Item {
	var items []Item
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &items)
	}
	return items
}

func encode(items []Item) (json.RawMessage, error) {
	if len(items) == 0 {
		return nil, nil // an empty queue is no document
	}
	return json.Marshal(items)
}

// Add appends a message to the thread's queue.
func (q *Queue) Add(ctx context.Context, threadID, text string, o Options) (Item, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Item{}, ErrEmpty
	}
	if len([]rune(text)) > MaxChars {
		return Item{}, fmt.Errorf("the message is too long (at most %d characters)", MaxChars)
	}
	it := Item{ID: uuid.NewString(), Text: text, Options: o, CreatedAt: time.Now().UTC().Truncate(time.Millisecond)}
	err := q.St.UpdateThreadState(ctx, threadID, Key, func(old json.RawMessage) (json.RawMessage, error) {
		items := decode(old)
		if len(items) >= MaxItems {
			return nil, ErrFull
		}
		return encode(append(items, it))
	})
	return it, err
}

// List returns the queue in sending order.
func (q *Queue) List(ctx context.Context, threadID string) ([]Item, error) {
	raw, err := q.St.GetThreadState(ctx, threadID, Key)
	if err != nil {
		return nil, err
	}
	items := decode(raw)
	if items == nil {
		items = []Item{}
	}
	return items, nil
}

// Head returns the first message of the queue.
func (q *Queue) Head(ctx context.Context, threadID string) (Item, bool, error) {
	items, err := q.List(ctx, threadID)
	if err != nil || len(items) == 0 {
		return Item{}, false, err
	}
	return items[0], true, nil
}

// Remove deletes a message and returns it; false when it is not (or no longer) queued. Removing is how a
// message is claimed: of two callers removing the same id, one gets it.
func (q *Queue) Remove(ctx context.Context, threadID, id string) (Item, bool, error) {
	var got Item
	var found bool
	err := q.St.UpdateThreadState(ctx, threadID, Key, func(old json.RawMessage) (json.RawMessage, error) {
		items := decode(old)
		out := items[:0:0]
		for _, it := range items {
			if it.ID == id && !found {
				got, found = it, true
				continue
			}
			out = append(out, it)
		}
		if !found {
			return old, nil
		}
		return encode(out)
	})
	return got, found, err
}

// Clear empties the queue and returns what was waiting.
func (q *Queue) Clear(ctx context.Context, threadID string) ([]Item, error) {
	var got []Item
	err := q.St.UpdateThreadState(ctx, threadID, Key, func(old json.RawMessage) (json.RawMessage, error) {
		got = decode(old)
		return nil, nil
	})
	return got, err
}

// Prepend puts a message back at the front of the queue (a steer that did not take). A message already queued
// is not duplicated.
func (q *Queue) Prepend(ctx context.Context, threadID string, it Item) error {
	return q.St.UpdateThreadState(ctx, threadID, Key, func(old json.RawMessage) (json.RawMessage, error) {
		items := decode(old)
		for _, x := range items {
			if x.ID == it.ID {
				return old, nil
			}
		}
		return encode(append([]Item{it}, items...))
	})
}
