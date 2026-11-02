package forge_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/engine/tools"
	"github.com/7ar-chat/harness/features/factory/forge"
)

// toolEnv is a task in running state with its thread, a fake GitHub and the tools of that thread.
type toolEnv struct {
	g     *fakeGitHub
	store factory.Store
	c     *ctrl
	task  *factory.Task
	p     tools.Provider
}

func newToolEnv(t *testing.T, mutate func(ti *factory.ThreadInfo)) *toolEnv {
	t.Helper()
	clk := newClock()
	st := newStore(clk)
	g := newFakeGitHub(t)
	task, _, err := st.CreateTask(context.Background(), factory.Task{Title: "T", Owner: "u", Repo: repo, BaseBranch: "main", Branch: "har/abc", State: factory.StateReady})
	if err != nil {
		t.Fatal(err)
	}
	running := factory.StateRunning
	if _, err := st.UpdateTask(context.Background(), task.ID, factory.Patch{State: &running}); err != nil {
		t.Fatal(err)
	}
	ti := factory.ThreadInfo{ThreadID: "th1", TaskID: task.ID, Repo: repo, BaseBranch: "main", Branch: "har/abc"}
	if mutate != nil {
		mutate(&ti)
	}
	if err := st.SetThreadInfo(context.Background(), ti); err != nil {
		t.Fatal(err)
	}
	c := &ctrl{store: st}
	return &toolEnv{g: g, store: st, c: c, task: task,
		p: forge.NewTools(forge.ToolsOptions{Forge: g.client(), Store: st, Controller: c})}
}

// withPush rebuilds the provider with a Push hook; the hook records its calls in order with the GitHub requests.
func (e *toolEnv) withPush(push func(ctx context.Context, threadID string, protected []string) (forge.PushResult, error)) {
	e.p = forge.NewTools(forge.ToolsOptions{Forge: e.g.client(), Store: e.store, Controller: e.c, Push: push})
}

func (e *toolEnv) tools(t *testing.T, thread string) map[string]ai.InvokableTool {
	t.Helper()
	ts, _, err := e.p.Tools(context.Background(), tools.RunContext{ThreadID: thread})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]ai.InvokableTool{}
	for _, bt := range ts {
		info, _ := bt.Info(context.Background())
		out[info.Name] = bt.(ai.InvokableTool)
	}
	return out
}

func call(t *testing.T, tool ai.InvokableTool, args string) (string, error) {
	t.Helper()
	return tool.InvokableRun(context.Background(), args)
}

func TestToolsOnlyForRepositoryTaskThreads(t *testing.T) {
	tests := []struct {
		name   string
		thread string
		mutate func(ti *factory.ThreadInfo)
		want   int
	}{
		{"task thread", "th1", nil, 4},
		{"unknown thread", "other", nil, 0},
		{"thread without a task", "th1", func(ti *factory.ThreadInfo) { ti.TaskID = "" }, 0},
		{"thread without a repository", "th1", func(ti *factory.ThreadInfo) { ti.Repo = "" }, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			e := newToolEnv(t, tc.mutate)
			// Act
			got := e.tools(t, tc.thread)
			// Assert
			if len(got) != tc.want {
				t.Fatalf("got %d tools, want %d", len(got), tc.want)
			}
		})
	}
	if forge.NewTools(forge.ToolsOptions{}).Name() != "forge" {
		t.Fatal("provider name must be forge")
	}
}

func TestOpenPRMovesTaskToReview(t *testing.T) {
	// Arrange
	e := newToolEnv(t, nil)
	open := e.tools(t, "th1")["open_pr"]
	// Act
	out, err := call(t, open, `{"title":"Add feature","body":"Does X","draft":false}`)
	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "#7") || !strings.Contains(out, "har/abc -> main") {
		t.Fatalf("out = %q", out)
	}
	sent := e.g.body("POST /repos/acme/app/pulls")
	if len(sent) != 1 || sent[0]["head"] != "har/abc" || sent[0]["base"] != "main" || sent[0]["title"] != "Add feature" {
		t.Fatalf("request = %v", sent)
	}
	got, _ := e.store.GetTask(context.Background(), e.task.ID)
	if got.State != factory.StateReview || got.PR.Number != 7 || got.PR.HeadSHA != sha1 {
		t.Fatalf("task = %+v", got)
	}
}

