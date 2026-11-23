package harness

import (
	"context"
	"log/slog"
	"path"
	"slices"
	"strings"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/roles"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/engine/tools"
	"github.com/7ar-chat/harness/features/factory/delegate"
	"github.com/7ar-chat/harness/features/factory/tasks"
)

// FeatureRoles is the name of the Roles feature.
const FeatureRoles = "roles"

// roleKeepTools are the tools a role's allow-list never removes: they are how a run asks a person and loads what
// it was told to.
var roleKeepTools = map[string]bool{"ask_user": true, "update_plan": true, "load_skill": true}

// Roles applies the role a run took on (RunOptions.Role, Options.Roles): the role's prompt as a section of the
// system prompt, and its limits on the run's tools. It contributes nothing without Options.Roles. The run
// records the role among its requested skills (see applyRole), so a worker process finds it too.
func Roles() Feature {
	return builtin{FeatureRoles, func(b *Builder) error {
		res := b.Options.Roles
		skillStore, ok := b.Store.(store.SkillStore)
		if res == nil || !ok {
			return nil
		}
		of := func(ctx context.Context, runID string) *roles.Role {
			if runID == "" {
				return nil
			}
			names, err := skillStore.RunSkills(ctx, runID)
			if err != nil {
				return nil
			}
			ref, ok := roles.RefOfRun(names)
			if !ok {
				return nil
			}
			r, err := res.Get(ctx, ref)
			if err != nil {
				slog.Warn("roles: the role of a run cannot be read, the run goes on without it", "run_id", runID, "role", ref.Name, "error", err)
				return nil
			}
			return r
		}
		b.Section(func(ctx context.Context, in agent.Input) string {
			if in.Run == nil {
				return ""
			}
			if r := of(ctx, in.Run.ID); r != nil {
				return RolePrompt(*r)
			}
			return ""
		})
		b.WrapTools(func(inner tools.Provider) tools.Provider { return roleTools{inner: inner, of: of} })
		return nil
	}}
}

// BuiltinRoles is the catalogue of the delegate tool (planner, coder, reviewer, tester, explorer) as roles, for a
// registry to list them beside the roles of projects and users.
func BuiltinRoles() []roles.Role {
	var out []roles.Role
	for name, r := range delegate.DefaultRoles() {
		br := roles.Role{Scope: roles.ScopeBuiltin, Name: name, Title: strings.ToUpper(name[:1]) + name[1:], Description: r.Description,
			Prompt: r.Instructions, Model: r.Model, Enabled: true, Version: 1, Builtin: true}
		if r.ReadOnly {
			br.ToolProfile, br.PermissionMode = roles.ToolsReadOnly, roles.PermPlan
		}
		out = append(out, br)
	}
	slices.SortFunc(out, func(a, b roles.Role) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// RolePrompt is the system-prompt section of a run taking on r.
func RolePrompt(r roles.Role) string {
	var b strings.Builder
	title := r.Title
	if title == "" {
		title = r.Name
	}
	b.WriteString("## Role: " + title + "\nFor this run you act as the role \"" + r.Name + "\"")
	if r.Description != "" {
		b.WriteString(" (" + r.Description + ")")
	}
	b.WriteString(". Follow its instructions below; they never change your safety rules, your sandbox or the tools you have.\n\n")
	b.WriteString(strings.TrimSpace(r.Prompt))
	return b.String()
}

// roleTools offers the tools of inner that the role of the run allows.
type roleTools struct {
	inner tools.Provider
	of    func(ctx context.Context, runID string) *roles.Role
}

func (p roleTools) Name() string { return p.inner.Name() }

func (p roleTools) Tools(ctx context.Context, rc tools.RunContext) ([]ai.BaseTool, func(), error) {
	ts, cleanup, err := p.inner.Tools(ctx, rc)
	if err != nil {
		return ts, cleanup, err
	}
	r := p.of(ctx, rc.RunID)
	if r == nil {
		return ts, cleanup, nil
	}
	out := ts[:0:0]
	for _, t := range ts {
		info, err := t.Info(ctx)
		if err != nil || info == nil || !RoleAllowsTool(*r, info.Name) {
			continue
		}
		out = append(out, t)
	}
	return out, cleanup, nil
}

// RoleAllowsTool reports whether a run in role r keeps the tool named name: the allow-list (when set) is the
// only set offered, the deny-list removes from it, and a role that names MCP servers keeps only their tools.
// Patterns use path.Match, so "github__*" is every tool of that server.
func RoleAllowsTool(r roles.Role, name string) bool {
	if roleKeepTools[name] {
		return true
	}
	if len(r.AllowTools) > 0 && !matchesAny(r.AllowTools, name) {
		// an MCP server the role names stays reachable without listing each of its tools
		if server, _, ok := strings.Cut(name, "__"); !ok || !contains(r.MCP, server) {
			return false
		}
	}
	if matchesAny(r.DenyTools, name) {
		return false
	}
	if server, _, ok := strings.Cut(name, "__"); ok && len(r.MCP) > 0 && !contains(r.MCP, server) {
		return false
	}
	return true
}

func matchesAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if p == name {
			return true
		}
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool { return containsString(list, s) }

// taskRoleResolver lets factory tasks name the roles of their project and owner (nil without a registry). A builtin
// role is left to the catalogue, which the host may have replaced.
func taskRoleResolver(res roles.Resolver) func(context.Context, factory.Task, string) (tasks.RoleSpec, bool) {
	if res == nil {
		return nil
	}
	return func(ctx context.Context, t factory.Task, name string) (tasks.RoleSpec, bool) {
		r, err := res.Resolve(ctx, roles.Query{Login: t.Owner, ProjectID: t.ProjectID}, strings.ToLower(strings.TrimSpace(name)))
		if err != nil || r.Builtin || r.Scope == roles.ScopeBuiltin {
			return tasks.RoleSpec{}, false
		}
		return tasks.RoleSpec{Instructions: "Your role's instructions are in the Role section of your system prompt.", ReadOnly: r.ReadOnly(),
			Model: r.Model, Effort: r.ReasoningEffort, Skills: r.Skills, Ref: roles.RefOf(*r).String()}, true
	}
}
