package harness_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/factorystore"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
)

// fakeForge is a code host with one repository: tests move its CI by calling push and set.
type fakeForge struct {
	mu           sync.Mutex
	prs          map[int]*factory.PRInfo
	checks       []factory.Check
	log          string
	merged       string // head sha the PR was merged at
	comments     []string
	reviews      []string
	reviewBodies []string
	opened       int
	mergeable    *bool
	headSHA      string // the head the next OpenPR reports (set by push before a PR exists)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func newFakeForge() *fakeForge {
	t := true
	return &fakeForge{prs: map[int]*factory.PRInfo{}, mergeable: &t}
}

func (f *fakeForge) OpenPR(_ context.Context, in factory.OpenPR) (*factory.PRInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened++
	pr := &factory.PRInfo{Number: 7, URL: "https://github.com/" + in.Repo + "/pull/7", Title: in.Title, State: "open",
		Base: in.Base, Head: in.Head, HeadSHA: cmpOr(f.headSHA, "sha-1"), Mergeable: f.mergeable}
	f.prs[7] = pr
	return pr, nil
}

func (f *fakeForge) GetPR(_ context.Context, _ string, n int) (*factory.PRInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pr, ok := f.prs[n]
	if !ok {
		return nil, fmt.Errorf("no pull request %d", n)
	}
	c := *pr
	c.Mergeable = f.mergeable
	if f.merged != "" {
		c.State = "merged"
	}
	return &c, nil
}

func (f *fakeForge) Diff(context.Context, string, int) (string, error) {
	return "diff --git a/app.go b/app.go\n--- a/app.go\n+++ b/app.go\n@@ -1 +1 @@\n-var x = 1\n+var x = 2\n" +
		"diff --git a/app_test.go b/app_test.go\n--- a/app_test.go\n+++ b/app_test.go\n@@ -1 +1 @@\n-// t\n+// t2\n", nil
}

func (f *fakeForge) Checks(context.Context, string, string) ([]factory.Check, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]factory.Check(nil), f.checks...), nil
}

func (f *fakeForge) FailureLog(context.Context, string, factory.Check) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.log, nil
}

func (f *fakeForge) Comments(context.Context, string, int) ([]factory.Comment, error) {
	return nil, nil
}

func (f *fakeForge) Comment(_ context.Context, _ string, _ int, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments = append(f.comments, body)
	return nil
}

func (f *fakeForge) Review(_ context.Context, _ string, _ int, event, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reviews = append(f.reviews, event)
	f.reviewBodies = append(f.reviewBodies, body)
	return nil
}

func (f *fakeForge) Merge(_ context.Context, _ string, n int, sha string, _ factory.MergeMethod) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sha != f.prs[n].HeadSHA {
		return fmt.Errorf("head moved")
	}
	f.merged = sha
	return nil
}

func (f *fakeForge) DefaultBranch(context.Context, string) (string, error) { return "main", nil }
func (f *fakeForge) ProtectedBranches(context.Context, string) ([]string, error) {
	return []string{"main"}, nil
}

// failCI makes the head's CI red with a log.
func (f *fakeForge) failCI(log string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks = []factory.Check{{Name: "build", State: factory.CheckFailed, Detail: "go test failed"}}
	f.log = log
}

// push is the agent pushing a fix: a new head with green CI.
func (f *fakeForge) push(sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.headSHA = sha
	if pr := f.prs[7]; pr != nil {
		pr.HeadSHA = sha
	}
	f.checks = []factory.Check{{Name: "build", State: factory.CheckPassed}}
}

func (f *fakeForge) mergedAt() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.merged
}

type fixedAuth struct{}

func (fixedAuth) Token(context.Context, string) (string, error) { return "tok", nil }

type escalations struct {
	mu      sync.Mutex
	reasons []string
}

func (e *escalations) Escalate(_ context.Context, _ factory.Task, reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reasons = append(e.reasons, reason)
}

func (e *escalations) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.reasons...)
}

// prScript scripts the author agent and the reviewer in one model. The agent opens its pull request on every
// attempt; on the attempt that received CI feedback it first "pushes a fix" (the forge's CI turns green).
// reviewJSON is the reviewer's answer; reviews counts its calls and prompts keeps what each attempt was told.
type prScript struct {
	forge      *fakeForge
	reviewJSON func(call int) string

	mu      sync.Mutex
	reviews int
	fixes   int
	prompts []string
}

