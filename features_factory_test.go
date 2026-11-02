package harness_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/factorystore"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/modelinfo"
)

// factoryHarness builds a harness with the Factory feature (no forge: tasks end with the agent's answer).
func factoryHarness(t *testing.T, model *llmtest.Model, o harness.FactoryOptions) (*harness.Harness, factory.Store, context.Context) {
	t.Helper()
	st := factorystore.NewMem(nil)
	o.Store = st
	h, ctx := facade(t, map[string]string{"LLM_DEFAULT_MODEL": "gpt-test"}, model, func(ho *harness.Options) {
		ho.Features = append(harness.ProfileMinimal(), harness.FactoryFeature(o))
	})
	return h, st, ctx
}

func waitTask(ctx context.Context, t *testing.T, st factory.Store, id string, want factory.State) *factory.Task {
	t.Helper()
	var got *factory.Task
	eventually(ctx, t, "task "+id+" to be "+string(want), func() bool {
		var err error
		if got, err = st.GetTask(ctx, id); err != nil {
			t.Fatal(err)
		}
		return got.State == want
	})
	return got
}

func TestFactoryRunsATaskToDoneAndMetersItsSpend(t *testing.T) {
	// Arrange
	model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		return llmtest.Turn{Chunks: []*ai.Message{llmtest.WithUsage(ai.AssistantMessage("report: the answer is 42", nil), 120, 30)}}
	})
	h, st, ctx := factoryHarness(t, model, harness.FactoryOptions{
		Run: true, PollEvery: 10 * time.Millisecond,
		Price: func(string) (modelinfo.Info, bool) {
			return modelinfo.Info{Priced: true, InputPerToken: 1e-6, OutputPerToken: 2e-6}, true
		},
	})
	f := h.FactoryOf()
	if f == nil {
		t.Fatal("the Factory feature published no service")
	}

	// Act
	task, created, err := f.Tasks.Create(ctx, factory.Task{Owner: "alice", Title: "Find the answer", Spec: "What is 6 x 7?"})
	if err != nil || !created {
		t.Fatalf("Create = %v, %v", created, err)
	}
	f.Tasks.Kick()
	done := waitTask(ctx, t, st, task.ID, factory.StateDone)

	// Assert
	if !strings.Contains(done.Result, "42") {
		t.Errorf("Result = %q, want the agent's final answer", done.Result)
	}
	if done.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", done.Attempts)
	}
	eventually(ctx, t, "the run's spend to be booked", func() bool {
		cur, _ := st.GetTask(ctx, task.ID)
		return cur.Spent.Runs == 1
	})
	cur, _ := st.GetTask(ctx, task.ID)
	if cur.Spent.Tokens == 0 {
		t.Errorf("Spent = %+v, want the run's tokens counted", cur.Spent)
	}
	ti, err := st.GetThreadInfo(ctx, cur.ThreadID)
	if err != nil || ti.TaskID != task.ID || ti.Policy != factory.PolicySandboxed {
		t.Errorf("thread info = %+v, %v; want the task id and the default policy", ti, err)
	}
}

func TestFactoryBlocksATaskWhoseRunsKeepFailingTheSameWay(t *testing.T) {
	// Arrange: every call fails with the same provider error, so attempts fingerprint alike.
	model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		return llmtest.Turn{Err: errBoom}
	})
	h, st, ctx := factoryHarness(t, model, harness.FactoryOptions{Run: true, MaxAttempts: 5, PollEvery: 10 * time.Millisecond,
		Backoff: func(int) time.Duration { return 0 }})
	f := h.FactoryOf()

	// Act
	task, _, err := f.Tasks.Create(ctx, factory.Task{Owner: "alice", Title: "Doomed", Spec: "anything",
		NotBefore: time.Time{}})
	if err != nil {
		t.Fatal(err)
	}
	blocked := waitTask(ctx, t, st, task.ID, factory.StateBlocked)

	// Assert: it stopped on the repetition, well before the 5 attempts.
	if blocked.Attempts >= 5 {
		t.Errorf("Attempts = %d, want the repeated failure to stop it before MaxAttempts", blocked.Attempts)
	}
	if !strings.Contains(blocked.BlockedReason, "same way") {
		t.Errorf("BlockedReason = %q, want the repeated-failure reason", blocked.BlockedReason)
	}
}

