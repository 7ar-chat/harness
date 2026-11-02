package filestore

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/7ar-chat/harness/core/store"
)

// SessionInfo summarises a thread for a session picker.
type SessionInfo struct {
	ID        string
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
	Pinned    bool
	// Preview is the start of the last user or assistant message.
	Preview  string
	Messages int
	// ActiveRun is set while a run is queued or running.
	ActiveRun *store.ActiveRun
	// LastStatus is the status of the latest run ("" without runs).
	LastStatus string
}

// Sessions is the session API of a Store: list, fork and delete threads.
type Sessions struct{ s *Store }

// Sessions returns the session helper.
func (s *Store) Sessions() Sessions { return Sessions{s} }

const previewRunes = 120

// List returns the user's threads, most recently updated first (limit <= 0: all).
func (x Sessions) List(ctx context.Context, user string, limit int) ([]SessionInfo, error) {
	threads, err := x.s.ListThreads(ctx, user, 1<<30, nil)
	if err != nil {
		return nil, err
	}
	out := make([]SessionInfo, 0, len(threads))
	for _, t := range threads {
		msgs, err := x.s.ListMessages(ctx, t.ID)
		if err != nil {
			return nil, err
		}
		info := SessionInfo{ID: t.ID, Title: t.Title, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
			Pinned: t.PinnedAt != nil, Messages: len(msgs), ActiveRun: t.ActiveRun}
		if t.LastRun != nil {
			info.LastStatus = t.LastRun.Status
		}
		for i := len(msgs) - 1; i >= 0; i-- {
			if m := msgs[i]; (m.Role == store.RoleUser || m.Role == store.RoleAssistant) && strings.TrimSpace(m.Content) != "" {
				info.Preview = preview(m.Content)
				break
			}
		}
		out = append(out, info)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func preview(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > previewRunes {
		return string(r[:previewRunes]) + "…"
	}
	return s
}

// Fork creates a new thread owned by user holding a copy of the history of
// threadID up to and including message uptoMessageID ("" copies everything).
// The copy ends at a boundary the model can continue from: it never stops
// between an assistant tool call and its results. Summaries, goals and
// approvals are not copied; the plan is recomputed from the copied messages.
func (x Sessions) Fork(ctx context.Context, user, threadID, uptoMessageID string) (*store.Thread, error) {
	src, err := x.s.GetThread(ctx, user, threadID)
	if err != nil {
		return nil, err
	}
	msgs, err := x.s.ListMessages(ctx, threadID)
	if err != nil {
		return nil, err
	}
	if uptoMessageID != "" {
		cut := -1
		for i, m := range msgs {
			if m.ID == uptoMessageID {
				cut = i
			}
		}
		if cut < 0 {
			return nil, store.ErrNotFound
		}
		msgs = msgs[:cut+1]
	}
	// End on a user message or a final assistant answer.
	for len(msgs) > 0 {
		m := msgs[len(msgs)-1]
		if m.Role == store.RoleUser || (m.Role == store.RoleAssistant && len(m.ToolCalls) == 0) {
			break
		}
		msgs = msgs[:len(msgs)-1]
	}
	title := src.Title
	if title != "" {
		title = "Fork of " + title
	}
	th, err := x.s.CreateThread(ctx, user, title)
	if err != nil {
		return nil, err
	}
	copies := make([]*store.Message, len(msgs))
	for i, m := range msgs {
		cp := *m
		cp.ID, cp.ThreadID, cp.RunID, cp.Position = "", th.ID, "", 0
		copies[i] = &cp
	}
	if err := x.s.AppendMessages(ctx, copies...); err != nil {
		_, _ = x.s.DeleteThread(ctx, user, th.ID)
		return nil, err
	}
	if plan := store.LatestPlan(copies); plan != nil {
		if err := x.s.SetThreadPlan(ctx, th.ID, plan); err != nil {
			return nil, err
		}
	}
	return x.s.GetThread(ctx, user, th.ID)
}

// Delete removes a thread (its records are dropped from disk at the next compaction).
func (x Sessions) Delete(ctx context.Context, user, threadID string) error {
	_, err := x.s.DeleteThread(ctx, user, threadID)
	return err
}