func TestOpenPRIsIdempotent(t *testing.T) {
	// Arrange
	e := newToolEnv(t, nil)
	open := e.tools(t, "th1")["open_pr"]
	first, err := call(t, open, `{"title":"A","body":"b"}`)
	if err != nil {
		t.Fatal(err)
	}
	// Act
	second, err := call(t, open, `{"title":"A again","body":"b"}`)
	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if n := e.g.count("POST /repos/acme/app/pulls"); n != 1 {
		t.Fatalf("pull requests created = %d, want 1", n)
	}
	if !strings.Contains(second, "already open") || !strings.Contains(second, "#7") {
		t.Fatalf("second = %q (first = %q)", second, first)
	}
}

func TestOpenPRAfterARetryPutsTheTaskBackInReview(t *testing.T) {
	// Arrange: the PR was opened, CI failed, the loop resumed the task and a new attempt is running.
	e := newToolEnv(t, nil)
	open := e.tools(t, "th1")["open_pr"]
	if _, err := call(t, open, `{"title":"A"}`); err != nil {
		t.Fatal(err)
	}
	running := factory.StateRunning
	if _, err := e.store.UpdateTask(context.Background(), e.task.ID, factory.Patch{State: &running}); err != nil {
		t.Fatal(err)
	}
	e.g.set(func() { e.g.headSHA = sha2 })
	// Act
	out, err := call(t, open, `{"title":"A"}`)
	// Assert
	if err != nil || !strings.Contains(out, "already open") {
		t.Fatalf("out = %q, err = %v", out, err)
	}
	got, _ := e.store.GetTask(context.Background(), e.task.ID)
	if got.State != factory.StateReview || got.PR.HeadSHA != sha2 {
		t.Fatalf("task = %+v, want review at the pushed head", got)
	}
	if n := e.g.count("POST /repos/acme/app/pulls"); n != 1 {
		t.Fatalf("pull requests created = %d", n)
	}
}

func TestOpenPRRecoversFromExistingPullRequest(t *testing.T) {
	// Arrange: the task has no PR recorded but GitHub already has one for the branch (a crash between open and record).
	e := newToolEnv(t, nil)
	e.g.set(func() {
		e.g.createStat = 422
		e.g.listPRs = []map[string]any{{"number": 7}}
	})
	open := e.tools(t, "th1")["open_pr"]
	// Act
	out, err := call(t, open, `{"title":"A","body":"b"}`)
	// Assert
	if err != nil {
		t.Fatal(err)
	}
	got, _ := e.store.GetTask(context.Background(), e.task.ID)
	if !strings.Contains(out, "#7") || got.State != factory.StateReview || got.PR.Number != 7 {
		t.Fatalf("out = %q, task = %+v", out, got)
	}
}

func TestOpenPRRefusals(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(ti *factory.ThreadInfo)
		setup  func(g *fakeGitHub)
		args   string
		want   string
	}{
		{"protected head", func(ti *factory.ThreadInfo) { ti.Branch = "release" }, func(g *fakeGitHub) { g.protected = []string{"main", "release"} },
			`{"title":"x"}`, "protected branch release"},
		{"head equals base", func(ti *factory.ThreadInfo) { ti.Branch = "main" }, nil, `{"title":"x"}`, "into itself"},
		{"empty title", nil, nil, `{"title":"  "}`, "title"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			e := newToolEnv(t, tc.mutate)
			if tc.setup != nil {
				e.g.set(func() { tc.setup(e.g) })
			}
			// Act
			_, err := call(t, e.tools(t, "th1")["open_pr"], tc.args)
			// Assert
			var ue *tools.UserError
			if !errors.As(err, &ue) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a tool error containing %q", err, tc.want)
			}
			if n := e.g.count("POST /repos/acme/app/pulls"); n != 0 {
				t.Fatalf("a pull request was opened")
			}
			if got, _ := e.store.GetTask(context.Background(), e.task.ID); got.State != factory.StateRunning {
				t.Fatalf("task state = %s", got.State)
			}
		})
	}
}

