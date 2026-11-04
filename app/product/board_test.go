package product

import (
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/factory"
)

func TestBoardColumnPlacesASession(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	fresh, old := now.Add(-time.Hour), now.Add(-30*24*time.Hour)
	task := func(s factory.State) *factory.Task { return &factory.Task{State: s} }
	tests := []struct {
		name    string
		status  string
		task    *factory.Task
		updated time.Time
		want    string
		shown   bool
	}{
		{"idle chat", "idle", nil, fresh, colReady, true},
		{"stale idle chat leaves the board", "idle", nil, old, colReady, false},
		{"agent working", "running", nil, fresh, colRunning, true},
		{"queued", "queued", nil, fresh, colRunning, true},
		{"waiting for a choice", "waiting_input", nil, fresh, colNeeds, true},
		{"waiting for an approval", "waiting_approval", nil, fresh, colNeeds, true},
		{"failed run", "failed", nil, fresh, colNeeds, true},
		{"pull request open", "idle", task(factory.StateReview), fresh, colReview, true},
		{"pull request open but the agent asks", "waiting_input", task(factory.StateReview), fresh, colNeeds, true},
		{"task blocked", "idle", task(factory.StateBlocked), fresh, colNeeds, true},
		{"task failed", "idle", task(factory.StateFailed), fresh, colNeeds, true},
		{"merged", "idle", task(factory.StateDone), old, colDone, true},
		{"cancelled task", "idle", task(factory.StateCancelled), fresh, "", false},
		{"task running between messages", "idle", task(factory.StateRunning), fresh, colRunning, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, shown := columnOf(tc.status, tc.task, tc.updated, now)
			if got != tc.want || shown != tc.shown {
				t.Fatalf("columnOf = %q, %v; want %q, %v", got, shown, tc.want, tc.shown)
			}
		})
	}
}
