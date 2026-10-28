package permissions

import (
	"path"
	"path/filepath"
	"strings"
)

type pathClass int

const (
	pathInside  pathClass = iota // the workspace
	pathTmp                      // scratch space (/tmp)
	pathOutside                  // anywhere else
	pathUnknown                  // depends on a variable or substitution
)

type pathInfo struct {
	class pathClass
	// clean is the cleaned absolute path as the agent sees it (/workspace/...).
	clean string
	// protected is why the path must not be written ("" if it may).
	protected string
	// system: the path is the root, the home directory or a top-level system directory.
	system bool
	// root: the workspace root itself.
	root bool
}

const virtualWorkspace = "/workspace"

// systemDirs are top-level directories whose recursive removal or rewrite is catastrophic.
var systemDirs = map[string]bool{
	"/": true, "/bin": true, "/boot": true, "/dev": true, "/etc": true, "/home": true, "/lib": true, "/lib64": true,
	"/opt": true, "/proc": true, "/root": true, "/sbin": true, "/sys": true, "/usr": true, "/var": true,
	"/Users": true, "/System": true, "/Library": true, "/Applications": true, "/private": true, "/Volumes": true,
}

// classify places a path argument relative to the workspace. Relative paths
// are under /workspace (the commands' working directory).
func (p *Policy) classify(raw string) pathInfo {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsRune(raw, 0) {
		return pathInfo{class: pathUnknown}
	}
	expanded, ok := p.expandHome(raw)
	if !ok || strings.ContainsAny(expanded, "$`") {
		return pathInfo{class: pathUnknown}
	}
	// A glob is judged by its fixed prefix; the rest only matches inside it.
	trimmed := expanded
	if i := strings.IndexAny(trimmed, "*?[{"); i >= 0 {
		trimmed = trimmed[:i]
		if !strings.HasSuffix(trimmed, "/") && trimmed != "" {
			trimmed = path.Dir(trimmed) // "src/ab*" lives in "src"
		}
		if trimmed == "" {
			trimmed = "."
		}
	}
	abs := trimmed
	if !strings.HasPrefix(abs, "/") {
		abs = virtualWorkspace + "/" + abs
	}
	abs = path.Clean(abs)
	info := pathInfo{clean: abs}
	switch {
	case abs == virtualWorkspace || strings.HasPrefix(abs, virtualWorkspace+"/"):
		info.class = pathInside
		info.root = abs == virtualWorkspace
	case p.Workspace != "" && (abs == p.Workspace || strings.HasPrefix(abs, p.Workspace+"/")):
		info.class = pathInside
		info.root = abs == p.Workspace
		info.clean = virtualWorkspace + strings.TrimPrefix(abs, p.Workspace)
	case abs == "/tmp" || strings.HasPrefix(abs, "/tmp/") || abs == "/private/tmp" || strings.HasPrefix(abs, "/private/tmp/") ||
		strings.HasPrefix(abs, "/var/folders/") || strings.HasPrefix(abs, "/private/var/folders/"):
		info.class = pathTmp
	default:
		info.class = pathOutside
	}
	if info.class == pathOutside {
		info.system = systemDirs[abs] || (p.Home != "" && abs == path.Clean(filepath.ToSlash(p.Home)))
	}
	info.protected = protectedReason(info.clean)
	return info
}

// expandHome expands ~ and $HOME; ok is false for another variable.
func (p *Policy) expandHome(s string) (string, bool) {
	home := filepath.ToSlash(p.Home)
	switch {
	case s == "~" || strings.HasPrefix(s, "~/"):
		if home == "" {
			return "", false
		}
		return home + s[1:], true
	case s == "$HOME" || strings.HasPrefix(s, "$HOME/"):
		if home == "" {
			return "", false
		}
		return home + s[5:], true
	case s == "${HOME}" || strings.HasPrefix(s, "${HOME}/"):
		if home == "" {
			return "", false
		}
		return home + s[7:], true
	}
	return s, true
}

// secretNames are file names that hold credentials.
var secretGlobs = []string{
	".env", ".env.*", "id_rsa", "id_rsa.*", "id_dsa", "id_ecdsa", "id_ed25519", "id_ed25519.*", "*.pem", "*.key", "*.p12", "*.pfx",
	".netrc", ".npmrc", ".pypirc", "credentials", "credentials.json", "secrets.yml", "secrets.yaml", "*.keystore",
}

// secretExempt are look-alikes that hold no secret.
var secretExempt = []string{".env.example", ".env.sample", ".env.template", ".env.dist", "*.pub", "id_*.pub"}

// protectedReason says why a path may not be written without an explicit allow rule.
func protectedReason(clean string) string {
	for _, el := range strings.Split(clean, "/") {
		if el == ".git" {
			return "it is inside .git (git internals)"
		}
	}
	base := path.Base(clean)
	for _, g := range secretExempt {
		if globPath(g, base) {
			return ""
		}
	}
	for _, g := range secretGlobs {
		if globPath(g, base) {
			return "it looks like a secret or credential file (" + base + ")"
		}
	}
	if strings.Contains(clean, "/.ssh/") || strings.Contains(clean, "/.aws/") || strings.Contains(clean, "/.gnupg/") {
		return "it is in a credentials directory"
	}
	return ""
}
