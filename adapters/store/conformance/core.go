package conformance

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"github.com/google/uuid"

	"github.com/7ar-chat/harness/core/store"
)

// Core runs the suite of the core store.Store port (threads, runs, messages, events), of the sandbox.Store
// bookkeeping on threads and of the store.NotificationStore (unread threads).
func Core(t *testing.T, f Factory) {
	t.Helper()
	t.Run("Ping", func(t *testing.T) { coreSuitePing(t, f) })
	t.Run("Threads", func(t *testing.T) { coreSuiteThreads(t, f) })
	t.Run("Runs", func(t *testing.T) { coreSuiteRuns(t, f) })
	t.Run("RunLifecycle", func(t *testing.T) { coreSuiteRunLifecycle(t, f) })
	t.Run("StaleRuns", func(t *testing.T) { coreSuiteStaleRuns(t, f) })
	t.Run("Messages", func(t *testing.T) { coreSuiteMessages(t, f) })
	t.Run("Events", func(t *testing.T) { coreSuiteEvents(t, f) })
	t.Run("Sandbox", func(t *testing.T) { coreSuiteSandbox(t, f) })
	t.Run("Notifications", func(t *testing.T) { coreSuiteNotifications(t, f) })
}

func coreSuitePing(t *testing.T, f Factory) {
	t.Run("ReportsAHealthyStore", func(t *testing.T) {
		// Arrange
		s := f(t).Store

		// Act
		err := s.Ping(context.Background())

		// Assert
		if err != nil {
			t.Fatalf("Ping: %v", err)
		}
	})
}

// coreNewThread creates a thread or fails the test.
func coreNewThread(t *testing.T, s store.Store, login, title string) *store.Thread {
	t.Helper()
	th, err := s.CreateThread(context.Background(), login, title)
	if err != nil {
		t.Fatalf("CreateThread(%q, %q): %v", login, title, err)
	}
	return th
}

// coreInput is a CreateRun input with a user message.
func coreInput(threadID, login, text string) store.NewRun {
	return store.NewRun{RunID: uuid.NewString(), ThreadID: threadID, UserLogin: login, Model: "model-1",
		Message: store.Message{Role: store.RoleUser, Content: text}}
}

// coreQueue creates a queued run (with a user message) on the thread.
func coreQueue(t *testing.T, s store.Store, th *store.Thread, text string) *store.Run {
	t.Helper()
	r, _, err := s.CreateRun(context.Background(), coreInput(th.ID, th.UserLogin, text))
	if err != nil {
		t.Fatalf("CreateRun(%q): %v", text, err)
	}
	return r
}

// coreStart queues a run and claims it for the runtime.
func coreStart(t *testing.T, s store.Store, th *store.Thread, runtimeID string) *store.Run {
	t.Helper()
	r := coreQueue(t, s, th, "go")
	claimed, err := s.ClaimRun(context.Background(), r.ID, runtimeID)
	if err != nil {
		t.Fatalf("ClaimRun: %v", err)
	}
	return claimed
}

// coreFinish runs a run to the given terminal status on runtime "rt" and returns the stored run.
func coreFinish(t *testing.T, s store.Store, th *store.Thread, status string) *store.Run {
	t.Helper()
	r := coreStart(t, s, th, "rt")
	if err := s.FinishRun(context.Background(), r.ID, "rt", status, "", ""); err != nil {
		t.Fatalf("FinishRun(%s): %v", status, err)
	}
	return coreGetRun(t, s, r.ID)
}

func coreGetRun(t *testing.T, s store.Store, id string) *store.Run {
	t.Helper()
	r, err := s.GetRun(context.Background(), id)
	if err != nil {
		t.Fatalf("GetRun(%s): %v", id, err)
	}
	return r
}

func coreGetThread(t *testing.T, s store.Store, login, id string) *store.Thread {
	t.Helper()
	th, err := s.GetThread(context.Background(), login, id)
	if err != nil {
		t.Fatalf("GetThread(%s): %v", id, err)
	}
	return th
}

// coreThreadIDs lists thread ids in order.
func coreThreadIDs(threads []*store.Thread) []string {
	ids := make([]string, 0, len(threads))
	for _, th := range threads {
		ids = append(ids, th.ID)
	}
	return ids
}

func coreSorted(ss []string) []string {
	out := append([]string(nil), ss...)
	sort.Strings(out)
	return out
}

// coreCursor is the pagination cursor that follows the thread.
func coreCursor(th *store.Thread) *store.ThreadCursor {
	return &store.ThreadCursor{Pinned: th.PinnedAt != nil, CreatedAt: th.CreatedAt, ID: th.ID}
}

// coreJSONEqual compares two JSON documents by value (Postgres jsonb normalizes the text).
func coreJSONEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("not JSON: %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("not JSON: %s: %v", b, err)
	}
	return reflect.DeepEqual(x, y)
}
