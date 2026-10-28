package skills

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/GhaziBenDahmane/harness/agent"
	"github.com/GhaziBenDahmane/harness/agent/prompts"
	"github.com/GhaziBenDahmane/harness/store"
)

// Attachment reasons.
const (
	AttachDefault   = "default"
	AttachRequested = "requested"
)

// Attachment is a skill attached to a run, listed in the prompt ("Attached
// Skills" in the legacy prompt). Requested ones (RunOptions.Skills) are
// installed under /skills/<name>/ when the run's sandbox starts; defaults
// (the built-ins) only once loaded or used (Installer, lazy skills).
type Attachment struct {
	Skill   Skill
	Package *Package
	Reason  string
	// Explicit attachments (requested for the run) get their SKILL.md in the
	// system prompt.
	Explicit bool
}

// RunPlan is the skills of a run: its attachments and the catalog (offered
// through load_skill).
type RunPlan struct {
	Attached []Attachment
	Catalog  []Skill
}

// Find returns the attachment or catalog skill of that name.
func (p RunPlan) Find(name string) (Skill, *Attachment, bool) {
	for i := range p.Attached {
		if p.Attached[i].Skill.Name == name {
			return p.Attached[i].Skill, &p.Attached[i], true
		}
	}
	for _, s := range p.Catalog {
		if s.Name == name {
			return s, nil, true
		}
	}
	return Skill{}, nil, false
}

// Names lists the attached and catalog skill names.
func (p RunPlan) Names() []string {
	var out []string
	seen := map[string]bool{}
	for _, a := range p.Attached {
		if !seen[a.Skill.Name] {
			seen[a.Skill.Name] = true
			out = append(out, a.Skill.Name)
		}
	}
	for _, s := range p.Catalog {
		if !seen[s.Name] {
			seen[s.Name] = true
			out = append(out, s.Name)
		}
	}
	return out
}

// Plan resolves the skills of a run: the built-in skills are attached by
// default, then the requested ones (RunOptions.Skills, by name; unknown names
// are ignored) are attached explicitly, deduplicated by name. Every skill is
// in the catalog.
func (r *Registry) Plan(ctx context.Context, run *store.Run) (RunPlan, error) {
	var plan RunPlan
	requested, err := r.requested(ctx, run.ID)
	if err != nil {
		return plan, err
	}
	index := map[string]int{}
	attach := func(e *entry, reason string, explicit bool) {
		a := Attachment{Skill: e.skill, Package: e.pkg, Reason: reason, Explicit: explicit}
		if i, ok := index[e.skill.Name]; ok {
			if explicit {
				a.Reason = plan.Attached[i].Reason
				plan.Attached[i] = a
			}
			return
		}
		index[e.skill.Name] = len(plan.Attached)
		plan.Attached = append(plan.Attached, a)
	}
	for _, s := range r.List(ctx) {
		plan.Catalog = append(plan.Catalog, s)
		if s.Source == SourceBuiltin {
			attach(r.byName[s.Name], AttachDefault, false)
		}
	}
	for _, name := range requested {
		name, _, _ = strings.Cut(name, "@") // tolerate "name@version" entries
		if e, ok := r.byName[strings.TrimSpace(name)]; ok {
			attach(e, AttachRequested, true)
		}
	}
	return plan, nil
}

// PromptSection lists the enabled skills, the run's attached skills (Work's
// "Attached Skills" section) and appends the SKILL.md of the skills the user
// requested. Text of project skills is fenced as data (see Fence); the others
// are not. It is an agent.PromptSection.
//
// In the v3 prompt variants the skills appear once, as an index (A-08): one
// "- name: description" line each, no "Attached Skills" block; the SKILL.md
// of skills the user explicitly requested still follows. Injecting an
// untrusted (user or global) SKILL.md marks the run's context untrusted.
func (r *Registry) PromptSection(ctx context.Context, in agent.Input) string {
	run, sandbox := in.Run, in.Sandbox
	plan, err := r.Plan(ctx, run)
	if err != nil {
		slog.Warn("skills: resolving run skills failed", "run_id", run.ID, "error", err)
		return ""
	}
	for _, a := range plan.Attached {
		if a.Explicit && a.Skill.Untrusted() {
			agent.ReportUntrusted(ctx)
		}
	}
	if !prompts.IsLegacy(in.Variant) {
		return indexOf(plan, sandbox)
	}
	return promptOf(plan, sandbox)
}

