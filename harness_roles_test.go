package harness_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/roles"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/tools"
	"github.com/7ar-chat/harness/longtest"
)

// rolesFake resolves from a fixed list, whatever the query.
type rolesFake struct{ list []roles.Role }

func (f rolesFake) find(name string, ref *roles.Ref) (*roles.Role, error) {
	for _, r := range f.list {
		if (ref != nil && roles.RefOf(r) == *ref) || (ref == nil && r.Name == name && r.Enabled) {
			c := r
			return &c, nil
		}
	}
	return nil, roles.ErrNotFound
}

func (f rolesFake) Resolve(_ context.Context, _ roles.Query, name string) (*roles.Role, error) {
	return f.find(name, nil)
}

func (f rolesFake) Get(_ context.Context, ref roles.Ref) (*roles.Role, error) {
	return f.find("", &ref)
}

func (f rolesFake) Available(context.Context, roles.Query) ([]roles.Role, error) { return f.list, nil }

func docsRole() roles.Role {
	return roles.Role{Scope: roles.ScopeProject, ScopeID: "p1", Name: "docs", Title: "Docs writer", Prompt: "WRITE-DOCS-ONLY: be brief.",
		Model: "gpt-test", ReasoningEffort: "low", DenyTools: []string{"ping"}, Skills: []string{"writing"}, Enabled: true, Version: 1}
}

