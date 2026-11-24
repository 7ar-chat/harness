package product

import (
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/factory"
)

func TestATaskMovingIsPushedToTheProjectsFeed(t *testing.T) {
	// Arrange: bob (a member) watches the live feed, dave (an outsider) too
	e := newRqEnv(t, rqOpts{factory: true})
	bob, dave := feedOf(e.rig, "bob"), feedOf(e.rig, "dave")
	time.Sleep(200 * time.Millisecond)

	// Act
	e.hooks.OnTaskState(factory.Task{ID: "t-9", ProjectID: e.project, Owner: "alice", Title: "Say hello"}, factory.StateRunning, factory.StateDone)

	// Assert: the web app can refetch the tasks at once; nobody outside the project hears of it
	select {
	case l := <-bob:
		if !strings.Contains(l, `"task.updated"`) || !strings.Contains(l, `"done"`) {
			t.Fatalf("feed line = %s", l)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no task.updated event reached the project's member")
	}
	select {
	case l := <-dave:
		t.Fatalf("an outsider saw %s", l)
	case <-time.After(300 * time.Millisecond):
	}
}