// indexOf is the skills section of the v3 prompts: the index, then the
// skill-creation instructions and the explicitly requested SKILL.md files.
func indexOf(plan RunPlan, sandbox bool) string {
	entries := listed(plan)
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<skills>\n")
	if sandbox {
		b.WriteString(prompts.SkillsIntro + "\n")
	} else {
		b.WriteString(prompts.SkillsIntroNoSandbox + "\n")
	}
	untrusted := false
	for _, s := range entries {
		line := prompts.SkillLine(s.Name, s.Description)
		if s.Untrusted() {
			line, untrusted = "- "+s.Name+": "+Fence(s.Name, strings.TrimPrefix(line, "- "+s.Name+": ")), true
		}
		b.WriteString(line + "\n")
	}
	if untrusted {
		b.WriteString(UserSkillNote + "\n")
	}
	b.WriteString("</skills>")
	writeRequested(&b, plan, sandbox)
	return b.String()
}

// listed is the catalog, then the attached skills missing from it, each once.
func listed(plan RunPlan) []Skill {
	seen := map[string]bool{}
	var entries []Skill
	for _, s := range plan.Catalog {
		seen[s.Name] = true
		entries = append(entries, s)
	}
	for _, a := range plan.Attached {
		if !seen[a.Skill.Name] {
			seen[a.Skill.Name] = true
			entries = append(entries, a.Skill)
		}
	}
	return entries
}

// writeRequested appends the SKILL.md of the explicitly requested skills
// (untrusted ones fenced).
func writeRequested(b *strings.Builder, plan RunPlan, sandbox bool) {
	for _, a := range plan.Attached {
		if !a.Explicit {
			continue
		}
		where := "no sandbox is available: follow the instructions without running its scripts"
		if sandbox {
			where = fmt.Sprintf("its files are installed under /skills/%s/ on first sandbox use", a.Skill.Name)
		}
		body := strings.TrimSpace(a.Package.Body)
		if a.Skill.Untrusted() {
			body = Fence(a.Skill.Name, body)
		}
		fmt.Fprintf(b, "\n\n## Skill %q (requested for this run; %s)\n\n%s", a.Skill.Name, where, body)
	}
}

// promptOf is the harness v2 skills section (legacy prompt), verbatim.
func promptOf(plan RunPlan, sandbox bool) string {
	listed := map[string]bool{}
	var entries []Skill
	for _, s := range plan.Catalog {
		listed[s.Name] = true
		entries = append(entries, s)
	}
	for _, a := range plan.Attached {
		if !listed[a.Skill.Name] {
			listed[a.Skill.Name] = true
			entries = append(entries, a.Skill)
		}
	}
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Skills: these skills contain step-by-step instructions and tested helper scripts:\n")
	untrusted := false
	for _, s := range entries {
		desc := oneLine(s.Description)
		if s.Untrusted() {
			desc, untrusted = Fence(s.Name, desc), true
		}
		fmt.Fprintf(&b, "- %s — %s — %s/%s/SKILL.md\n", s.Name, desc, Root, s.Name)
	}
	b.WriteString("Before producing or editing a document of one of these kinds, call load_skill with the skill name and follow its instructions exactly. ")
	if sandbox {
		b.WriteString("Loading a skill starts nothing: load_skill returns its instructions, and a skill's folder /skills/<name>/ appears once you load it (load_skill) or name its path in a command (read-only: never modify it; write final deliverables to /workspace/outputs/).")
	}
	if untrusted {
		b.WriteString("\n" + UserSkillNote)
	}
	if len(plan.Attached) > 0 {
		b.WriteString("\n\n## Attached Skills\n")
		b.WriteString("The following skills are attached to this request and are available inside the workspace.\n")
		b.WriteString("Apply them in the exact order shown below.\n\n")
		for _, a := range plan.Attached {
			name := a.Skill.Name
			desc := strings.TrimSpace(a.Skill.Description)
			if a.Skill.Untrusted() {
				// Uploaded text: its fenced description is in the list above.
				name, desc = a.Skill.Name, ""
			}
			fmt.Fprintf(&b, "- %s\n", oneLine(name))
			if desc != "" {
				fmt.Fprintf(&b, "  Description: %s\n", oneLine(desc))
			}
			if sandbox {
				when := "before your first sandbox command"
				if a.Reason == AttachDefault {
					when = "once you load it or before a command that uses it"
				}
				fmt.Fprintf(&b, "  Path: %s/%s (installed with its scripts and assets %s; load_skill %s returns its instructions)\n", Root, a.Skill.Name, when, a.Skill.Name)
			} else {
				fmt.Fprintf(&b, "  Instructions: call load_skill %s (no sandbox: follow them without running its scripts)\n", a.Skill.Name)
			}
		}
	}
	writeRequested(&b, plan, sandbox)
	return b.String()
}