// sendAsRole plays one run in a work-profile harness with the role registry and returns the first model request.
func sendAsRole(t *testing.T, res roles.Resolver, o harness.RunOptions) (*harness.Harness, *llmtest.Model, *store.Run, error) {
	t.Helper()
	model := llmtest.New(
		llmtest.Turn{Chunks: llmtest.Text("done")},
		llmtest.Turn{Chunks: llmtest.Text("A title")},
	)
	h, err := harness.New(harness.Options{
		Config: featureConfig(t, nil), Backend: sandboxtest.NewBackend(), Models: llmtest.Factory{M: model}, Roles: res,
		Tools: []tools.Provider{longtest.ExtraTool("ping", func(context.Context, string) (string, error) { return "pong", nil })},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	go func() { _ = h.Run(ctx) }()
	th, _ := h.NewThread(ctx, "alice", "")
	o.Model = "gpt-test"
	run, err := h.Send(ctx, "alice", th.ID, "write the readme", o)
	if err != nil {
		return h, model, nil, err
	}
	var last string
	for ev := range h.Events(ctx, run.ID, 0) {
		last = ev.Type
	}
	if last != agui.RunFinished {
		t.Fatalf("last event %q", last)
	}
	run, _ = h.Store.GetRun(ctx, run.ID)
	return h, model, run, nil
}

func systemOf(m *llmtest.Model) string {
	var system string
	for _, msg := range m.Calls()[0].Messages {
		if msg.Role == "system" {
			system += msg.Content + "\n"
		}
	}
	return system
}

func TestARunInARoleGetsItsPromptAndToolLimitsAndRecordsIt(t *testing.T) {
	// Act
	h, model, run, err := sendAsRole(t, rolesFake{[]roles.Role{docsRole()}}, harness.RunOptions{Role: "Docs"})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if sys := systemOf(model); !strings.Contains(sys, "## Role: Docs writer") || !strings.Contains(sys, "WRITE-DOCS-ONLY: be brief.") {
		t.Errorf("the role's prompt is not in the system prompt:\n%s", sys)
	}
	names := requestTools(model)
	if slices.Contains(names, "ping") {
		t.Errorf("a denied tool is offered: %v", names)
	}
	if !slices.Contains(names, "read_file") || !slices.Contains(names, "write_file") {
		t.Errorf("the other tools must stay: %v", names)
	}
	got, err := h.Store.RunSkills(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	ref, ok := roles.RefOfRun(got)
	if !ok || ref.Name != "docs" || ref.ScopeID != "p1" || !slices.Contains(got, "writing") {
		t.Errorf("recorded skills = %v", got)
	}
	if run.Mode != store.ModeBuild {
		t.Errorf("mode = %q", run.Mode)
	}
}

func TestAReadOnlyRoleRunsInPlanMode(t *testing.T) {
	// Arrange
	r := docsRole()
	r.ToolProfile = roles.ToolsReadOnly
	r.DenyTools = nil

	// Act
	_, model, run, err := sendAsRole(t, rolesFake{[]roles.Role{r}}, harness.RunOptions{Role: "docs"})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if run.Mode != store.ModePlan {
		t.Fatalf("mode = %q", run.Mode)
	}
	if names := requestTools(model); slices.Contains(names, "write_file") || slices.Contains(names, "ping") {
		t.Errorf("a read-only role offers writers: %v", names)
	}
}

func TestARoleAllowListKeepsOnlyTheListedToolsAndTheAlwaysOnes(t *testing.T) {
	r := docsRole()
	r.DenyTools = nil
	r.AllowTools = []string{"read_file", "search_*"}

	_, model, _, err := sendAsRole(t, rolesFake{[]roles.Role{r}}, harness.RunOptions{Role: "docs"})

	if err != nil {
		t.Fatal(err)
	}
	for _, n := range requestTools(model) {
		if n != "read_file" && !strings.HasPrefix(n, "search_") && n != "ask_user" && n != "update_plan" && n != "load_skill" {
			t.Errorf("tool %s is offered although it is not allowed", n)
		}
	}
	if !slices.Contains(requestTools(model), "read_file") {
		t.Error("read_file is missing")
	}
}

func TestARoleCanSetTheModelAndEffortAndAskFirst(t *testing.T) {
	r := docsRole()
	r.PermissionMode = roles.PermAsk

	_, _, run, err := sendAsRole(t, rolesFake{[]roles.Role{r}}, harness.RunOptions{Role: "docs"})

	if err != nil {
		t.Fatal(err)
	}
	if run.Model != "gpt-test" || run.ReasoningEffort != "low" || run.InteractionMode != "interactive" {
		t.Errorf("run = model %q effort %q interaction %q", run.Model, run.ReasoningEffort, run.InteractionMode)
	}
}

func TestAnUnknownOrDisabledRoleIsAnInvalidRequest(t *testing.T) {
	off := docsRole()
	off.Enabled = false
	for name, res := range map[string]roles.Resolver{"unknown": rolesFake{}, "disabled": rolesFake{[]roles.Role{off}}} {
		t.Run(name, func(t *testing.T) {
			_, _, _, err := sendAsRole(t, res, harness.RunOptions{Role: "docs"})
			if !errors.Is(err, harness.ErrInvalidRequest) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestRolesNeedAResolver(t *testing.T) {
	_, _, _, err := sendAsRole(t, nil, harness.RunOptions{Role: "docs"})
	if !errors.Is(err, harness.ErrInvalidRequest) {
		t.Fatalf("err = %v", err)
	}
}

func TestARoleReferenceCannotBeSmuggledThroughSkills(t *testing.T) {
	// Arrange: the caller names another project's role among the skills, without asking for a role
	evil := roles.RefOf(docsRole()).String()

	// Act
	h, model, run, err := sendAsRole(t, rolesFake{[]roles.Role{docsRole()}}, harness.RunOptions{Skills: []string{evil, "writing"}})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	got, _ := h.Store.RunSkills(context.Background(), run.ID)
	if _, ok := roles.RefOfRun(got); ok {
		t.Errorf("a role reference was accepted from the caller's skills: %v", got)
	}
	if strings.Contains(systemOf(model), "WRITE-DOCS-ONLY") {
		t.Error("the smuggled role was applied")
	}
}

func TestRoleAllowsTool(t *testing.T) {
	cases := []struct {
		name string
		role roles.Role
		tool string
		want bool
	}{
		{"no limits", roles.Role{}, "write_file", true},
		{"deny", roles.Role{DenyTools: []string{"write_file"}}, "write_file", false},
		{"deny glob", roles.Role{DenyTools: []string{"github__*"}}, "github__create_issue", false},
		{"allow list excludes others", roles.Role{AllowTools: []string{"read_file"}}, "write_file", false},
		{"allow list keeps listed", roles.Role{AllowTools: []string{"read_file"}}, "read_file", true},
		{"allow glob", roles.Role{AllowTools: []string{"search_*"}}, "search_files", true},
		{"always kept", roles.Role{AllowTools: []string{"read_file"}, DenyTools: []string{"ask_user"}}, "ask_user", true},
		{"mcp server kept", roles.Role{MCP: []string{"github"}}, "github__list_issues", true},
		{"mcp other server dropped", roles.Role{MCP: []string{"github"}}, "linear__list_issues", false},
		{"mcp does not touch native tools", roles.Role{MCP: []string{"github"}}, "read_file", true},
		{"mcp reachable under an allow list", roles.Role{AllowTools: []string{"read_file"}, MCP: []string{"github"}}, "github__list_issues", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := harness.RoleAllowsTool(c.role, c.tool); got != c.want {
				t.Errorf("RoleAllowsTool(%+v, %q) = %v, want %v", c.role, c.tool, got, c.want)
			}
		})
	}
}

func TestBuiltinRolesAreTheDelegateCatalogue(t *testing.T) {
	got := harness.BuiltinRoles()
	var names []string
	for _, r := range got {
		names = append(names, r.Name)
		if !r.Builtin || r.Scope != roles.ScopeBuiltin || !r.Enabled || r.Prompt == "" {
			t.Errorf("builtin %s = %+v", r.Name, r)
		}
		if (r.Name == "planner" || r.Name == "reviewer" || r.Name == "explorer") != r.ReadOnly() {
			t.Errorf("%s read-only = %v", r.Name, r.ReadOnly())
		}
	}
	if !slices.Equal(names, []string{"coder", "explorer", "planner", "reviewer", "tester"}) {
		t.Errorf("names = %v", names)
	}
}