func TestFactoryOptionsAreValidatedAtBuildTime(t *testing.T) {
	cases := map[string]harness.FactoryOptions{
		"no store":           {},
		"forge without auth": {Store: factorystore.NewMem(nil), Forge: nopForge{}},
	}
	for name, o := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			_, err := harness.New(harness.Options{
				Config: featureConfig(t, nil), Models: llmtest.Factory{M: echoModel()},
				Features: append(harness.ProfileMinimal(), harness.FactoryFeature(o)),
			})

			// Assert
			if err == nil {
				t.Fatal("New succeeded with invalid factory options")
			}
		})
	}
}

var errBoom = errorString("provider exploded: upstream 500")

type errorString string

func (e errorString) Error() string { return string(e) }

// nopForge is a Forge that is never called: the options tests only build the harness.
type nopForge struct{ factory.Forge }

func TestFactoryPolicyDeniesAForcePushInATaskRunButNotInOrdinaryChat(t *testing.T) {
	// Arrange: the model tries a force-push in whatever thread it runs.
	push := `{"command":"git push --force origin main"}`
	calls := 0
	model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		calls++
		if calls%2 == 1 {
			return llmtest.Turn{Chunks: []*ai.Message{ai.AssistantMessage("", []ai.ToolCall{{ID: fmt.Sprintf("c%d", calls), Type: "function",
				Function: ai.FunctionCall{Name: "execute_command", Arguments: push}}})}}
		}
		return llmtest.Turn{Chunks: llmtest.Text("done")}
	})
	h, st, ctx := factoryHarness(t, model, harness.FactoryOptions{Run: true, Policy: true, PollEvery: 10 * time.Millisecond})
	f := h.FactoryOf()

	// Act
	task, _, err := f.Tasks.Create(ctx, factory.Task{Owner: "alice", Title: "Ship it", Spec: "push to main", Policy: factory.PolicyAutonomous})
	if err != nil {
		t.Fatal(err)
	}
	waitTask(ctx, t, st, task.ID, factory.StateDone)

	// Assert: the run saw a refusal, and the audit trail names the rule.
	cur, _ := st.GetTask(ctx, task.ID)
	var refused bool
	for _, m := range messagesOf(ctx, t, h, cur.ThreadID) {
		if m.Role == "tool" && strings.Contains(strings.ToLower(m.Content), "not run") {
			refused = true
		}
	}
	if !refused {
		t.Errorf("the task run was not refused the force-push; messages: %q", contents(messagesOf(ctx, t, h, cur.ThreadID)))
	}
	evs, _ := st.ListEvents(ctx, task.ID, 0)
	var audited bool
	for _, e := range evs {
		if e.Kind == factory.EventPolicy {
			audited = true
		}
	}
	if !audited {
		t.Errorf("no %q event in the task history: %+v", factory.EventPolicy, evs)
	}
}

