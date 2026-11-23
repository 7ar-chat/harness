package product

import (
	"encoding/json"
	"io"
	"testing"

	"github.com/7ar-chat/harness/core/factory"
)

func (e *fxEnv) inbox(user string) inboxDoc {
	e.t.Helper()
	resp := e.do("GET", "/api/needs-review", user, "")
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		e.t.Fatalf("GET /api/needs-review as %s = %d: %s", user, resp.StatusCode, b)
	}
	var d inboxDoc
	if err := json.Unmarshal(b, &d); err != nil {
		e.t.Fatal(err)
	}
	return d
}

func TestNeedsReviewListsBlockedTasksAndPullRequestsInReview(t *testing.T) {
	// Arrange
	e := newFxEnv(t, fxOpts{})
	blocked := e.newTask("stuck")
	e.block(blocked, "needs a decision")
	inReview := e.newTask("pr open")
	noPR := e.newTask("review without pr")
	e.newTask("just ready")
	review := factory.StateReview
	if _, err := e.st.UpdateTask(e.ctx, inReview, factory.Patch{State: &review, PR: &factory.PullRequest{Number: 7, URL: "https://github.com/x/api/pull/7"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.UpdateTask(e.ctx, noPR, factory.Patch{State: &review}); err != nil {
		t.Fatal(err)
	}

	// Act
	got := e.inbox("bob")

	// Assert
	if len(got.Tasks) != 2 || got.Count != 2 {
		t.Fatalf("tasks = %+v, count %d; want the blocked task and the review with a pull request", got.Tasks, got.Count)
	}
	if k := got.Tasks[0]; k.Kind != "blocked" || k.Task.ID != blocked || k.Reason != "needs a decision" || k.ProjectName != "api" {
		t.Errorf("first item = %+v, want the blocked task first with its reason and project", k)
	}
	if k := got.Tasks[1]; k.Kind != "review" || k.Task.ID != inReview || k.Task.PR.Number != 7 {
		t.Errorf("second item = %+v, want the task in review", k)
	}
	if got.Tasks[0].LastEvent == nil {
		t.Error("a blocked task carries the newest line of its history")
	}
	if got.Threads == nil {
		t.Error("threads must be an empty list, not null")
	}
}

func TestNeedsReviewIsForMembersOfTheProject(t *testing.T) {
	// Arrange
	e := newFxEnv(t, fxOpts{})
	e.block(e.newTask("stuck"), "why")

	// Act and assert
	for user, want := range map[string]int{"alice": 1, "bob": 1, "carol": 0, "dave": 0} {
		if n := len(e.inbox(user).Tasks); n != want {
			t.Errorf("%s sees %d tasks, want %d", user, n, want)
		}
	}
}

func TestNeedsReviewWithoutTheFactoryListsNoTasks(t *testing.T) {
	// Arrange
	r := newRig(t, rigOpts{team: true})

	// Act
	resp := r.do("GET", "/api/needs-review", "alice", "")

	// Assert
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	var d inboxDoc
	if err := json.Unmarshal(b, &d); err != nil || len(d.Tasks) != 0 || d.Count != 0 {
		t.Fatalf("body %s, err %v", b, err)
	}
}
