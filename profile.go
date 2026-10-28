package harness

import (
	"context"
	"strings"

	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/engine/agent"
	"github.com/GhaziBenDahmane/harness/engine/tools"
	"github.com/GhaziBenDahmane/harness/features/memory/memorytools"
	"github.com/GhaziBenDahmane/harness/features/skills"
	"github.com/GhaziBenDahmane/harness/store"
)

// builtinTools are the tools a profile governs; any other tool (MCP, web
// search, host tools) is always offered unless AGENT_TOOLS removes it by name.
var builtinTools = append([]string{"execute_command", "job_output", "job_kill", "read_file", "view_file",
	"search_files", "write_file", "edit_file", "list_files", "explore", "load_skill", "update_plan", "ask_user"},
	memorytools.Names...)

// toolGroups are the AGENT_TOOLS names standing for several tools.
var toolGroups = map[string][]string{"memory": memorytools.Names, "skills": {"load_skill"}}

// profile decides, per run, which built-in tools and prompt sections the
// agent gets. The work profile offers everything (unchanged behaviour); the
// coding profile leaves out the document viewer, the skills (unless the user
// installed or attached some) and the memory, which a coding agent rarely
// needs and which cost thousands of tokens in every request.
type profile struct {
	active  bool // false: nothing is filtered
	coding  bool
	allow   map[string]bool
	removed map[string]bool
	// skillsAuto offers load_skill and the skills index only when the run has
	// user skills (coding default).
	skillsAuto bool
	vision     bool
	reg        *skills.Registry
}

func newProfile(cfg *config.Config, reg *skills.Registry) *profile {
	p := &profile{reg: reg, allow: map[string]bool{}, removed: map[string]bool{},
		coding: strings.EqualFold(strings.TrimSpace(cfg.AgentProfile), "coding"), vision: len(cfg.LLMVisionModels) > 0}
	spec := strings.TrimSpace(cfg.AgentTools)
	p.active = p.coding || spec != ""
	for _, n := range builtinTools {
		p.allow[n] = true
	}
	if p.coding {
		for _, n := range []string{"view_file", "load_skill"} {
			delete(p.allow, n)
		}
		if !cfg.MemoryInCoding {
			for _, n := range memorytools.Names {
				delete(p.allow, n)
			}
		}
		if cfg.SkillsInCoding {
			p.allow["load_skill"] = true
		}
	}
	var bare, add []string
	for _, tok := range strings.Split(spec, ",") {
		tok = strings.TrimSpace(tok)
		switch {
		case tok == "":
		case tok[0] == '+':
			add = append(add, expand(tok[1:])...)
		case tok[0] == '-':
			for _, n := range expand(tok[1:]) {
				p.removed[n] = true
			}
		default:
			bare = append(bare, expand(tok)...)
		}
	}
	if len(bare) > 0 {
		p.allow = map[string]bool{}
		for _, n := range bare {
			p.allow[n] = true
		}
	}
	for _, n := range add {
		p.allow[n] = true
	}
	for n := range p.removed {
		delete(p.allow, n)
	}
	p.skillsAuto = p.coding && !p.allow["load_skill"] && !p.removed["load_skill"]
	return p
}

func expand(name string) []string {
	if g, ok := toolGroups[name]; ok {
		return g
	}
	return []string{name}
}

func isBuiltin(name string) bool {
	for _, n := range builtinTools {
		if n == name {
			return true
		}
	}
	return false
}

// hasUserSkills reports whether the run has a skill beyond the built-in ones:
// a skill from the user directory, the project or the host, or one requested for the run.
func (p *profile) hasUserSkills(ctx context.Context, run *store.Run) bool {
	if p.reg == nil {
		return false
	}
	plan, err := p.reg.Plan(ctx, run)
	if err != nil {
		return false
	}
	for _, a := range plan.Attached {
		if a.Explicit {
			return true
		}
	}
	for _, s := range plan.Catalog {
		if s.Source != skills.SourceBuiltin {
			return true
		}
	}
	return false
}

func (p *profile) allowTool(ctx context.Context, rc tools.RunContext, name string) bool {
	switch {
	case p.removed[name]:
		return false
	case !isBuiltin(name):
		return true
	case name == "view_file":
		return p.allow[name] && (!p.coding || p.vision)
	case p.allow[name]:
		return true
	case name == "load_skill" && p.skillsAuto:
		return p.hasUserSkills(ctx, &store.Run{ID: rc.RunID, ThreadID: rc.ThreadID, UserLogin: rc.Login})
	}
	return false
}

// Wrap applies the profile to the tool providers.
func (p *profile) Wrap(inner tools.Provider) tools.Provider {
	if !p.active {
		return inner
	}
	return tools.Filter{Inner: inner, Allow: p.allowTool}
}

// skillsSection drops the skills index when load_skill is not offered.
func (p *profile) skillsSection(s agent.PromptSection) agent.PromptSection {
	if !p.active {
		return s
	}
	return func(ctx context.Context, in agent.Input) string {
		if p.removed["load_skill"] || (!p.allow["load_skill"] && !(p.skillsAuto && p.hasUserSkills(ctx, in.Run))) {
			return ""
		}
		return s(ctx, in)
	}
}

// memorySection drops a memory prompt section when the memory tools are not offered.
func (p *profile) memorySection(s agent.PromptSection) agent.PromptSection {
	if !p.active {
		return s
	}
	return func(ctx context.Context, in agent.Input) string {
		if !p.allow["memory_search"] || p.removed["memory_search"] {
			return ""
		}
		return s(ctx, in)
	}
}
