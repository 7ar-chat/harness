package tasks_test

import (
	"context"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/features/factory/tasks"
)

func TestTaskRoleOfTheProjectIsAcceptedAndSpawnedWithItsSettings(t *testing.T) {
	// Arrange: "docs" exists only in project p1; the catalogue knows the coder
	var asked []factory.Task
	e := newEnv(t, func(o *tasks.Options) {
		o.Roles = func(role string) (string, bool, bool) { return "builtin " + role, false, role == factory.RoleCoder }
		o.ResolveRole = func(_ context.Context, tk factory.Task, role string) (tasks.RoleSpec, bool) {
			asked = append(asked, tk)
			if role != "docs" || tk.ProjectID != "p1" {
				return tasks.RoleSpec{}, false
			}
			return tasks.RoleSpec{Instructions: "see the role section", ReadOnly: true, Model: "cheap", Effort: "low", Skills: []string{"writing"}, Ref: "role:project:docs:p1"}, true
		}
	})

	// Act
	e.create(t, factory.Task{Role: "docs", ProjectID: "p1"})
	if _, _, err := e.svc.Create(e.ctx, factory.Task{Title: "x", Owner: "alice", Role: "docs", ProjectID: "other"}); err == nil {
		t.Error("a role of another project was accepted")
	}
	e.dispatch(t)

	// Assert
	reqs := e.sp.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %+v", reqs)
	}
	r := reqs[0]
	if r.Role != "docs" || r.RoleRef != "role:project:docs:p1" || r.Model != "cheap" || r.ReasoningEffort != "low" || !r.ReadOnly || len(r.Skills) != 1 || r.Instructions != "see the role section" {
		t.Errorf("request = %+v", r)
	}
	if len(asked) == 0 || asked[0].ProjectID != "p1" {
		t.Errorf("the resolver was not given the task: %+v", asked)
	}
}

func TestTaskOfARoleWithAnExecutorRunsAsItsDriver(t *testing.T) {
	e := newEnv(t, func(o *tasks.Options) {
		o.Roles = func(role string) (string, bool, bool) { return "builtin " + role, false, role == factory.RoleCoder }
		o.ResolveRole = func(_ context.Context, _ factory.Task, role string) (tasks.RoleSpec, bool) {
			if role != "fixer" {
				return tasks.RoleSpec{}, false
			}
			return tasks.RoleSpec{Instructions: "see the role section", Executor: "claude-code", Ref: "role:project:fixer:p1"}, true
		}
	})

	e.create(t, factory.Task{Role: "fixer", ProjectID: "p1"})
	e.dispatch(t)

	reqs := e.sp.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %+v", reqs)
	}
	ins := reqs[0].Instructions
	if !strings.Contains(ins, `run_executor with executor="claude-code"`) || !strings.Contains(ins, "see the role section") {
		t.Errorf("instructions = %q", ins)
	}
}

func TestTaskRoleFallsThroughToTheCatalogueAndTheTaskModelWins(t *testing.T) {
	e := newEnv(t, func(o *tasks.Options) {
		o.Models = []string{"mine", "cheap"}
		o.Roles = func(role string) (string, bool, bool) { return "builtin " + role, false, true }
		o.ResolveRole = func(_ context.Context, _ factory.Task, role string) (tasks.RoleSpec, bool) {
			if role == "docs" {
				return tasks.RoleSpec{Model: "cheap", Ref: "role:project:docs:p1"}, true
			}
			return tasks.RoleSpec{}, false
		}
	})

	e.create(t, factory.Task{Role: factory.RoleCoder})
	e.create(t, factory.Task{Role: "docs", Model: "mine"})
	e.dispatch(t)

	byRole := map[string]factory.SpawnRequest{}
	for _, r := range e.sp.requests() {
		byRole[r.Role] = r
	}
	if r := byRole[factory.RoleCoder]; r.RoleRef != "" || r.Instructions != "builtin coder" {
		t.Errorf("coder = %+v", r)
	}
	if r := byRole["docs"]; r.RoleRef == "" || r.Model != "mine" {
		t.Errorf("docs = %+v", r)
	}
}
