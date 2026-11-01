package policy

import (
	"path"
	"regexp"
	"strings"
	"sync"
)

// The sandbox layout a factory task runs in: the repository is checked out in
// workDir under workspaceRoot.
const (
	workspaceRoot = "/workspace"
	workDir       = "/workspace/repo"
	homeMarker    = "/~home" // stands for the user's home directory, which is outside the workspace
)

// resolve turns a path as the agent wrote it into an absolute one. Relative
// paths resolve under workDir; "~", "$HOME" and "${HOME}" resolve under
// homeMarker. known is false for paths holding any other expansion.
func resolve(p string) (abs string, known bool) {
	p = strings.TrimSpace(p)
	for _, h := range []string{"~", "${HOME}", "$HOME"} {
		if p == h || strings.HasPrefix(p, h+"/") {
			return path.Clean(homeMarker + strings.TrimPrefix(p, h)), true
		}
	}
	if strings.ContainsAny(p, "$`") {
		return "", false
	}
	if p == "" {
		return workDir, true
	}
	if strings.HasPrefix(p, "/") {
		return path.Clean(p), true
	}
	return path.Join(workDir, p), true
}

func inWorkspace(abs string) bool {
	return abs == workspaceRoot || strings.HasPrefix(abs, workspaceRoot+"/")
}

// scratch reports a path commands may write outside the workspace: temporary
// files and the standard devices.
func scratch(abs string) bool {
	for _, d := range []string{"/tmp", "/var/tmp", "/dev"} {
		if strings.HasPrefix(abs, d+"/") {
			return true
		}
	}
	return false
}

// relToRepo is abs relative to the repository (or the workspace when it is
// not inside the repository).
func relToRepo(abs string) string {
	for _, root := range []string{workDir, workspaceRoot} {
		if abs == root {
			return "."
		}
		if strings.HasPrefix(abs, root+"/") {
			return abs[len(root)+1:]
		}
	}
	return strings.TrimPrefix(abs, "/")
}

var globCache sync.Map // key -> *regexp.Regexp

// globRegexp compiles a glob. With cross, "*" and "?" also match "/" (the
// config rules); without it only "**" spans directories (the protected paths).
// A trailing "/**" also matches the directory itself.
func globRegexp(pat string, cross bool) *regexp.Regexp {
	key := pat
	if cross {
		key = "x:" + pat
	}
	if v, ok := globCache.Load(key); ok {
		return v.(*regexp.Regexp)
	}
	tail := ""
	if strings.HasSuffix(pat, "/**") {
		pat = strings.TrimSuffix(pat, "/**")
		tail = "(?:/.*)?"
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pat); i++ {
		switch c := pat[i]; c {
		case '*':
			if i+1 < len(pat) && pat[i+1] == '*' {
				i++
				if i+1 < len(pat) && pat[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else if cross {
				b.WriteString(".*")
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			if cross {
				b.WriteString(".")
			} else {
				b.WriteString("[^/]")
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString(tail + "$")
	re := regexp.MustCompile("(?s)" + b.String())
	globCache.Store(key, re)
	return re
}

// globMatch is the config rules' glob: "*" matches anything, "/" included.
func globMatch(pat, s string) bool { return globRegexp(pat, true).MatchString(s) }

// matchProtected reports the first protected pattern p matches. A pattern
// without "/" matches the basename at any depth; one with "/" matches the
// path relative to the repository (at any depth below the workspace).
func matchProtected(patterns []string, p string) (string, bool) {
	abs, known := resolve(p)
	if !known {
		return "", false
	}
	rel := relToRepo(abs)
	for _, pat := range patterns {
		switch {
		case strings.HasPrefix(pat, "/"):
			if globRegexp(pat, false).MatchString(abs) {
				return pat, true
			}
		case !strings.Contains(pat, "/"):
			if globRegexp(pat, false).MatchString(path.Base(rel)) {
				return pat, true
			}
		default:
			if globRegexp(pat, false).MatchString(rel) ||
				(!strings.HasPrefix(pat, "**/") && globRegexp("**/"+pat, false).MatchString(rel)) {
				return pat, true
			}
		}
	}
	return "", false
}

// secretPath reports whether p names a file that holds credentials: SSH
// keys, git and netrc credentials, cloud credentials, process environments,
// the credentials the sandbox keeps under .work.
func secretPath(p string) bool {
	p = strings.TrimSpace(p)
	if strings.HasPrefix(p, "-") { // --flag=path
		if i := strings.IndexByte(p, '='); i >= 0 {
			p = p[i+1:]
		} else {
			return false
		}
	}
	p = strings.TrimPrefix(p, "@") // curl -d @file
	if p == "" {
		return false
	}
	segs := strings.Split(path.Clean(p), "/")
	base := segs[len(segs)-1]
	switch base {
	case ".git-credentials", ".netrc", "_netrc", ".pgpass", ".npmrc", ".pypirc", ".dockercfg", ".vault-token":
		return true
	}
	for _, pre := range []string{"id_rsa", "id_ed25519", "id_ecdsa", "id_dsa"} {
		if strings.HasPrefix(base, pre) && !strings.HasSuffix(base, ".pub") {
			return true
		}
	}
	hasSeg := func(name string) bool {
		for _, s := range segs[:len(segs)-1] {
			if s == name {
				return true
			}
		}
		return false
	}
	switch {
	case base == ".ssh" || hasSeg(".ssh"):
		return true
	case base == "credentials" && hasSeg(".aws"):
		return true
	case base == "environ" && len(segs) >= 3 && segs[0] == "" && segs[1] == "proc":
		return true
	// The sandbox keeps the git credentials of the task's repository in .work (git-credentials): the agent
	// pushes through git's credential helper and must never read the token itself.
	case hasSeg(".work") && strings.Contains(strings.ToLower(base), "cred"):
		return true
	case base == "auth.json" && hasSeg(".har"): // the user's model API keys
		return true
	case base == "hosts.yml" && hasSeg("gh"), base == "hosts.yml" && hasSeg(".config"): // the GitHub CLI's token
		return true
	case base == "config.json" && hasSeg(".docker"), base == "config" && hasSeg(".kube"):
		return true
	case base == "credentials" || base == "credentials.db" || base == "access_tokens.db":
		return hasSeg(".config") || hasSeg(".aws") || hasSeg("gcloud")
	}
	return false
}

// secretText reports whether free text (an interpreter's program) names a
// credentials file.
func secretText(s string) bool {
	for _, m := range []string{"/.ssh/", ".git-credentials", ".netrc", ".aws/credentials", "/environ", "id_rsa",
		".work/git-credentials", "gh-credentials", ".har/auth.json", "gh/hosts.yml", ".docker/config.json", ".npmrc", ".pypirc",
		"find-generic-password", "find-internet-password", "dump-keychain"} {
		if strings.Contains(s, m) {
			if m == "/environ" && !strings.Contains(s, "/proc/") {
				continue
			}
			return true
		}
	}
	return false
}