func (s *prScript) model() *llmtest.Model {
	return llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if len(c.Messages) > 0 && strings.Contains(c.Messages[0].Content, "independent code reviewer") {
			s.mu.Lock()
			s.reviews++
			n := s.reviews
			s.mu.Unlock()
			return llmtest.Turn{Chunks: llmtest.Text(s.reviewJSON(n))}
		}
		// A real agent works in the repository (a command), then publishes with open_pr, then reports.
		last := c.Messages[len(c.Messages)-1]
		if last.Role == ai.RoleTool && strings.Contains(last.Content, "exitCode") {
			return llmtest.Turn{Chunks: []*ai.Message{ai.AssistantMessage("", []ai.ToolCall{{ID: "open", Type: "function",
				Function: ai.FunctionCall{Name: "open_pr", Arguments: `{"title":"Fix the thing","body":"It fixes the thing."}`}}})}}
		}
		if last.Role == ai.RoleTool {
			return llmtest.Turn{Chunks: llmtest.Text("The pull request is open.")}
		}
		text := lastUserText(c)
		s.mu.Lock()
		s.prompts = append(s.prompts, text)
		s.mu.Unlock()
		// The model is called twice per attempt (open_pr, then the final answer): push a fix once per feedback.
		if strings.Contains(text, "CI failed on pull request") || strings.Contains(text, "automated review") {
			s.mu.Lock()
			s.fixes++
			sha := fmt.Sprintf("sha-%d", s.fixes+1)
			s.mu.Unlock()
			s.forge.push(sha)
		}
		return llmtest.Turn{Chunks: []*ai.Message{ai.AssistantMessage("", []ai.ToolCall{{ID: "work", Type: "function",
			Function: ai.FunctionCall{Name: "execute_command", Arguments: `{"command":"go build ./..."}`}}})}}
	})
}

func (s *prScript) promptsCopy() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.prompts...)
}

const approveJSON = `{"approve":true,"summary":"Correct and tested.","findings":[]}`

func prFactory(t *testing.T, s *prScript, mod func(*harness.FactoryOptions)) (*harness.Harness, factory.Store, context.Context, *escalations) {
	t.Helper()
	esc := &escalations{}
	o := harness.FactoryOptions{
		Run: true, Forge: s.forge, Auth: fixedAuth{}, ReviewModel: "reviewer-model",
		PollEvery: 10 * time.Millisecond, LoopEvery: 20 * time.Millisecond, Settle: time.Millisecond,
		Backoff: func(int) time.Duration { return 0 }, Escalator: esc,
	}
	if mod != nil {
		mod(&o)
	}
	st := factorystore.NewMem(nil)
	o.Store = st
	// The host pushes on open_pr, so the task's thread needs a sandbox whose checkout is "two commits ahead";
	// every other command (the clone, the push itself) succeeds without output.
	be := sandboxtest.NewBackend()
	be.OnCreate = func(sb *sandboxtest.Sandbox) {
		sb.SetExec(func(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
			if strings.Contains(req.Command, "rev-list --count") {
				if req.OnStdout != nil {
					req.OnStdout("dirty=0\nahead=2\n")
				}
			}
			return sandbox.ExecResult{}, nil
		})
	}
	h, ctx := facade(t, map[string]string{"LLM_DEFAULT_MODEL": "gpt-test"}, s.model(), func(ho *harness.Options) {
		ho.Backend = be
		ho.Features = append(harness.ProfileCoding(), harness.FactoryFeature(o))
	})
	return h, st, ctx, esc
}

func TestFactoryDeliversAPullRequestThroughCIFeedbackAndReview(t *testing.T) {
	// Arrange: CI is red on the first head; the reviewer approves the fixed one.
	forge := newFakeForge()
	forge.failCI("app.go:3: undefined: foo")
	s := &prScript{forge: forge, reviewJSON: func(int) string { return approveJSON }}
	h, st, ctx, _ := prFactory(t, s, nil)
	f := h.FactoryOf()

	// Act
	task, _, err := f.Tasks.Create(ctx, factory.Task{Owner: "alice", Title: "Fix the thing", Spec: "Make x equal 2.",
		Repo: "acme/app", BaseBranch: "main", AutoMerge: true})
	if err != nil {
		t.Fatal(err)
	}
	done := waitTask(ctx, t, st, task.ID, factory.StateDone)

	// Assert: two attempts, the second one told what CI said, merged at the fixed head after one review.
	if done.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2 (one CI round)", done.Attempts)
	}
	if got := forge.mergedAt(); got != "sha-2" {
		t.Errorf("merged at %q, want the fixed head sha-2", got)
	}
	prompts := s.promptsCopy()
	if len(prompts) != 2 || !strings.Contains(prompts[1], "undefined: foo") {
		t.Errorf("the second attempt was not given the CI log: %q", prompts)
	}
	if s.reviews != 1 {
		t.Errorf("reviewer called %d times, want once for the green head", s.reviews)
	}
	if !strings.Contains(done.Result, "merged") {
		t.Errorf("Result = %q, want the merge", done.Result)
	}
	evs, _ := st.ListEvents(ctx, task.ID, 0)
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind+":"+fmt.Sprint(e.Data["state"])+fmt.Sprint(e.Data["verdict"]))
	}
	joined := strings.Join(kinds, " ")
	for _, want := range []string{"ci:failed", "ci:passed", "review:<nil>approve"} {
		if !strings.Contains(joined, want) {
			t.Errorf("history lacks %q: %s", want, joined)
		}
	}
	if len(forge.reviews) == 0 {
		t.Error("the approval was not posted on the pull request")
	}
}

