package forge_test

import (
	"context"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/engine/tools"
	"github.com/7ar-chat/harness/features/factory/forge"
)

// sessionEnv is a chat session of a project with a repository: no task, no thread info.
func sessionEnv(t *testing.T, adoptable bool) *toolEnv {
	t.Helper()
	st := newStore(newClock())
	g := newFakeGitHub(t)
	c := &ctrl{store: st}
	adopt := func(_ context.Context, threadID string) (*forge.Adoption, error) {
		if !adoptable || threadID != "sess" {
			return nil, nil
		}
		return &forge.Adoption{
			Info: factory.ThreadInfo{Login: "u", Repo: repo, BaseBranch: "main", Branch: "har/abc"},
			Task: factory.Task{ProjectID: "p1", Owner: "u", Repo: repo, BaseBranch: "main", Branch: "har/abc"},
		}, nil
	}
	return &toolEnv{g: g, store: st, c: c,
		p: forge.NewTools(forge.ToolsOptions{Forge: g.client(), Store: st, Controller: c, Adopt: adopt})}
}

func TestASessionOfARepositoryProjectGetsThePullRequestTools(t *testing.T) {
	// Arrange
	e := sessionEnv(t, true)
	// Act / Assert
	if got := e.tools(t, "sess"); len(got) != 4 {
		t.Fatalf("session got %d tools, want 4", len(got))
	}
	if got := e.tools(t, "elsewhere"); len(got) != 0 {
		t.Fatalf("another thread got %d tools, want 0", len(got))
	}
	if got := sessionEnv(t, false).tools(t, "sess"); len(got) != 0 {
		t.Fatalf("a session that cannot be adopted got %d tools, want 0", len(got))
	}
}

func TestOpenPRFromASessionCreatesATaskInReviewBoundToTheThread(t *testing.T) {
	// Arrange
	e := sessionEnv(t, true)
	open := e.tools(t, "sess")["open_pr"]
	// Act
	out, err := call(t, open, `{"title":"Fix login","body":"Handles the empty password"}`)
	// Assert
	if err != nil || !strings.Contains(out, "#7") {
		t.Fatalf("out = %q, err = %v", out, err)
	}
	ts, _ := e.store.ListTasks(context.Background(), factory.Filter{ThreadID: "sess"})
	if len(ts) != 1 {
		t.Fatalf("tasks = %+v", ts)
	}
	got := ts[0]
	if got.State != factory.StateReview || got.PR.Number != 7 || got.Source.Kind != factory.SourceSession || got.Title != "Fix login" || got.ProjectID != "p1" {
		t.Fatalf("task = %+v", got)
	}
}

func TestOpenPRFromASessionTwiceReusesItsTask(t *testing.T) {
	// Arrange
	e := sessionEnv(t, true)
	call(t, e.tools(t, "sess")["open_pr"], `{"title":"Fix login","body":"b"}`)
	// Act
	out, err := call(t, e.tools(t, "sess")["open_pr"], `{"title":"Fix login again","body":"b"}`)
	// Assert
	if err != nil || !strings.Contains(out, "already open") {
		t.Fatalf("out = %q, err = %v", out, err)
	}
	if ts, _ := e.store.ListTasks(context.Background(), factory.Filter{ThreadID: "sess"}); len(ts) != 1 {
		t.Fatalf("tasks = %d, want 1", len(ts))
	}
}

func TestReadToolsOfASessionWithoutAPullRequestSayThereIsNone(t *testing.T) {
	// Arrange
	e := sessionEnv(t, true)
	// Act
	_, err := call(t, e.tools(t, "sess")["pr_status"], `{}`)
	// Assert
	if err == nil || !strings.Contains(err.Error(), "no pull request yet") {
		t.Fatalf("err = %v", err)
	}
	var _ tools.Provider = e.p
}
