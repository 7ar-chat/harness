package delegate_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/roles"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/engine/tools"
	"github.com/7ar-chat/harness/features/factory/delegate"
)

// fakeRegistry answers from a list; the query is recorded.
type fakeRegistry struct {
	list []roles.Role
	got  []roles.Query
	err  error
}

func (f *fakeRegistry) Resolve(_ context.Context, q roles.Query, name string) (*roles.Role, error) {
	f.got = append(f.got, q)
	if f.err != nil {
		return nil, f.err
	}
	for _, r := range f.list {
		if r.Name == name {
			c := r
			return &c, nil
		}
	}
	return nil, roles.ErrNotFound
}

func (f *fakeRegistry) Get(context.Context, roles.Ref) (*roles.Role, error) {
	return nil, roles.ErrNotFound
}

func (f *fakeRegistry) Available(context.Context, roles.Query) ([]roles.Role, error) {
	return f.list, f.err
}

func projectRole(name string) roles.Role {
	return roles.Role{Scope: roles.ScopeProject, ScopeID: "p1", Name: name, Description: "docs writer", Prompt: "write docs",
		Model: "cheap", ReasoningEffort: "low", Skills: []string{"writing"}, ToolProfile: roles.ToolsReadOnly, Enabled: true}
}

func TestDelegateResolvesProjectRolesFromTheRegistryFirst(t *testing.T) {
	// Arrange
	reg := &fakeRegistry{list: []roles.Role{projectRole("docs")}}
	sp := &fakeSpawner{}
	p := delegate.New(delegate.Options{Spawner: sp, Registry: reg})
	tl := toolByName(t, p, tools.RunContext{ThreadID: "t1", Login: "ann"}, "delegate")

	// Act
	_, err := call(t, tl, map[string]any{"role": "Docs", "task": "write the readme"})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	r := sp.requests()[0]
	ref := roles.Ref{Scope: roles.ScopeProject, ScopeID: "p1", Name: "docs"}.String()
	if r.Role != "docs" || r.RoleRef != ref || r.Model != "cheap" || r.ReasoningEffort != "low" || !r.ReadOnly || len(r.Skills) != 1 {
		t.Errorf("request = %+v", r)
	}
	if strings.Contains(r.Instructions, "write docs") {
		t.Errorf("the role's prompt must be applied by the run, not copied into the thread info: %q", r.Instructions)
	}
	if len(reg.got) != 1 || reg.got[0].ThreadID != "t1" || reg.got[0].Login != "ann" {
		t.Errorf("registry asked %+v", reg.got)
	}
}

func TestDelegateRegistryRoleOverridesTheBuiltinOfTheSameName(t *testing.T) {
	// Arrange
	over := projectRole("coder")
	over.ToolProfile = ""
	reg := &fakeRegistry{list: []roles.Role{over}}
	sp := &fakeSpawner{}
	tl := toolByName(t, delegate.New(delegate.Options{Spawner: sp, Registry: reg}), tools.RunContext{ThreadID: "t"}, "delegate")

	// Act
	_, err := call(t, tl, map[string]any{"role": "coder", "task": "x"})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if r := sp.requests()[0]; r.RoleRef == "" || r.Model != "cheap" {
		t.Errorf("the project's coder was not used: %+v", r)
	}
}

func TestDelegateFallsBackToTheBuiltinCatalogue(t *testing.T) {
	// Arrange: the registry knows nothing, or fails
	for name, reg := range map[string]*fakeRegistry{"unknown": {}, "failing": {err: errors.New("db down")}} {
		t.Run(name, func(t *testing.T) {
			sp := &fakeSpawner{}
			tl := toolByName(t, delegate.New(delegate.Options{Spawner: sp, Registry: reg}), tools.RunContext{ThreadID: "t"}, "delegate")

			// Act
			_, err := call(t, tl, map[string]any{"role": "planner", "task": "x"})

			// Assert
			if err != nil {
				t.Fatal(err)
			}
			if r := sp.requests()[0]; r.RoleRef != "" || !r.ReadOnly || r.Instructions != delegate.DefaultRoles()["planner"].Instructions {
				t.Errorf("request = %+v", r)
			}
		})
	}
}

func TestDelegateListsRegistryRolesInTheToolDescriptionAndRoster(t *testing.T) {
	// Arrange
	reg := &fakeRegistry{list: []roles.Role{projectRole("docs")}}
	p := delegate.New(delegate.Options{Spawner: &fakeSpawner{}, Registry: reg})

	// Act
	list, _, err := p.Tools(context.Background(), tools.RunContext{ThreadID: "t"})
	if err != nil {
		t.Fatal(err)
	}
	info, _ := list[0].Info(context.Background())
	roster := delegate.RosterWith(nil, reg)(context.Background(), agent.Input{Run: &store.Run{ThreadID: "t", UserLogin: "ann"}})

	// Assert
	if !strings.Contains(info.Desc, "- docs (read-only): docs writer") || !strings.Contains(info.Desc, "- coder") {
		t.Errorf("tool description:\n%s", info.Desc)
	}
	if !strings.Contains(roster, "- docs: docs writer") || !strings.Contains(roster, "- planner") {
		t.Errorf("roster:\n%s", roster)
	}
}

func TestDelegateUnknownRoleListsTheRegistryRoles(t *testing.T) {
	reg := &fakeRegistry{list: []roles.Role{projectRole("docs")}}
	tl := toolByName(t, delegate.New(delegate.Options{Spawner: &fakeSpawner{}, Registry: reg}), tools.RunContext{ThreadID: "t"}, "delegate")

	_, err := call(t, tl, map[string]any{"role": "wizard", "task": "x"})

	if err == nil || !strings.Contains(err.Error(), "coder, docs, explorer") {
		t.Fatalf("err = %v", err)
	}
}
