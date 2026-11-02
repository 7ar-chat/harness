package config

// ToolProfile selects the built-in tools and prompt sections of a profile
// (profile.go of the harness package). The work profile offers everything; the
// coding profile leaves out what a coding agent rarely needs.
type ToolProfile struct {
	// AgentTools is AGENT_TOOLS: a comma list over the profile's default tools.
	// "+name" adds a tool, "-name" removes one, a bare "name" list replaces the
	// default set. "memory" stands for the memory_* tools and their prompt
	// section, "skills" for load_skill and the skills index. Tools that are not
	// built in (MCP, web search, host tools) stay unless removed.
	AgentTools string `env:"AGENT_TOOLS"`
	// SkillsInCoding keeps load_skill and the skills index in the coding
	// profile even when no skill was installed or attached.
	SkillsInCoding bool `env:"SKILLS_IN_CODING"`
	// MemoryInCoding keeps the memory tools and prompt in the coding profile
	// (off by default there; the work profile is unaffected).
	MemoryInCoding bool `env:"MEMORY_IN_CODING"`
}
