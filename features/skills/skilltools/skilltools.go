// Package skilltools exposes load_skill: it returns a skill's SKILL.md
// (without frontmatter) and file list, and records the skill as loaded by the
// run. It never starts a sandbox: the skill is installed at /skills/<name>/ in
// the running sandbox at once, else when the run's sandbox starts.
package skilltools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/core/sandbox"
	"github.com/GhaziBenDahmane/harness/engine/tools"
	"github.com/GhaziBenDahmane/harness/features/skills"
)

// Provider is the skills tool provider.
type Provider struct {
	Registry  *skills.Registry
	Installer *skills.Installer
	Sandbox   sandbox.Manager
}

// Name implements tools.Provider.
func (p *Provider) Name() string { return "skills" }

const loadDesc = `Load a skill: returns its SKILL.md and file list. Loading a skill starts nothing; a skill's folder /skills/<name>/ (read-only, with its helper scripts) appears once you load it (load_skill) or name its path in a command.
Call it before creating or editing a file of the kind the skill covers (see the skills listed in your instructions), then follow it: it names the scripts to use and the checks to run before you call the work done.`

type loadInput struct {
	Name string `json:"name" jsonschema_description:"The skill name as listed in the system prompt (e.g. docx)"`
}

// Tools implements tools.Provider.
func (p *Provider) Tools(_ context.Context, rc tools.RunContext) ([]ai.BaseTool, func(), error) {
	t, err := ai.InferTool("load_skill", loadDesc, func(ctx context.Context, in loadInput) (string, error) {
		return p.load(ctx, rc, strings.TrimSpace(in.Name))
	})
	if err != nil {
		return nil, nil, err
	}
	return []ai.BaseTool{readOnlyTool{t}}, func() {}, nil
}

// readOnlyTool marks load_skill read-only (tools.ReadOnly): it returns the
// skill's instructions and at most installs its files (idempotently).
type readOnlyTool struct{ ai.InvokableTool }

func (readOnlyTool) ReadOnly() bool { return true }

func (p *Provider) load(ctx context.Context, rc tools.RunContext, name string) (string, error) {
	plan, err := p.Installer.Plan(ctx, rc)
	if err != nil {
		return "", fmt.Errorf("listing skills: %w", err)
	}
	s, att, found := plan.Find(name)
	if !found {
		list := "none"
		if names := plan.Names(); len(names) > 0 {
			list = strings.Join(names, ", ")
		}
		return "", tools.Errorf("unknown skill %q; available skills: %s", name, list)
	}
	var pkg *skills.Package
	if att != nil {
		pkg = att.Package
	} else if s, pkg, err = p.Installer.ByName(ctx, rc, name); err != nil {
		return "", fmt.Errorf("loading skill %s: %w", name, err)
	}
	if p.Sandbox == nil || !p.Sandbox.Enabled() || rc.Sandbox == tools.SandboxOff {
		return skills.Instructions(s, pkg, skills.NoSandbox), nil
	}
	p.Installer.MarkNeeded(rc, s.Name)
	// Only a running sandbox gets the skill now (never created here, so the
	// sandbox quota does not apply); otherwise it is installed when the
	// run's sandbox starts (Installer.EnsureRunSkills).
	sb, err := p.Sandbox.Get(ctx, rc.ThreadID)
	if err != nil {
		if !errors.Is(err, sandbox.ErrNoSandbox) {
			slog.Warn("load_skill: sandbox unavailable, the skill is installed later", "thread_id", rc.ThreadID, "skill", name, "error", err)
		}
		return skills.Instructions(s, pkg, skills.OnFirstCommand), nil
	}
	if err := p.Installer.InstallWithDependencies(ctx, rc, sb, s, pkg); err != nil {
		if sandbox.IsGone(err) {
			p.Sandbox.Invalidate(ctx, rc.ThreadID, sb.ID())
		} else if ctx.Err() == nil {
			slog.Warn("load_skill: installing failed, retried before the next command", "thread_id", rc.ThreadID, "skill", name, "error", err)
		}
		return skills.Instructions(s, pkg, skills.OnFirstCommand), nil
	}
	return skills.Instructions(s, pkg, skills.Installed), nil
}
