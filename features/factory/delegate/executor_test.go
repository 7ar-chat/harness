package delegate_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/7ar-chat/harness/adapters/store/factorystore"
	"github.com/7ar-chat/harness/core/executors"
	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/roles"
	"github.com/7ar-chat/harness/engine/tools"
	"github.com/7ar-chat/harness/features/factory/delegate"
)

type fakeRunner struct {
	mu  sync.Mutex
	req []executors.Request
	res *executors.Result
	err error
}

func (f *fakeRunner) Run(_ context.Context, r executors.Request, _ func(string)) (*executors.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.req = append(f.req, r)
	return f.res, f.err
}

func execRole() roles.Role {
	r := projectRole("fixer")
	r.ToolProfile, r.Executor, r.Prompt = "", "claude-code", "ROLE-PROMPT"
	return r
}

func TestDelegateToARoleWithAnExecutorRunsTheCLIInsteadOfASubAgent(t *testing.T) {
	reg := &fakeRegistry{list: []roles.Role{execRole()}}
	sp := &fakeSpawner{}
	run := &fakeRunner{res: &executors.Result{Executor: "claude-code", Level: executors.LevelEdit, Summary: "fixed it", Changed: []string{"a.go"}}}
	st := factorystore.NewMem(nil)
	seed(t, st, factory.ThreadInfo{ThreadID: "t1", Login: "ann", TaskID: "task1", Policy: factory.PolicySandboxed})
	p := delegate.New(delegate.Options{Spawner: sp, Registry: reg, Executors: run, Store: st})
	tl := toolByName(t, p, tools.RunContext{ThreadID: "t1", Login: "ann"}, "delegate")

	out, err := call(t, tl, map[string]any{"role": "fixer", "task": "fix the bug"})

	if err != nil {
		t.Fatal(err)
	}
	if len(sp.requests()) != 0 {
		t.Fatal("a sub-agent was spawned")
	}
	if len(run.req) != 1 {
		t.Fatalf("runner calls: %d", len(run.req))
	}
	r := run.req[0]
	if r.Executor != "claude-code" || r.ThreadID != "t1" || r.Login != "ann" || r.Policy != factory.PolicySandboxed || r.TaskID != "task1" {
		t.Errorf("request %+v", r)
	}
	if !strings.Contains(r.Task, "ROLE-PROMPT") || !strings.Contains(r.Task, "fix the bug") {
		t.Errorf("task %q", r.Task)
	}
	if !strings.Contains(out, "fixed it") || !strings.Contains(out, "a.go") {
		t.Errorf("report %q", out)
	}
}

func TestDelegateExecutorFailuresAreReports(t *testing.T) {
	reg := &fakeRegistry{list: []roles.Role{execRole()}}
	for name, run := range map[string]*fakeRunner{
		"error":       {err: errors.New("claude is not installed")},
		"exit code":   {res: &executors.Result{Executor: "claude-code", ExitCode: 2, Summary: "nope"}},
		"no executor": nil,
	} {
		o := delegate.Options{Spawner: &fakeSpawner{}, Registry: reg}
		if run != nil {
			o.Executors = run
		}
		tl := toolByName(t, delegate.New(o), tools.RunContext{ThreadID: "t"}, "delegate")
		out, err := call(t, tl, map[string]any{"role": "fixer", "task": "x"})
		if err != nil || !strings.HasPrefix(out, "DELEGATION FAILED") {
			t.Errorf("%s: %q %v", name, out, err)
		}
	}
}

func TestDelegateExecutorRolesAreRefusedUnderTheSupervisedPolicy(t *testing.T) {
	reg := &fakeRegistry{list: []roles.Role{execRole()}}
	run := &fakeRunner{res: &executors.Result{Executor: "claude-code"}}
	st := factorystore.NewMem(nil)
	seed(t, st, factory.ThreadInfo{ThreadID: "t1", Login: "ann", TaskID: "k", Policy: factory.PolicySupervised})
	tl := toolByName(t, delegate.New(delegate.Options{Spawner: &fakeSpawner{}, Registry: reg, Executors: run, Store: st}), tools.RunContext{ThreadID: "t1"}, "delegate")

	out, _ := call(t, tl, map[string]any{"role": "fixer", "task": "x"})

	if !strings.HasPrefix(out, "DELEGATION FAILED") || !strings.Contains(out, "supervised") || len(run.req) != 0 {
		t.Fatalf("%q runs %d", out, len(run.req))
	}
}

func TestRosterNamesTheExecutorOfARole(t *testing.T) {
	reg := &fakeRegistry{list: []roles.Role{execRole()}}
	p := delegate.New(delegate.Options{Spawner: &fakeSpawner{}, Registry: reg})
	list, _, err := p.Tools(context.Background(), tools.RunContext{ThreadID: "t"})
	if err != nil {
		t.Fatal(err)
	}
	info, _ := list[0].Info(context.Background())
	if !strings.Contains(info.Desc, "external executor claude-code") {
		t.Errorf("description: %s", info.Desc)
	}
}
