package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/GhaziBenDahmane/harness/coding/gitsafe"
	"github.com/GhaziBenDahmane/harness/core/agui"
	"github.com/GhaziBenDahmane/harness/core/store"
)

// Errors of the history operations (Compact, Undo, Redo, Fork).
var (
	// ErrNothingToUndo means the thread has no user message left to remove.
	ErrNothingToUndo = errors.New("nothing to undo")
	// ErrNothingToRedo means no undone step is available: none was undone, or a message was sent since.
	ErrNothingToRedo = errors.New("nothing to redo")
	// ErrHistoryUnsupported means the Store cannot edit a thread's history (store.HistoryStore).
	ErrHistoryUnsupported = errors.New("the store cannot edit a thread's history")
)

// CompactResult is the outcome of Compact.
type CompactResult struct {
	// Compacted is false when there was nothing to summarise.
	Compacted    bool
	TokensBefore int
	TokensAfter  int
}

// HistoryStep is the outcome of Undo and Redo.
type HistoryStep struct {
	// Prompt is the user message removed (Undo) or restored (Redo).
	Prompt string
	// Files are the workspace paths whose content changed; empty without a git workspace.
	Files []string
	// CanUndo and CanRedo tell whether another step is available.
	CanUndo, CanRedo bool
}

// Compact summarises the thread's history now with a run of kind compact on model, and waits for it.
// store.ErrRunActive (wrapped) when the thread has a run in progress.
func (h *Harness) Compact(ctx context.Context, user, threadID, model string) (*CompactResult, error) {
	if err := h.ownsThread(ctx, user, threadID); err != nil {
		return nil, err
	}
	if model == "" || !h.cfg.ModelAllowed(model) {
		return nil, fmt.Errorf("%w: model %q is not allowed", ErrInvalidRequest, model)
	}
	run, _, err := h.Store.CreateRun(ctx, store.NewRun{RunID: uuid.NewString(), ThreadID: threadID, UserLogin: user, Model: model, Kind: store.KindCompact})
	if err != nil {
		return nil, err
	}
	if err := h.Queue.Enqueue(ctx, run.ID); err != nil {
		return nil, err
	}
	res := &CompactResult{}
	var failure string
	for ev := range h.Events(ctx, run.ID, 0) {
		var e struct {
			Name  string `json:"name"`
			Value struct {
				Phase        string `json:"phase"`
				TokensBefore int    `json:"tokensBefore"`
				TokensAfter  int    `json:"tokensAfter"`
				Summarized   int    `json:"summarizedMessages"`
			} `json:"value"`
			Message string `json:"message"`
		}
		if json.Unmarshal(ev.Data, &e) != nil {
			continue
		}
		switch {
		case ev.Type == agui.RunError:
			failure = e.Message
		case e.Name == agui.CustomCompaction && e.Value.Phase == "completed":
			res = &CompactResult{Compacted: e.Value.Summarized > 0, TokensBefore: e.Value.TokensBefore, TokensAfter: e.Value.TokensAfter}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if failure != "" {
		return nil, fmt.Errorf("compaction failed: %s", failure)
	}
	return res, nil
}

// redoStep is an undone step: what Redo puts back.
type redoStep struct {
	removed []*store.Message
	// backup is the workspace state replaced by the undo ("" without a git workspace).
	backup string
	// visible is the number of messages the thread had after the undo: a different count means a
	// message was sent since, which ends the redo.
	visible int
}

// redoStacks keeps, per thread, the steps Undo removed (the last one on top).
type redoStacks struct {
	mu sync.Mutex
	by map[string][]redoStep
}

func (r *redoStacks) get(threadID string) []redoStep {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]redoStep(nil), r.by[threadID]...)
}

func (r *redoStacks) set(threadID string, steps []redoStep) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(steps) == 0 {
		delete(r.by, threadID)
		return
	}
	if r.by == nil {
		r.by = map[string][]redoStep{}
	}
	r.by[threadID] = steps
}

// historyStore returns the Store's history editing, or ErrHistoryUnsupported.
func (h *Harness) historyStore() (store.HistoryStore, error) {
	hs, ok := h.Store.(store.HistoryStore)
	if !ok {
		return nil, ErrHistoryUnsupported
	}
	return hs, nil
}

// workspaceDir is the git workspace undo restores files in ("" without a local sandbox).
func (h *Harness) workspaceDir() string {
	if !h.cfg.LocalSandbox() {
		return ""
	}
	if h.cfg.LocalWorkspace != "" {
		return h.cfg.LocalWorkspace
	}
	dir, _ := os.Getwd()
	return dir
}

// idle returns store.ErrRunActive (wrapped) when the thread has a run in progress.
func (h *Harness) idle(ctx context.Context, user, threadID string) error {
	th, err := h.Store.GetThread(ctx, user, threadID)
	if err != nil {
		return err
	}
	if th.ActiveRun != nil {
		return store.ErrRunActive
	}
	return nil
}

// lastUserMessage is the index of the last message the user typed (a steer is not one), -1 when none.
func lastUserMessage(msgs []*store.Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == store.RoleUser && !msgs[i].IsSteer() {
			return i
		}
	}
	return -1
}

