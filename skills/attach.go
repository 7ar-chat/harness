package skills

import (
	"context"
	"errors"
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
	AttachSession   = "session"
	AttachRequested = "requested"
)

// Attachment is a skill attached to a run, listed in the prompt ("Attached
// Skills" in the legacy prompt). Requested ones (and a creation session's
// skill-creator) are installed under /skills/<name>/ when the run's sandbox
// starts; defaults only once loaded or used (Installer, lazy skills).
type Attachment struct {
	Skill   Skill
	Package *Package
	// VersionID / VersionLabel are the attached version.
	VersionID    string
	VersionLabel string
	Reason       string
	// Explicit attachments (mentioned or selected by the user) get their
	// SKILL.md in the system prompt.
	Explicit bool
}

// RunPlan is the skills of a run: its attachments and the enabled catalog
// (offered through load_skill).
type RunPlan struct {
	Attached []Attachment
	Catalog  []Skill
	// Session is set for a guided skill creation thread.
	Session *store.SkillCreationSession
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

// splitEntry parses a work_runs.skills entry ("name" or "name@versionId").
func splitEntry(e string) (name, versionID string) {
	name, versionID, _ = strings.Cut(e, "@")
	return name, versionID
}

// Plan resolves the skills of a run: the default skills (Work: published
// global studio-work-default* skills, here also the built-ins) unless the user
// disabled them, the skill-creator of a skill creation session, then the
// requested ones (mentions / selection, a specific version when one was
// chosen), deduplicated by name — an explicit version replaces the default one.
func (r *Registry) Plan(ctx context.Context, run *store.Run) (RunPlan, error) {
	var plan RunPlan
	requested, err := r.store.RunSkills(ctx, run.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return plan, err
	}
	v, err := r.view(ctx, run.UserLogin, false)
	if err != nil {
		return plan, err
	}
	if r.cat != nil && run.ThreadID != "" {
		if cs, err := r.cat.GetSkillCreationSession(ctx, run.UserLogin, run.ThreadID); err == nil {
			plan.Session = cs
		} else if !errors.Is(err, store.ErrNotFound) {
			return plan, err
		}
	}
	index := map[string]int{}
	attach := func(s Skill, versionID, reason string, explicit bool) {
		if versionID == "" {
			versionID = s.VersionID
		}
		pkg, err := r.packageOf(ctx, run.UserLogin, s, versionID)
		if err != nil {
			slog.Warn("skills: attached skill unavailable", "run_id", run.ID, "skill", s.Name, "error", err)
			return
		}
		label := s.VersionLabel
		for _, ver := range s.Versions {
			if ver.ID == versionID {
				label = ver.Version
			}
		}
		a := Attachment{Skill: s, Package: pkg, VersionID: versionID, VersionLabel: label, Reason: reason, Explicit: explicit}
		if i, ok := index[s.Name]; ok {
			prev := plan.Attached[i]
			if explicit {
				a.Reason = prev.Reason
				if versionID == prev.VersionID {
					a.Package = prev.Package
				}
				plan.Attached[i] = a
			}
			return
		}
		index[s.Name] = len(plan.Attached)
		plan.Attached = append(plan.Attached, a)
	}
	all := v.all()
	for _, s := range all {
		if s.Allowed && s.IncludedByDefault && s.Enabled && !v.shadowed(s) {
			attach(s, s.Published.ID, AttachDefault, false)
		}
	}
	if plan.Session != nil {
		if s, ok := lookupName(v, SkillCreatorSlug); ok {
			attach(s, "", AttachSession, false)
		}
	}
	for _, e := range requested {
		name, versionID := splitEntry(e)
		s, ok := lookupEntry(v, name, versionID)
		if !ok {
			continue
		}
		attach(s, versionID, AttachRequested, true)
	}
	for _, s := range all {
		if s.Allowed && s.Enabled && !v.shadowed(s) {
			plan.Catalog = append(plan.Catalog, s)
		}
	}
	return plan, nil
}

// lookupEntry resolves a work_runs.skills entry: a pinned version belongs to
// exactly one skill (also a global one shadowed by a private skill of the same
// name); a bare name resolves like lookupName.
func lookupEntry(v *catalogView, name, versionID string) (Skill, bool) {
	if versionID == "" {
		return lookupName(v, name)
	}
	for _, s := range v.all() {
		if s.Name == name && s.Allowed && hasVersion(s, versionID) {
			return s, true
		}
	}
	return Skill{}, false
}

// SkillCreationPackageRoot is where guided sessions write generated packages (Work).
const SkillCreationPackageRoot = "/workspace/generated-skill"

// SkillCreationInstruction is Work's _SKILL_CREATION_PACKAGE_INSTRUCTION.
const SkillCreationInstruction = "Store every complete generated skill package only under " + SkillCreationPackageRoot +
	"/<skill-name>. Each skill must have its own entry point at " + SkillCreationPackageRoot + "/<skill-name>/SKILL.md. " +
	"Before finishing, write all package files there; do not save generated packages under .codex, .claude, or any other directory."

// SkillCreationPrompt is Work's default guided creation prompt.
const SkillCreationPrompt = "Help me create a new skill. Ask me focused questions, then build the complete skill package."

// SessionPrompt is the first message of a session (Work's _skill_creation_prompt).
func SessionPrompt(prompt string) string {
	base := strings.TrimSpace(prompt)
	if base == "" {
		base = SkillCreationPrompt
	}
	return base + "\n\n" + SkillCreationInstruction
}

// PromptSection lists the enabled skills, the run's attached skills (Work's
// "Attached Skills" section) and appends the SKILL.md of the skills the user
// mentioned or selected. Text of user and operator skills is fenced as data
// (see Fence); built-in and Anthropic skills are not. It is an agent.PromptSection.
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

// writeRequested appends the skill creation session and the SKILL.md of the
// explicitly requested skills (untrusted ones fenced).
func writeRequested(b *strings.Builder, plan RunPlan, sandbox bool) {
	if plan.Session != nil {
		fmt.Fprintf(b, "\n\n## Skill creation\nThis conversation is a guided skill creation session: use the %s skill (call load_skill %s first) to interview the user and build the skill. %s",
			SkillCreatorSlug, SkillCreatorSlug, SkillCreationInstruction)
	}
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
		fmt.Fprintf(b, "\n\n## Skill %q (requested by the user for this message; %s)\n\n%s", a.Skill.Name, where, body)
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
		b.WriteString("The following marketplace skills are attached to this request and are available inside the workspace.\n")
		b.WriteString("Apply them in the exact order shown below.\n\n")
		for _, a := range plan.Attached {
			name := a.Skill.DisplayName
			if name == "" {
				name = a.Skill.Name
			}
			desc := strings.TrimSpace(a.Skill.Description)
			if a.Skill.Untrusted() {
				// Uploaded text: its fenced description is in the list above.
				name, desc = a.Skill.Name, ""
			}
			version := strings.TrimSpace(a.VersionLabel)
			if version == "" {
				version = "latest"
			}
			fmt.Fprintf(&b, "- %s @ %s\n", oneLine(name), version)
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