func TestFactorySupervisedTaskWaitsForAHumanApprovalThenContinues(t *testing.T) {
	// Arrange: the agent runs one command; the supervised policy must stop it for a human.
	cmd := `{"command":"go build ./..."}`
	model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if c.Messages[len(c.Messages)-1].Role == ai.RoleTool {
			return llmtest.Turn{Chunks: llmtest.Text("built it")}
		}
		return llmtest.Turn{Chunks: []*ai.Message{ai.AssistantMessage("", []ai.ToolCall{{ID: "c1", Type: "function",
			Function: ai.FunctionCall{Name: "execute_command", Arguments: cmd}}})}}
	})
	esc := &escalations{}
	h, st, ctx := factoryHarness(t, model, harness.FactoryOptions{Run: true, Policy: true, Escalator: esc,
		PollEvery: 10 * time.Millisecond, Backoff: func(int) time.Duration { return 0 }})
	f := h.FactoryOf()

	// Act: the run ends waiting for approval, which blocks the task and tells a human.
	task, _, err := f.Tasks.Create(ctx, factory.Task{Owner: "alice", Title: "Build", Spec: "build it", Policy: factory.PolicySupervised})
	if err != nil {
		t.Fatal(err)
	}
	blocked := waitTask(ctx, t, st, task.ID, factory.StateBlocked)

	// Assert (1): it waits for an approval and nobody had to retry or fail it.
	if !strings.Contains(blocked.BlockedReason, "approval") {
		t.Fatalf("BlockedReason = %q, want a wait for approval", blocked.BlockedReason)
	}
	if blocked.Attempts != 1 {
		t.Errorf("Attempts = %d, want the wait not to burn attempts", blocked.Attempts)
	}
	eventually(ctx, t, "the escalation", func() bool { return len(esc.all()) == 1 })
	th, err := h.Store.GetThread(ctx, "alice", blocked.ThreadID)
	if err != nil || th.PendingApproval == nil {
		t.Fatalf("thread = %+v, %v; want a pending approval", th, err)
	}

	// Act: the human approves.
	run, err := h.Resume(ctx, "alice", blocked.ThreadID, harness.ResumeRequest{
		Model: "gpt-test", InterruptID: th.PendingApproval.ID, Reason: "tool_approval", Decision: "approve",
	})
	if err != nil {
		t.Fatal(err)
	}
	waitRun(ctx, t, h, run)

	// Assert (2): the same task carries on and finishes.
	done := waitTask(ctx, t, st, task.ID, factory.StateDone)
	if !strings.Contains(done.Result, "built it") {
		t.Errorf("Result = %q, want the run's final answer", done.Result)
	}
	if done.Attempts != 1 {
		t.Errorf("Attempts = %d after the approval, want still 1", done.Attempts)
	}
}

func TestFactoryDailyBudgetStopsNewWorkBeforeItStarts(t *testing.T) {
	// Arrange: every run spends 228 tokens and the day's cap is 300. The first run ends under it.
	model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		return llmtest.Turn{Chunks: []*ai.Message{llmtest.WithUsage(ai.AssistantMessage("done", nil), 200, 28)}}
	})
	esc := &escalations{}
	h, st, ctx := factoryHarness(t, model, harness.FactoryOptions{Run: true, MaxConcurrent: 1, Escalator: esc,
		DailyBudget: factory.Budget{MaxTokens: 300}, PollEvery: 10 * time.Millisecond})
	f := h.FactoryOf()

	// Act: the second run crosses the cap, so that task is parked at the end of its run.
	first, _, _ := f.Tasks.Create(ctx, factory.Task{Owner: "alice", Title: "first", Spec: "a", Priority: 3})
	waitTask(ctx, t, st, first.ID, factory.StateDone)
	second, _, _ := f.Tasks.Create(ctx, factory.Task{Owner: "alice", Title: "second", Spec: "b", Priority: 2})
	waitTask(ctx, t, st, second.ID, factory.StateBlocked)
	third, _, _ := f.Tasks.Create(ctx, factory.Task{Owner: "alice", Title: "third", Spec: "c", Priority: 1})
	blocked := waitTask(ctx, t, st, third.ID, factory.StateBlocked)

	// Assert: the third task never started, and the reason names the day's cap.
	if blocked.Attempts != 0 || blocked.ThreadID != "" {
		t.Errorf("third task ran: attempts=%d thread=%q", blocked.Attempts, blocked.ThreadID)
	}
	if !strings.Contains(blocked.BlockedReason, "daily budget") {
		t.Errorf("BlockedReason = %q, want the daily budget", blocked.BlockedReason)
	}
}

