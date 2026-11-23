package roles_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/roles"
)

func valid() roles.Role {
	return roles.Role{Scope: roles.ScopeProject, ScopeID: "p", Name: "docs", Prompt: "write", Enabled: true}
}

func TestValidateAcceptsAndNormalizes(t *testing.T) {
	r := valid()
	r.Name, r.Title, r.Prompt = " docs ", "  Docs  ", "  write  "
	r.Skills = []string{" a ", "a", "", "b"}
	r.DenyTools = []string{"x", " x"}

	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	if r.Name != "docs" || r.Title != "Docs" || r.Prompt != "write" || !reflect.DeepEqual(r.Skills, []string{"a", "b"}) || !reflect.DeepEqual(r.DenyTools, []string{"x"}) {
		t.Errorf("normalized = %+v", r)
	}
}

func TestValidateRefuses(t *testing.T) {
	cases := map[string]func(*roles.Role){
		"upper case name":  func(r *roles.Role) { r.Name = "Docs" },
		"name too long":    func(r *roles.Role) { r.Name = strings.Repeat("a", 33) },
		"name with space":  func(r *roles.Role) { r.Name = "a b" },
		"builtin scope":    func(r *roles.Role) { r.Scope = roles.ScopeBuiltin },
		"no owner":         func(r *roles.Role) { r.ScopeID = "" },
		"no prompt":        func(r *roles.Role) { r.Prompt = "  " },
		"huge prompt":      func(r *roles.Role) { r.Prompt = strings.Repeat("x", roles.MaxPrompt+1) },
		"unknown profile":  func(r *roles.Role) { r.ToolProfile = "root" },
		"unknown mode":     func(r *roles.Role) { r.PermissionMode = "yolo" },
		"write in plan":    func(r *roles.Role) { r.ToolProfile, r.PermissionMode = roles.ToolsWrite, roles.PermPlan },
		"long list entry":  func(r *roles.Role) { r.Skills = []string{strings.Repeat("s", 129)} },
		"too many entries": func(r *roles.Role) { r.MCP = manyNames(roles.MaxListed + 1) },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			r := valid()
			mut(&r)
			if err := r.Validate(); !errors.Is(err, roles.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func manyNames(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "s" + strings.Repeat("x", i%5) + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	return out
}

func TestReadOnly(t *testing.T) {
	for _, c := range []struct {
		profile, mode string
		want          bool
	}{{"", "", false}, {roles.ToolsReadOnly, "", true}, {"", roles.PermPlan, true}, {roles.ToolsWrite, roles.PermAsk, false}} {
		if got := (roles.Role{ToolProfile: c.profile, PermissionMode: c.mode}).ReadOnly(); got != c.want {
			t.Errorf("%+v: %v", c, got)
		}
	}
}

func TestRefEncodingRoundTripsAndRejectsOrdinarySkills(t *testing.T) {
	for _, r := range []roles.Ref{
		{Scope: roles.ScopeProject, ScopeID: "7f3a", Name: "coder"},
		{Scope: roles.ScopeUser, ScopeID: "a:b@c.d", Name: "x"},
		{Scope: roles.ScopeBuiltin, Name: "planner"},
	} {
		got, ok := roles.ParseRef(r.String())
		if !ok || got != r {
			t.Errorf("ParseRef(%q) = %+v %v", r.String(), got, ok)
		}
	}
	for _, s := range []string{"review", "role:", "role:project", "role:team:x:y", "role:project::p"} {
		if _, ok := roles.ParseRef(s); ok {
			t.Errorf("%q parsed as a role reference", s)
		}
	}
	got := roles.Without([]string{"a", "role:project:x:p", "b"})
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("Without = %v", got)
	}
	if r, ok := roles.RefOfRun([]string{"a", "role:user:x:bob"}); !ok || r.ScopeID != "bob" {
		t.Errorf("RefOfRun = %+v %v", r, ok)
	}
}
