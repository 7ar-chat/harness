package skills

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/GhaziBenDahmane/harness/config"
)

// Folder names other agent tools keep their skills in (the Agent Skills
// convention: <dir>/<skill>/SKILL.md).
const (
	agentsDir = ".agents"
	claudeDir = ".claude"
)

// DiscoverOptions say which skill directories of the host to read.
type DiscoverOptions struct {
	// SkillsDir is the explicit user directory (SKILLS_DIR), read before the others.
	SkillsDir string
	// Home is the user's home directory ("" = os.UserHomeDir).
	Home string
	// ForeignUserDirs also reads ~/.agents/skills and ~/.claude/skills (the
	// skills the user's other agent tools use). Off for a server that serves
	// other people: the host's home is not theirs.
	ForeignUserDirs bool
	// ProjectRoot is the repository the agent works in ("" = no project skills).
	ProjectRoot string
}

// Dir is a skill directory and the source its skills get.
type Dir struct {
	Path   string
	Source string
}

// Dirs lists the directories to read, lowest priority first: later ones
// replace skills of the same name. Built-in and Options.ExtraSkills come
// before them; the project directories come last and are untrusted.
//
//	SKILLS_DIR, ~/.har/skills, ~/.agents/skills, ~/.claude/skills,
//	<repo>/.har/skills, <repo>/.agents/skills, <repo>/.claude/skills
func Dirs(o DiscoverOptions) []Dir {
	home := o.Home
	userDir := ""
	if home != "" {
		userDir = filepath.Join(home, config.DirName)
	} else if !config.NoUserDir() {
		userDir = config.UserDir()
		home, _ = os.UserHomeDir()
	}
	var out []Dir
	add := func(path, source string) {
		if strings.TrimSpace(path) != "" {
			out = append(out, Dir{path, source})
		}
	}
	add(o.SkillsDir, SourceUser)
	if userDir != "" {
		add(filepath.Join(userDir, config.SkillsFolder), SourceUser)
	}
	if home != "" {
		if o.ForeignUserDirs {
			add(filepath.Join(home, agentsDir, config.SkillsFolder), SourceUser)
			add(filepath.Join(home, claudeDir, config.SkillsFolder), SourceUser)
		}
	}
	if o.ProjectRoot != "" {
		for _, d := range []string{config.DirName, agentsDir, claudeDir} {
			add(filepath.Join(o.ProjectRoot, d, config.SkillsFolder), SourceProject)
		}
	}
	return dedupe(out)
}

func dedupe(in []Dir) []Dir {
	seen := map[string]bool{}
	var out []Dir
	for _, d := range in {
		key := filepath.Clean(d.Path)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, d)
	}
	return out
}

// Discover adds the skills of Dirs(o) to the registry and returns how many it found.
func (r *Registry) Discover(o DiscoverOptions) int {
	n := 0
	for _, d := range Dirs(o) {
		n += r.AddDir(d.Source, d.Path)
	}
	return n
}
