package policy

import (
	"strings"
)

// rule is one parsed Config pattern: Name(glob) or a bare tool-name glob.
type rule struct {
	raw  string
	name string // bash, write, read, tool
	glob string
}

func parseRules(patterns []string) []rule {
	var out []rule
	for _, raw := range patterns {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		r := rule{raw: raw, name: "tool", glob: raw}
		if i := strings.IndexByte(raw, '('); i > 0 && strings.HasSuffix(raw, ")") {
			r.glob = raw[i+1 : len(raw)-1]
			switch strings.ToLower(raw[:i]) {
			case "bash", "shell", "exec":
				r.name = "bash"
			case "write", "edit":
				r.name = "write"
			case "read":
				r.name = "read"
			default:
				r.name = "tool"
			}
		}
		out = append(out, r)
	}
	return out
}

// configMatch returns the raw pattern of the first rule the call matches ("" none).
func (e *Engine) configMatch(rules []rule, toolName string, a Action) string {
	for _, r := range rules {
		if r.matches(toolName, a) {
			return r.raw
		}
	}
	return ""
}

func (r rule) matches(toolName string, a Action) bool {
	switch r.name {
	case "tool":
		return globMatch(r.glob, toolName)
	case "bash":
		if toolName != "execute_command" {
			return false
		}
		if globMatch(r.glob, strings.TrimSpace(a.Command)) {
			return true
		}
		for _, c := range commandTexts(a.Command, 0) {
			if globMatch(r.glob, c) {
				return true
			}
		}
	case "write":
		if toolName == "write_file" || toolName == "edit_file" || toolName == "execute_command" {
			return r.matchPaths(a, a.Kind != KindRead)
		}
	case "read":
		if readTools[toolName] {
			return r.matchPaths(a, true)
		}
	}
	return false
}

func (r rule) matchPaths(a Action, ok bool) bool {
	if !ok {
		return false
	}
	for _, p := range a.Paths {
		if globMatch(r.glob, p) {
			return true
		}
		if abs, known := resolve(p); known {
			if globMatch(r.glob, abs) || globMatch(r.glob, relToRepo(abs)) {
				return true
			}
			if !strings.Contains(r.glob, "/") && globMatch(r.glob, baseName(abs)) {
				return true
			}
		}
	}
	return false
}

func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}
