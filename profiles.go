package harness

import (
	"strings"

	"github.com/GhaziBenDahmane/harness/config"
)

// ProfileWork is the full feature set: the sandbox tools, skills, MCP, plan
// tools, project documents, artifacts, workspace checkpoints, titles,
// verification, goals and memory. It is what AGENT_PROFILE=work (or unset)
// runs. Code checks (AGENT_CODE_CHECKS) and permissions (local backend) apply
// when configured.
func ProfileWork() []Feature {
	return []Feature{
		Sandbox(), Codecheck(false),
		Skills(), MCP(), Plan(), ProjectDocs(),
		Artifacts(), Workspace(), Titles(), Verify(), Goals(), Memory(),
		ToolProfile(false), Permissions(false),
	}
}

// ProfileCoding is the lean coding agent (AGENT_PROFILE=coding): ProfileWork
// plus the explore sub-agent, the syntax checks of written files and the
// permission layer, with the tool allow-list that leaves out the document
// viewer, the skills (unless the user has some) and the memory.
func ProfileCoding() []Feature {
	return []Feature{
		Sandbox(), Codecheck(true), Explore(),
		Skills(), MCP(), Plan(), ProjectDocs(),
		Artifacts(), Workspace(), Titles(), Verify(), Goals(), Memory(),
		ToolProfile(true), Permissions(true),
	}
}

// ProfileMinimal is the smallest useful agent: the sandbox tools and the plan
// tools, with no artifacts, memory, skills, goals or titles.
func ProfileMinimal() []Feature {
	return []Feature{Sandbox(), Plan()}
}

// ProfileNamed returns the feature set of a profile name (AGENT_PROFILE):
// "coding", "minimal", anything else the work profile.
func ProfileNamed(name string) []Feature {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "coding":
		return ProfileCoding()
	case "minimal":
		return ProfileMinimal()
	}
	return ProfileWork()
}

// profileOf is the feature set of the configuration's AGENT_PROFILE.
func profileOf(cfg *config.Config) []Feature { return ProfileNamed(cfg.AgentProfile) }