func TestOpenPRUsesDefaultBranchWhenBaseUnset(t *testing.T) {
	// Arrange
	e := newToolEnv(t, func(ti *factory.ThreadInfo) { ti.BaseBranch = "" })
	// Act
	_, err := call(t, e.tools(t, "th1")["open_pr"], `{"title":"x"}`)
	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if sent := e.g.body("POST /repos/acme/app/pulls"); sent[0]["base"] != "main" {
		t.Fatalf("base = %v, want the default branch main", sent[0]["base"])
	}
}

func TestOpenPRGitHubErrorIsAToolError(t *testing.T) {
	// Arrange
	e := newToolEnv(t, nil)
	e.g.set(func() { e.g.createStat = 422 })
	// Act: 422 and the finder finds nothing.
	_, err := call(t, e.tools(t, "th1")["open_pr"], `{"title":"x"}`)
	// Assert
	var ue *tools.UserError
	if !errors.As(err, &ue) || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v", err)
	}
}

func TestStatusLogAndCommentsNeedAPullRequest(t *testing.T) {
	e := newToolEnv(t, nil)
	ts := e.tools(t, "th1")
	for _, name := range []string{"pr_status", "ci_log", "pr_comments"} {
		_, err := call(t, ts[name], `{}`)
		var ue *tools.UserError
		if !errors.As(err, &ue) || !strings.Contains(err.Error(), "open_pr") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestPRStatusSummarizesChecks(t *testing.T) {
	// Arrange
	e := newToolEnv(t, nil)
	if _, err := call(t, e.tools(t, "th1")["open_pr"], `{"title":"x"}`); err != nil {
		t.Fatal(err)
	}
	e.g.set(func() {
		e.g.runs[sha1] = []map[string]any{
			run("build", "completed", "success", ""), run("test", "completed", "failure", "11"), run("lint", "in_progress", "", ""),
		}
	})
	// Act
	out, err := call(t, e.tools(t, "th1")["pr_status"], `{}`)
	// Assert
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"State: open; mergeable", "1 passed, 1 failed, 1 pending", "- test: failed", "- lint: pending"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "- build") {
		t.Errorf("green checks are summarized, not listed:\n%s", out)
	}
}

func TestPRStatusConflict(t *testing.T) {
	// Arrange
	e := newToolEnv(t, nil)
	if _, err := call(t, e.tools(t, "th1")["open_pr"], `{"title":"x"}`); err != nil {
		t.Fatal(err)
	}
	no := false
	e.g.set(func() { e.g.mergeable = &no })
	// Act
	out, err := call(t, e.tools(t, "th1")["pr_status"], `{}`)
	// Assert
	if err != nil || !strings.Contains(out, "NOT mergeable") || !strings.Contains(out, "rebase onto origin/main") {
		t.Fatalf("out = %q, err = %v", out, err)
	}
}

func TestCILog(t *testing.T) {
	// Arrange
	e := newToolEnv(t, nil)
	if _, err := call(t, e.tools(t, "th1")["open_pr"], `{"title":"x"}`); err != nil {
		t.Fatal(err)
	}
	e.g.set(func() {
		e.g.runs[sha1] = []map[string]any{
			run("build", "completed", "success", "10"),
			run("test", "completed", "failure", "11"),
			run("vet", "completed", "failure", "12"),
			run("nolog", "completed", "failure", ""),
		}
		e.g.logs["11"] = "FAIL: TestA\n"
		e.g.logs["12"] = "vet: unused variable\n"
	})
	tool := e.tools(t, "th1")["ci_log"]
	tests := []struct {
		name, args, want string
		wantErr          bool
	}{
		{"first failing", `{}`, "FAIL: TestA", false},
		{"named", `{"check":"vet"}`, "vet: unused variable", false},
		{"named is case-insensitive", `{"check":"VET"}`, "vet: unused variable", false},
		{"unknown name lists the checks", `{"check":"zzz"}`, "build, test, vet, nolog", true},
		{"passing check", `{"check":"build"}`, "is passed, not failed", false},
		{"log unavailable", `{"check":"nolog"}`, "log is not available", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Act
			out, err := call(t, tool, tc.args)
			// Assert
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("err = %v, want %q", err, tc.want)
				}
				return
			}
			if err != nil || !strings.Contains(out, tc.want) {
				t.Fatalf("out = %q, err = %v, want %q", out, err, tc.want)
			}
		})
	}
}

