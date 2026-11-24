package product

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/capstore"
	"github.com/7ar-chat/harness/adapters/store/executorstore"
	"github.com/7ar-chat/harness/adapters/store/reqstore"
	"github.com/7ar-chat/harness/adapters/store/rolestore"
	"github.com/7ar-chat/harness/adapters/store/schedstore"
	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/adapters/store/workflowstore"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/executors"
	"github.com/7ar-chat/harness/core/requirements"
	"github.com/7ar-chat/harness/core/roles"
	"github.com/7ar-chat/harness/core/schedule"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/core/workflows"
	"github.com/7ar-chat/harness/features/artifacts"
	"github.com/7ar-chat/harness/features/chatqueue"
)

// cleanRig has a store of every kind a project or a thread owns something in.
type cleanRig struct {
	*rig
	team    *teamstore.Mem
	reqs    *reqstore.Mem
	defs    *executorstore.Mem
	wfs     *workflowstore.Mem
	sched   *schedstore.Mem
	roles   *rolestore.Mem
	project string
}

func newCleanRig(t *testing.T) *cleanRig {
	t.Helper()
	tm := teamstore.NewMem()
	for _, l := range []string{"alice", "bob"} {
		if _, err := tm.UpsertUser(context.Background(), store.User{Login: l, Name: l, Email: l + "@x.io"}); err != nil {
			t.Fatal(err)
		}
	}
	c := &cleanRig{team: tm, reqs: reqstore.NewMem(nil), defs: executorstore.NewMem(nil), wfs: workflowstore.NewMem(nil), sched: schedstore.NewMem(), roles: rolestore.NewMem(nil)}
	api, err := NewExecutorAPI(tm, capstore.NewMem(nil), c.defs, []byte("a-secret-key-for-the-test-0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	c.rig = newRig(t, rigOpts{team: true, model: llmtest.New(llmtest.Turn{Chunks: llmtest.Text("ok")}),
		prod: Options{Team: tm, Requirements: c.reqs, Executors: api, Workflows: c.wfs, Roles: c.roles, Capabilities: capstore.NewMem(nil),
			Schedules: &SchedulesOptions{Store: c.sched, NoRun: true}, SecretKey: []byte("a-secret-key-for-the-test-0123456789")},
		mod: func(o *harness.Options) { o.Roles = NewRoleRegistry(tm, c.roles) }})
	c.project = c.json("POST", "/api/projects", "alice", `{"name":"api"}`, 201)["id"].(string)
	return c
}

// state is the thread-state documents of a thread by key.
func (c *cleanRig) docs(thread string) []string {
	var keys []string
	for _, k := range []string{chatqueue.Key, artifacts.PublishedKey, harness.EnvironmentKey} {
		if raw, _ := c.h.ThreadState.GetThreadState(c.ctx, thread, k); len(raw) > 0 {
			keys = append(keys, k)
		}
	}
	return keys
}

func (c *cleanRig) putDocs(thread string) {
	c.t.Helper()
	for _, k := range []string{chatqueue.Key, artifacts.PublishedKey, harness.EnvironmentKey} {
		err := c.h.ThreadState.UpdateThreadState(c.ctx, thread, k, func(json.RawMessage) (json.RawMessage, error) { return json.RawMessage(`[]`), nil })
		if err != nil {
			c.t.Fatal(err)
		}
	}
	if len(c.docs(thread)) != 3 {
		c.t.Fatalf("documents not stored: %v", c.docs(thread))
	}
}

