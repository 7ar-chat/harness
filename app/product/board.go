package product

import (
	"net/http"
	"time"

	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/store"
)

// The project board: every session of a project as a card in one of five columns. A session is a chat; once its
// agent opens a pull request it is also a factory task, and the task's state places the card.
//
//	GET /api/projects/{pid}/board → {columns: [{id, title, cards: [Card]}]}
//
// Columns: ready (idle, nothing started), running (the agent works), needs_you (a question, an approval, a failure
// or a blocked task), review (a pull request is open), done (merged or completed). Idle sessions untouched for
// idleWindow drop off the board; the thread list still has them.

const idleWindow = 14 * 24 * time.Hour

const (
	colReady   = "ready"
	colRunning = "running"
	colNeeds   = "needs_you"
	colReview  = "review"
	colDone    = "done"
)

var boardColumns = []struct{ ID, Title string }{
	{colReady, "Ready"}, {colRunning, "Running"}, {colNeeds, "Needs you"}, {colReview, "In review"}, {colDone, "Done"},
}

// BoardCard is one session on the board.
type BoardCard struct {
	ThreadID  string    `json:"threadId"`
	Title     string    `json:"title"`
	CreatedBy string    `json:"createdBy,omitempty"`
	Status    string    `json:"status"`
	UpdatedAt time.Time `json:"updatedAt"`
	// Why says what the card waits for (a question, the blocked reason).
	Why string `json:"why,omitempty"`
	// Task and PR are set once the session opened a pull request.
	TaskID    string `json:"taskId,omitempty"`
	TaskState string `json:"taskState,omitempty"`
	PRNumber  int    `json:"prNumber,omitempty"`
	PRURL     string `json:"prUrl,omitempty"`
}

type boardColumn struct {
	ID    string      `json:"id"`
	Title string      `json:"title"`
	Cards []BoardCard `json:"cards"`
}

// columnOf places a session. task is nil for a session that never opened a pull request.
func columnOf(status string, task *factory.Task, updated, now time.Time) (string, bool) {
	needsYou := status == "waiting_approval" || status == "waiting_input" || status == "failed"
	if task != nil {
		switch task.State {
		case factory.StateDone:
			return colDone, true
		case factory.StateCancelled:
			return "", false
		case factory.StateBlocked, factory.StateFailed:
			return colNeeds, true
		case factory.StateReview:
			if needsYou {
				return colNeeds, true
			}
			return colReview, true
		}
	}
	switch {
	case needsYou:
		return colNeeds, true
	case status == "running" || status == "queued":
		return colRunning, true
	case task != nil && (task.State == factory.StateRunning || task.State == factory.StateReady):
		return colRunning, true
	}
	return colReady, now.Sub(updated) < idleWindow
}

func (s *server) board(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleViewer)
	if !ok {
		return
	}
	threads, err := s.projectThreads(r.Context(), p.ID)
	if err != nil {
		fail(w, err)
		return
	}
	byThread := map[string]*factory.Task{}
	if f := s.factory(); f != nil {
		ts, err := f.Tasks.List(r.Context(), factory.Filter{ProjectID: p.ID, Limit: 500})
		if err != nil {
			fail(w, err)
			return
		}
		for i := range ts {
			// A task lists its latest thread; a session task's thread is the session itself.
			if ts[i].ThreadID != "" {
				byThread[ts[i].ThreadID] = &ts[i]
			}
		}
	}
	cols := make([]boardColumn, len(boardColumns))
	idx := map[string]int{}
	for i, bc := range boardColumns {
		cols[i] = boardColumn{ID: bc.ID, Title: bc.Title, Cards: []BoardCard{}}
		idx[bc.ID] = i
	}
	now := time.Now()
	for _, t := range threads {
		if t.Kind == store.ThreadSupervisor {
			continue
		}
		task := byThread[t.ID]
		col, show := columnOf(t.Status, task, t.UpdatedAt, now)
		if !show {
			continue
		}
		card := BoardCard{ThreadID: t.ID, Title: t.Title, CreatedBy: t.CreatedBy, Status: t.Status, UpdatedAt: t.UpdatedAt, Why: t.Question}
		if task != nil {
			card.TaskID, card.TaskState, card.PRNumber, card.PRURL = task.ID, string(task.State), task.PR.Number, task.PR.URL
			if task.BlockedReason != "" {
				card.Why = task.BlockedReason
			}
		}
		cols[idx[col]].Cards = append(cols[idx[col]].Cards, card)
	}
	for i := range cols {
		cs := cols[i].Cards
		for a := 1; a < len(cs); a++ { // newest first; boards are small
			for b := a; b > 0 && cs[b].UpdatedAt.After(cs[b-1].UpdatedAt); b-- {
				cs[b], cs[b-1] = cs[b-1], cs[b]
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"columns": cols})
}

func (s *server) routeBoard() {
	s.Route("GET /projects/{pid}/board", s.board)
}