func TestCILogWhenNothingFails(t *testing.T) {
	// Arrange
	e := newToolEnv(t, nil)
	if _, err := call(t, e.tools(t, "th1")["open_pr"], `{"title":"x"}`); err != nil {
		t.Fatal(err)
	}
	e.g.set(func() { e.g.runs[sha1] = []map[string]any{run("build", "completed", "success", "")} })
	// Act
	out, err := call(t, e.tools(t, "th1")["ci_log"], `{}`)
	// Assert
	if err != nil || !strings.Contains(out, "No check is failing") {
		t.Fatalf("out = %q, err = %v", out, err)
	}
}

func TestPRComments(t *testing.T) {
	// Arrange
	e := newToolEnv(t, nil)
	if _, err := call(t, e.tools(t, "th1")["open_pr"], `{"title":"x"}`); err != nil {
		t.Fatal(err)
	}
	e.g.set(func() {
		e.g.reviews = []map[string]any{{"user": user("bob"), "body": "add tests", "state": "CHANGES_REQUESTED"}}
		e.g.inline = []map[string]any{{"user": user("bob"), "body": "rename this", "path": "a.go", "line": 12}}
	})
	// Act
	out, err := call(t, e.tools(t, "th1")["pr_comments"], `{}`)
	// Assert
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"bob requested changes: add tests", "bob on a.go:12: rename this"} {
		if !strings.Contains(out, want) {
			t.Errorf("comments lack %q:\n%s", want, out)
		}
	}
}

func TestReadToolsAreReadOnly(t *testing.T) {
	e := newToolEnv(t, nil)
	for name, tool := range e.tools(t, "th1") {
		ro, _ := tool.(tools.ReadOnly)
		if got := ro != nil && ro.ReadOnly(); got != (name != "open_pr") {
			t.Errorf("%s read-only = %v", name, got)
		}
	}
}

func TestOpenPRPushesBeforeOpeningAndReportsIt(t *testing.T) {
	// Arrange
	e := newToolEnv(t, nil)
	var order []string
	var gotThread string
	var gotProtected []string
	e.g.set(func() { e.g.protected = []string{"main", "release/*"} })
	e.withPush(func(_ context.Context, thread string, protected []string) (forge.PushResult, error) {
		gotThread, gotProtected = thread, protected
		order = append(order, "push:"+strconv.Itoa(e.g.count("POST /repos/acme/app/pulls")))
		return forge.PushResult{Branch: "har/abc", Ahead: 3, Dirty: 2}, nil
	})
	// Act
	out, err := call(t, e.tools(t, "th1")["open_pr"], `{"title":"Add feature","body":"b"}`)
	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 1 || order[0] != "push:0" {
		t.Fatalf("the branch must be pushed before any pull request is created, order = %v", order)
	}
	if gotThread != "th1" || !slices.Equal(gotProtected, []string{"main", "release/*"}) {
		t.Fatalf("push called with thread %q protected %v", gotThread, gotProtected)
	}
	pushedAt := strings.Index(out, "Pushed har/abc (3 commit(s) ahead of main)")
	warnAt := strings.Index(out, "Warning: 2 uncommitted change(s) were not pushed")
	prAt := strings.Index(out, "Pull request opened: #7")
	if pushedAt != 0 || warnAt < pushedAt || prAt < warnAt {
		t.Fatalf("out = %q, want the push line, the warning, then the pull request", out)
	}
	if got, _ := e.store.GetTask(context.Background(), e.task.ID); got.State != factory.StateReview {
		t.Fatalf("task state = %s", got.State)
	}
}