// Undo removes the thread's last user message with everything after it and, in a git workspace, restores
// the files to the snapshot taken before that message's run. The replaced workspace state is kept so Redo
// can bring it back. The thread must be idle (store.ErrRunActive).
func (h *Harness) Undo(ctx context.Context, user, threadID string) (*HistoryStep, error) {
	hs, err := h.historyStore()
	if err != nil {
		return nil, err
	}
	if err := h.idle(ctx, user, threadID); err != nil {
		return nil, err
	}
	msgs, err := h.Store.ListMessages(ctx, threadID)
	if err != nil {
		return nil, err
	}
	// A message sent since the last undo ends its redo steps.
	steps := h.redo.get(threadID)
	if n := len(steps); n > 0 && steps[n-1].visible != len(msgs) {
		steps = nil
	}
	i := lastUserMessage(msgs)
	if i < 0 {
		return nil, ErrNothingToUndo
	}
	target := msgs[i]
	step := redoStep{removed: msgs[i:], visible: i}

	var files []string
	dir := h.workspaceDir()
	var backup, restored gitsafe.Snap
	if dir != "" && target.RunID != "" {
		if restored, err = gitsafe.ForRun(ctx, dir, threadID, target.RunID); err == nil {
			if backup, err = gitsafe.Restore(ctx, dir, restored.Ref); err != nil {
				return nil, fmt.Errorf("restoring the files: %w", err)
			}
			step.backup = backup.Ref
			files, _ = gitsafe.Changed(ctx, dir, backup.Ref, restored.Ref)
		} else {
			slog.Debug("undo: files not restored", "thread_id", threadID, "error", err)
		}
	}
	if err := hs.SupersedeFrom(ctx, user, threadID, target.ID); err != nil {
		if step.backup != "" { // the messages stay: put the files back as they were
			if _, rerr := gitsafe.Restore(ctx, dir, step.backup); rerr != nil {
				slog.Warn("undo: putting the files back failed", "thread_id", threadID, "error", rerr)
			}
		}
		return nil, err
	}
	h.redo.set(threadID, append(steps, step))
	return &HistoryStep{Prompt: target.Content, Files: files, CanUndo: lastUserMessage(msgs[:i]) >= 0, CanRedo: true}, nil
}

// Redo reapplies the step Undo removed last: its messages and, in a git workspace, its files. It is valid
// until a message is sent. The thread must be idle (store.ErrRunActive).
func (h *Harness) Redo(ctx context.Context, user, threadID string) (*HistoryStep, error) {
	if err := h.idle(ctx, user, threadID); err != nil {
		return nil, err
	}
	steps := h.redo.get(threadID)
	if len(steps) == 0 {
		return nil, ErrNothingToRedo
	}
	step := steps[len(steps)-1]
	msgs, err := h.Store.ListMessages(ctx, threadID)
	if err != nil {
		return nil, err
	}
	if len(msgs) != step.visible {
		h.redo.set(threadID, nil)
		return nil, ErrNothingToRedo
	}
	var files []string
	if dir := h.workspaceDir(); dir != "" && step.backup != "" {
		backup, err := gitsafe.Restore(ctx, dir, step.backup)
		if err != nil {
			return nil, fmt.Errorf("restoring the files: %w", err)
		}
		files, _ = gitsafe.Changed(ctx, dir, backup.Ref, step.backup)
	}
	back := make([]*store.Message, 0, len(step.removed))
	for _, m := range step.removed {
		cp := *m
		cp.ID, cp.Position = uuid.NewString(), 0
		back = append(back, &cp)
	}
	if err := h.Store.AppendMessages(ctx, back...); err != nil {
		return nil, err
	}
	steps = steps[:len(steps)-1]
	// The messages are back: the steps below were taken from a thread of that size.
	h.redo.set(threadID, steps)
	return &HistoryStep{Prompt: step.removed[0].Content, Files: files, CanUndo: true, CanRedo: len(steps) > 0}, nil
}

// Fork copies the thread's messages up to and including messageID ("" is the whole thread, a tool call
// is kept with its results) into a new thread titled "<old> (fork)".
func (h *Harness) Fork(ctx context.Context, user, threadID, messageID string) (*store.Thread, error) {
	old, err := h.Store.GetThread(ctx, user, threadID)
	if err != nil {
		return nil, err
	}
	msgs, err := h.Store.ListMessages(ctx, threadID)
	if err != nil {
		return nil, err
	}
	if messageID != "" {
		end := -1
		for i, m := range msgs {
			if m.ID == messageID {
				end = i + 1
			}
		}
		if end < 0 {
			return nil, store.ErrNotFound
		}
		// An assistant message that called tools brings the results that answer the calls.
		calls := map[string]bool{}
		for _, c := range msgs[end-1].ToolCalls {
			calls[c.ID] = true
		}
		for end < len(msgs) && msgs[end].Role == store.RoleTool && calls[msgs[end].ToolCallID] {
			end++
		}
		msgs = msgs[:end]
	}
	title := strings.TrimSpace(old.Title + " (fork)")
	th, err := h.Store.CreateThread(ctx, user, title)
	if err != nil {
		return nil, err
	}
	copies := make([]*store.Message, 0, len(msgs))
	for _, m := range msgs {
		cp := *m
		cp.ID, cp.ThreadID, cp.RunID, cp.Position = uuid.NewString(), th.ID, "", 0
		copies = append(copies, &cp)
	}
	if err := h.Store.AppendMessages(ctx, copies...); err != nil {
		_, _ = h.Store.DeleteThread(ctx, user, th.ID)
		return nil, err
	}
	return h.Store.GetThread(ctx, user, th.ID)
}