// sandboxFactory builds a factory harness over one in-memory sandbox backend, so a test can see which sandboxes
// exist and which were killed. The model runs one shell command, which makes the run create the sandbox.
func sandboxFactory(t *testing.T, maxPerUser string) (*harness.Harness, factory.Store, context.Context, *sandboxtest.Backend) {
	t.Helper()
	be := sandboxtest.NewBackend()
	model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if c.Messages[len(c.Messages)-1].Role == ai.RoleTool {
			return llmtest.Turn{Chunks: llmtest.Text("ran it")}
		}
		return llmtest.Turn{Chunks: []*ai.Message{ai.AssistantMessage("", []ai.ToolCall{{ID: "c1", Type: "function",
			Function: ai.FunctionCall{Name: "execute_command", Arguments: `{"command":"true"}`}}})}}
	})
	st := factorystore.NewMem(nil)
	h, ctx := facade(t, map[string]string{"LLM_DEFAULT_MODEL": "gpt-test", "MAX_SANDBOXES_PER_USER": maxPerUser}, model,
		func(ho *harness.Options) {
			ho.Backend = be
			ho.Features = append(harness.ProfileCoding(), harness.FactoryFeature(harness.FactoryOptions{
				Store: st, Run: true, MaxConcurrent: 1, PollEvery: 10 * time.Millisecond}))
		})
	return h, st, ctx, be
}

func TestFactoryReleasesTheSandboxOfAFinishedTaskSoTheNextOneFitsTheQuota(t *testing.T) {
	// Arrange: the owner may hold ONE sandbox. Each task's run creates one; without a release the second task
	// would hit the quota and fail.
	h, st, ctx, be := sandboxFactory(t, "1")
	f := h.FactoryOf()

	// Act
	first, _, _ := f.Tasks.Create(ctx, factory.Task{Owner: "alice", Title: "one", Spec: "run it", Priority: 2})
	waitTask(ctx, t, st, first.ID, factory.StateDone)
	second, _, _ := f.Tasks.Create(ctx, factory.Task{Owner: "alice", Title: "two", Spec: "run it", Priority: 1})
	waitTask(ctx, t, st, second.ID, factory.StateDone)

	// Assert: both ran, each in a sandbox of its own, and the first one was destroyed.
	if n := len(be.Created()); n != 2 {
		t.Errorf("sandboxes created = %d, want one per task", n)
	}
	eventually(ctx, t, "the finished tasks' sandboxes to be killed", func() bool { return len(be.Killed()) == 2 })
	done, _ := st.GetTask(ctx, second.ID)
	if done.Attempts != 1 {
		t.Errorf("second task needed %d attempts, want 1 (no quota failure)", done.Attempts)
	}
}

func TestFactoryKeepsTheSandboxOfABlockedTaskForTheHumanAnswer(t *testing.T) {
	// Arrange: a supervised task stops for approval after its run created a sandbox.
	be := sandboxtest.NewBackend()
	model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if c.Messages[len(c.Messages)-1].Role == ai.RoleTool {
			return llmtest.Turn{Chunks: llmtest.Text("ran it")}
		}
		return llmtest.Turn{Chunks: []*ai.Message{ai.AssistantMessage("", []ai.ToolCall{{ID: "c1", Type: "function",
			Function: ai.FunctionCall{Name: "execute_command", Arguments: `{"command":"go build ./..."}`}}})}}
	})
	st := factorystore.NewMem(nil)
	h, ctx := facade(t, map[string]string{"LLM_DEFAULT_MODEL": "gpt-test"}, model, func(ho *harness.Options) {
		ho.Backend = be
		ho.Features = append(harness.ProfileCoding(), harness.FactoryFeature(harness.FactoryOptions{
			Store: st, Run: true, Policy: true, PollEvery: 10 * time.Millisecond}))
	})
	f := h.FactoryOf()

	// Act
	task, _, _ := f.Tasks.Create(ctx, factory.Task{Owner: "alice", Title: "wait", Spec: "x", Policy: factory.PolicySupervised})
	waitTask(ctx, t, st, task.ID, factory.StateBlocked)
	time.Sleep(100 * time.Millisecond) // give a wrongly eager release time to happen

	// Assert: blocked is not finished, so nothing was destroyed.
	if k := be.Killed(); len(k) != 0 {
		t.Errorf("killed %v while the task waits for a human", k)
	}
}