func TestOpenPROmitsTheWarningWhenNothingIsUncommitted(t *testing.T) {
	// Arrange
	e := newToolEnv(t, nil)
	e.withPush(func(context.Context, string, []string) (forge.PushResult, error) {
		return forge.PushResult{Branch: "har/abc", Ahead: 1}, nil
	})
	// Act
	out, err := call(t, e.tools(t, "th1")["open_pr"], `{"title":"x"}`)
	// Assert
	if err != nil || strings.Contains(out, "Warning") || !strings.HasPrefix(out, "Pushed har/abc (1 commit(s) ahead of main)\n") {
		t.Fatalf("out = %q, err = %v", out, err)
	}
}

func TestOpenPRPushFailureOpensNothingAndKeepsTheTaskRunning(t *testing.T) {
	// Arrange
	e := newToolEnv(t, nil)
	e.withPush(func(context.Context, string, []string) (forge.PushResult, error) {
		return forge.PushResult{}, errors.New("git push failed (exit 1): ! [rejected] stale info with " + token)
	})
	// Act
	_, err := call(t, e.tools(t, "th1")["open_pr"], `{"title":"x"}`)
	// Assert
	var ue *tools.UserError
	if !errors.As(err, &ue) || !strings.Contains(err.Error(), "pushing your branch failed") || !strings.Contains(err.Error(), "stale info") {
		t.Fatalf("err = %v, want a tool error quoting the push failure", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("the tool error leaks the token: %v", err)
	}
	if n := e.g.count("POST /repos/acme/app/pulls") + e.g.count("GET /repos/acme/app/pulls/7"); n != 0 {
		t.Fatalf("GitHub was asked about a pull request %d time(s) after a failed push", n)
	}
	if got, _ := e.store.GetTask(context.Background(), e.task.ID); got.State != factory.StateRunning || len(e.c.names()) != 0 {
		t.Fatalf("task state = %s, controller calls = %v", got.State, e.c.names())
	}
}

func TestOpenPRRefusalsHappenBeforeAnyPush(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(ti *factory.ThreadInfo)
		setup  func(g *fakeGitHub)
	}{
		{"protected head", func(ti *factory.ThreadInfo) { ti.Branch = "release" }, func(g *fakeGitHub) { g.protected = []string{"main", "release"} }},
		{"head equals base", func(ti *factory.ThreadInfo) { ti.Branch = "main" }, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			e := newToolEnv(t, tc.mutate)
			if tc.setup != nil {
				e.g.set(func() { tc.setup(e.g) })
			}
			pushed := false
			e.withPush(func(context.Context, string, []string) (forge.PushResult, error) {
				pushed = true
				return forge.PushResult{}, nil
			})
			// Act
			_, err := call(t, e.tools(t, "th1")["open_pr"], `{"title":"x"}`)
			// Assert
			if err == nil || pushed {
				t.Fatalf("err = %v, pushed = %v", err, pushed)
			}
		})
	}
}

func TestOpenPRSecondCallPushesAgainThenReusesThePullRequest(t *testing.T) {
	// Arrange
	e := newToolEnv(t, nil)
	pushes := 0
	e.withPush(func(context.Context, string, []string) (forge.PushResult, error) {
		pushes++
		return forge.PushResult{Branch: "har/abc", Ahead: pushes}, nil
	})
	open := e.tools(t, "th1")["open_pr"]
	if _, err := call(t, open, `{"title":"A"}`); err != nil {
		t.Fatal(err)
	}
	// Act
	second, err := call(t, open, `{"title":"A"}`)
	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if pushes != 2 {
		t.Fatalf("pushes = %d, want one per call", pushes)
	}
	if n := e.g.count("POST /repos/acme/app/pulls"); n != 1 {
		t.Fatalf("pull requests created = %d, want 1", n)
	}
	if !strings.HasPrefix(second, "Pushed har/abc (2 commit(s) ahead of main)\n") || !strings.Contains(second, "Pull request already open: #7") {
		t.Fatalf("second = %q", second)
	}
}