func TestFactoryHandsAGreenApprovedPullRequestToAHumanWithoutAutoMerge(t *testing.T) {
	// Arrange
	forge := newFakeForge()
	forge.push("sha-1")
	s := &prScript{forge: forge, reviewJSON: func(int) string { return approveJSON }}
	h, st, ctx, esc := prFactory(t, s, nil)
	f := h.FactoryOf()

	// Act
	task, _, _ := f.Tasks.Create(ctx, factory.Task{Owner: "alice", Title: "Fix", Spec: "x", Repo: "acme/app", BaseBranch: "main"})
	blocked := waitTask(ctx, t, st, task.ID, factory.StateBlocked)

	// Assert: nothing merged, a human was told, and merging by hand finishes the task.
	if forge.mergedAt() != "" {
		t.Error("merged without AutoMerge")
	}
	if !strings.Contains(blocked.BlockedReason, "merge") {
		t.Errorf("BlockedReason = %q, want ready-to-merge", blocked.BlockedReason)
	}
	eventually(ctx, t, "the escalation", func() bool { return len(esc.all()) == 1 })
	forge.mu.Lock()
	forge.merged = "sha-1" // a human clicks merge
	forge.mu.Unlock()
	waitTask(ctx, t, st, task.ID, factory.StateDone)
}

func TestFactorySendsReviewerFindingsBackAsTheNextAttempt(t *testing.T) {
	// Arrange: the reviewer rejects the first head with a finding, then approves.
	forge := newFakeForge()
	forge.push("sha-1")
	s := &prScript{forge: forge, reviewJSON: func(n int) string {
		if n == 1 {
			return `{"approve":false,"summary":"Not safe.","findings":[{"severity":"major","file":"app.go","line":3,"issue":"unchecked error"}]}`
		}
		return approveJSON
	}}
	h, st, ctx, _ := prFactory(t, s, nil)
	f := h.FactoryOf()

	// Act
	task, _, _ := f.Tasks.Create(ctx, factory.Task{Owner: "alice", Title: "Fix", Spec: "x", Repo: "acme/app", BaseBranch: "main", AutoMerge: true})
	done := waitTask(ctx, t, st, task.ID, factory.StateDone)

	// Assert
	prompts := s.promptsCopy()
	if done.Attempts != 2 || len(prompts) != 2 || !strings.Contains(prompts[1], "unchecked error") {
		t.Errorf("attempts=%d prompts=%q; want the finding delivered to attempt 2", done.Attempts, prompts)
	}
	if len(forge.comments) == 0 || !strings.Contains(forge.comments[0], "changes needed") {
		t.Errorf("the findings were not posted on the pull request: %q", forge.comments)
	}
}

func TestFactoryReviewsWithTheDefaultModelWhenNoReviewerIsNamedAndSaysSo(t *testing.T) {
	// Arrange: no ReviewModel. The single model both writes and reviews, which the facade allows.
	forge := newFakeForge()
	forge.push("sha-1")
	s := &prScript{forge: forge, reviewJSON: func(int) string { return approveJSON }}
	h, st, ctx, _ := prFactory(t, s, func(o *harness.FactoryOptions) { o.ReviewModel = "" })
	f := h.FactoryOf()

	// Act
	task, _, err := f.Tasks.Create(ctx, factory.Task{Owner: "alice", Title: "Fix", Spec: "x", Repo: "acme/app", BaseBranch: "main", AutoMerge: true})
	if err != nil {
		t.Fatal(err)
	}
	waitTask(ctx, t, st, task.ID, factory.StateDone)

	// Assert: it was reviewed and merged, and the approval posted on the pull request admits the review was not
	// independent: a human deciding whether to trust it must be able to see that.
	if s.reviews != 1 || forge.mergedAt() == "" {
		t.Errorf("reviews = %d, merged at %q; want one review and a merge", s.reviews, forge.mergedAt())
	}
	forge.mu.Lock()
	defer forge.mu.Unlock()
	if len(forge.reviewBodies) != 1 || !strings.Contains(forge.reviewBodies[0], "same model that wrote the change") {
		t.Errorf("posted approvals = %q; want one that says the review is not independent", forge.reviewBodies)
	}
}