func TestDeletingAThreadDropsItsStateAndTheScheduleLink(t *testing.T) {
	// Arrange
	c := newCleanRig(t)
	th := c.json("POST", "/api/threads", "alice", `{"projectId":"`+c.project+`"}`, 201)["id"].(string)
	other := c.json("POST", "/api/threads", "alice", `{"projectId":"`+c.project+`"}`, 201)["id"].(string)
	c.putDocs(th)
	c.putDocs(other)
	sc, err := c.sched.Create(c.ctx, schedule.Schedule{ProjectID: c.project, Owner: "alice", RunAs: "alice", Name: "n", Prompt: "p", Cron: "@daily", ReuseSession: true, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := c.sched.StartManual(c.ctx, sc.ID, "alice", sc.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.sched.SetThread(c.ctx, claim.Run.ID, th, ""); err != nil {
		t.Fatal(err)
	}

	// Act
	c.json("DELETE", "/api/threads/"+th, "alice", "", 204)

	// Assert
	if got := c.docs(th); len(got) != 0 {
		t.Fatalf("the deleted thread keeps %v", got)
	}
	if got := c.docs(other); len(got) != 3 {
		t.Fatalf("another thread lost its documents: %v", got)
	}
	got, _ := c.sched.Get(c.ctx, sc.ID)
	if got.ThreadID != "" {
		t.Fatalf("the schedule still reuses the deleted conversation %q", got.ThreadID)
	}
}

func TestDeletingAProjectRemovesEverythingItOwned(t *testing.T) {
	// Arrange
	c := newCleanRig(t)
	other := c.json("POST", "/api/projects", "alice", `{"name":"keep"}`, 201)["id"].(string)
	thread := c.json("POST", "/api/threads", "alice", `{"projectId":"`+c.project+`"}`, 201)["id"].(string)
	c.putDocs(thread)
	seed := func(pid string) {
		if _, _, err := c.reqs.Create(c.ctx, requirements.Requirement{ProjectID: pid, Title: "r"}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.defs.Create(c.ctx, executors.Executor{ProjectID: pid, Name: "mycli", Command: "mycli {task}", Binary: "mycli", Enabled: true}, "alice"); err != nil {
			t.Fatal(err)
		}
		if _, err := c.roles.Create(c.ctx, roles.Role{Scope: roles.ScopeProject, ScopeID: pid, Name: "reviewer", Prompt: "x"}, "alice"); err != nil {
			t.Fatal(err)
		}
		w, err := c.wfs.Create(c.ctx, workflows.Workflow{ProjectID: pid, Name: "wf", Nodes: []workflows.Node{{ID: "a", Kind: workflows.KindAgent, Prompt: "x"}}, Status: workflows.StatusPublished}, "alice")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.wfs.CreateRun(c.ctx, workflows.Run{ProjectID: pid, WorkflowID: w.ID, WorkflowName: "wf", Graph: w.Nodes, State: workflows.RunDone, Owner: "alice"}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.sched.Create(c.ctx, schedule.Schedule{ProjectID: pid, Owner: "alice", RunAs: "alice", Name: "n", Prompt: "p", Cron: "@daily", Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	seed(c.project)
	seed(other)

	// Act
	c.json("DELETE", "/api/projects/"+c.project, "alice", "", 204)

	// Assert: nothing of the deleted project is left in any store, and the other project is untouched.
	count := func(pid string) map[string]int {
		n := map[string]int{}
		rl, _ := c.reqs.List(c.ctx, requirements.Filter{ProjectID: pid})
		n["requirements"] = len(rl)
		el, _ := c.defs.List(c.ctx, pid)
		n["executors"] = len(el)
		ro, _ := c.roles.List(c.ctx, roles.ScopeProject, pid)
		n["roles"] = len(ro)
		wl, _ := c.wfs.List(c.ctx, pid)
		n["workflows"] = len(wl)
		runs, _ := c.wfs.ListRuns(c.ctx, workflows.RunFilter{ProjectID: pid})
		n["runs"] = len(runs)
		sl, _ := c.sched.List(c.ctx, pid, "")
		n["schedules"] = len(sl)
		return n
	}
	for k, v := range count(c.project) {
		if v != 0 {
			t.Errorf("deleted project still has %d %s", v, k)
		}
	}
	for k, v := range count(other) {
		if v != 1 {
			t.Errorf("the other project has %d %s, want 1", v, k)
		}
	}
	if got := c.docs(thread); len(got) != 0 {
		t.Errorf("a thread of the deleted project keeps its documents %v", got)
	}
}