func TestOpenPRWithoutAPushHookDoesNotPush(t *testing.T) {
	// Arrange: the default environment has no Push hook (a host whose agent pushes itself).
	e := newToolEnv(t, nil)
	// Act
	out, err := call(t, e.tools(t, "th1")["open_pr"], `{"title":"x"}`)
	// Assert
	if err != nil || strings.Contains(out, "Pushed") || !strings.HasPrefix(out, "Pull request opened: #7") {
		t.Fatalf("out = %q, err = %v", out, err)
	}
}

func TestOpenPRDescribesThatItPushes(t *testing.T) {
	// Arrange
	e := newToolEnv(t, nil)
	ts, _, err := e.p.Tools(context.Background(), tools.RunContext{ThreadID: "th1"})
	if err != nil {
		t.Fatal(err)
	}
	// Act
	var desc string
	for _, bt := range ts {
		if info, _ := bt.Info(context.Background()); info.Name == "open_pr" {
			desc = info.Desc
		}
	}
	// Assert
	for _, want := range []string{"it pushes your branch for you (you have no push credentials)", "Calling it again after more commits pushes them."} {
		if !strings.Contains(desc, want) {
			t.Errorf("description lacks %q:\n%s", want, desc)
		}
	}
	if strings.Contains(desc, "git push") {
		t.Errorf("description still tells the agent to push:\n%s", desc)
	}
}

func TestOpenPRForgeLookupFailuresAreToolErrorsAndNeverPush(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(ti *factory.ThreadInfo)
		key    string
		want   string
	}{
		{"protected branches unreadable", nil, "GET /repos/acme/app/branches", "which branches are protected"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			e := newToolEnv(t, tc.mutate)
			e.g.set(func() {
				e.g.failNext[tc.key] = []failure{{status: 500, message: "boom"}, {status: 500, message: "boom"}, {status: 500, message: "boom"}, {status: 500, message: "boom"}}
			})
			pushed := false
			e.withPush(func(context.Context, string, []string) (forge.PushResult, error) {
				pushed = true
				return forge.PushResult{}, nil
			})
			// Act
			_, err := call(t, e.tools(t, "th1")["open_pr"], `{"title":"x"}`)
			// Assert
			var ue *tools.UserError
			if !errors.As(err, &ue) || !strings.Contains(err.Error(), tc.want) || pushed {
				t.Fatalf("err = %v, pushed = %v", err, pushed)
			}
		})
	}
}

func TestOpenPRToolErrorsForAThreadWithoutBranchOrTask(t *testing.T) {
	// Arrange: the task and the thread both lack a branch.
	e := newToolEnv(t, func(ti *factory.ThreadInfo) { ti.Branch = "" })
	branchless, _, err := e.store.CreateTask(context.Background(), factory.Task{Title: "B", Owner: "u", Repo: repo, BaseBranch: "main", State: factory.StateReady})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.SetThreadInfo(context.Background(), factory.ThreadInfo{ThreadID: "th1", TaskID: branchless.ID, Repo: repo, BaseBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	// Act
	_, err = call(t, e.tools(t, "th1")["open_pr"], `{"title":"x"}`)
	// Assert
	var ue *tools.UserError
	if !errors.As(err, &ue) || !strings.Contains(err.Error(), "no working branch") {
		t.Fatalf("err = %v", err)
	}
}

type reviewFails struct{ *ctrl }

func (reviewFails) Review(context.Context, string, factory.PullRequest) error {
	return errors.New("store down")
}

func TestOpenPRReportsAControllerFailureAfterThePush(t *testing.T) {
	// Arrange
	e := newToolEnv(t, nil)
	pushes := 0
	e.p = forge.NewTools(forge.ToolsOptions{Forge: e.g.client(), Store: e.store, Controller: reviewFails{e.c},
		Push: func(context.Context, string, []string) (forge.PushResult, error) {
			pushes++
			return forge.PushResult{Branch: "har/abc", Ahead: 1}, nil
		}})
	// Act
	_, err := call(t, e.tools(t, "th1")["open_pr"], `{"title":"x"}`)
	// Assert
	if err == nil || !strings.Contains(err.Error(), "move task") || pushes != 1 {
		t.Fatalf("err = %v, pushes = %d", err, pushes)
	}
}
