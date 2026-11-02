package policy

import (
	"net/url"
	"path"
	"strings"
)

// gitAction classifies a git command line (its arguments after "git").
func gitAction(args []word) Action {
	for i := 0; i < len(args); i++ {
		t := args[i].text
		if args[i].dyn && !strings.HasPrefix(t, "-") {
			return Action{Kind: KindGitPush, Branches: []string{unknownBranch}, Detail: "git subcommand built from an expansion"}
		}
		switch {
		case t == "-c" && i+1 < len(args):
			i++
			if v := args[i].text; strings.HasPrefix(v, "alias.") && strings.Contains(v, "push") {
				return Action{Kind: KindGitPush, Branches: []string{unknownBranch}, Detail: "git push behind an alias"}
			}
		case t == "-C" || t == "--git-dir" || t == "--work-tree":
			// git acting on another directory or repository: only the workspace is the agent's to operate on.
			if i+1 < len(args) {
				if v := args[i+1]; v.dyn || !inWorkspaceArg(v.text) {
					return Action{Kind: KindOutsideWorkspace, Detail: "git " + t + " " + v.text + " operates outside the workspace"}
				}
			}
			i++
		case strings.HasPrefix(t, "--git-dir=") || strings.HasPrefix(t, "--work-tree="):
			if v := t[strings.IndexByte(t, '=')+1:]; !inWorkspaceArg(v) {
				return Action{Kind: KindOutsideWorkspace, Detail: "git " + t + " operates outside the workspace"}
			}
		case t == "--namespace" || t == "--exec-path" || t == "--super-prefix":
			i++
		case strings.HasPrefix(t, "-"):
		case t == "push":
			return gitPush(args[i+1:])
		case t == "worktree":
			// A clone can be a linked worktree of another repository; `worktree add/remove/move/prune` then writes
			// into that repository's .git. The agent has no use for worktrees: it has a checkout.
			return Action{Kind: KindOutsideWorkspace, Detail: "git worktree changes repositories outside the checkout"}
		case t == "credential" || t == "credential-store" || t == "credential-cache":
			return Action{Kind: KindSecrets, Detail: "git " + t + " reads stored credentials"}
		default:
			return Action{Kind: KindExec}
		}
	}
	return Action{Kind: KindExec}
}

// gitPush classifies the arguments of "git push".
func gitPush(args []word) Action {
	f := parseArgs(args, map[string]bool{"--repo": true, "-o": true, "--push-option": true, "--receive-pack": true, "--exec": true})
	a := Action{Kind: KindGitPush, Detail: "git push"}
	if f.short("fd") || f.has("--force", "--delete", "--mirror", "--all", "--prune", "--force-if-includes") {
		a.Kind, a.Detail = KindGitForcePush, "forced or destructive git push"
	}
	for _, fl := range f.flags {
		if strings.HasPrefix(fl, "--force") {
			a.Kind, a.Detail = KindGitForcePush, "forced git push"
		}
	}
	if len(f.pos) > 1 {
		for _, w := range f.pos[1:] {
			if strings.HasPrefix(w.text, "+") {
				a.Kind, a.Detail = KindGitForcePush, "git push of a +refspec"
			}
			if strings.HasPrefix(w.text, ":") && len(w.text) > 1 {
				a.Kind, a.Detail = KindGitForcePush, "git push that deletes a remote branch"
			}
			if w.dyn {
				a.Branches = append(a.Branches, unknownBranch)
				continue
			}
			dst := w.text
			if i := strings.LastIndexByte(dst, ':'); i >= 0 {
				dst = dst[i+1:]
			}
			dst = strings.TrimPrefix(strings.TrimPrefix(dst, "+"), "refs/heads/")
			if dst == "" || dst == "HEAD" {
				continue // the current branch
			}
			a.Branches = append(a.Branches, dst)
		}
	}
	return a
}

// ghAction classifies gh, glab and hub.
func ghAction(name string, args []word) Action {
	f := parseArgs(args, map[string]bool{"-X": true, "--method": true, "-H": true, "--header": true, "-R": true, "--repo": true,
		"-f": true, "-F": true, "--field": true, "--raw-field": true, "--input": true, "-q": true, "--jq": true, "-t": true, "--template": true,
		"-b": true, "--body": true, "-t2": true, "--title": true})
	pos := make([]string, len(f.pos))
	for i, w := range f.pos {
		pos[i] = w.text
	}
	at := func(i int) string {
		if i < len(pos) {
			return pos[i]
		}
		return ""
	}
	switch {
	case (at(0) == "pr" || at(0) == "mr") && at(1) == "merge":
		return Action{Kind: KindMerge, Detail: name + " " + at(0) + " merge"}
	case at(0) == "release" && (at(1) == "create" || at(1) == "upload"):
		return Action{Kind: KindPublish, Detail: name + " release " + at(1)}
	case at(0) == "repo" && at(1) == "delete":
		return Action{Kind: KindDestructive, Detail: name + " repo delete"}
	case at(0) == "auth" && (at(1) == "token" || at(1) == "login" || at(1) == "setup-git"):
		return Action{Kind: KindSecrets, Detail: name + " auth " + at(1) + " handles credentials"}
	case at(0) == "api":
		return ghAPI(f, pos)
	}
	return Action{Kind: KindNet}
}

func ghAPI(f argv, pos []string) Action {
	method := "GET"
	for _, k := range []string{"-X", "--method"} {
		for _, v := range f.vals[k] {
			method = strings.ToUpper(v.text)
		}
	}
	fields := f.has("-f", "-F", "--field", "--raw-field", "--input")
	if method == "GET" && fields {
		method = "POST"
	}
	for _, p := range pos[min(1, len(pos)):] {
		if mergeAPIRe.MatchString(p) && (method == "PUT" || method == "POST") {
			return Action{Kind: KindMerge, Detail: "merges a pull request through the API"}
		}
		if i := strings.Index(p, "/git/refs/heads/"); i >= 0 && method != "GET" {
			a := Action{Kind: KindGitPush, Branches: []string{p[i+len("/git/refs/heads/"):]}, Detail: "updates a branch through the API"}
			for _, k := range []string{"-f", "-F", "--field", "--raw-field"} {
				for _, v := range f.vals[k] {
					if v.text == "force=true" {
						a.Kind = KindGitForcePush
					}
				}
			}
			if method == "DELETE" {
				a.Kind = KindGitForcePush
			}
			return a
		}
	}
	for _, k := range []string{"-f", "-F", "--field", "--raw-field", "--input"} {
		for _, v := range f.vals[k] {
			if strings.Contains(v.text, "mergePullRequest") || strings.Contains(v.text, "enablePullRequestAutoMerge") {
				return Action{Kind: KindMerge, Detail: "merges a pull request through GraphQL"}
			}
		}
	}
	return Action{Kind: KindNet}
}

// netAction classifies curl, wget and similar.
func netAction(name string, args []word) Action {
	f := parseArgs(args, map[string]bool{"-X": true, "--request": true, "--method": true, "-o": true, "--output": true,
		"-O": true, "-P": true, "--output-document": true, "--directory-prefix": true, "-H": true, "--header": true,
		"-d": true, "--data": true, "--data-raw": true, "--data-binary": true, "--data-urlencode": true,
		"-u": true, "--user": true, "-A": true, "--user-agent": true, "-e": true, "--referer": true, "-F": true,
		"--form": true, "-T": true, "--upload-file": true, "--json": true, "-x": true, "--proxy": true, "--post-data": true, "--post-file": true})
	a := Action{Kind: KindNet}
	method := ""
	for _, k := range []string{"-X", "--request", "--method"} {
		for _, v := range f.vals[k] {
			method = strings.ToUpper(v.text)
		}
	}
	data := f.has("-d", "--data", "--data-raw", "--data-binary", "--data-urlencode", "-F", "--form", "--json", "--post-data", "--post-file")
	if method == "" && data {
		method = "POST"
	}
	for _, p := range f.pos {
		u, err := url.Parse(p.text)
		if err != nil || !strings.EqualFold(u.Hostname(), "api.github.com") {
			continue
		}
		if mergeAPIRe.MatchString(u.Path) && (method == "PUT" || method == "POST") {
			return Action{Kind: KindMerge, Detail: name + " merges a pull request through the GitHub API"}
		}
	}
	for _, k := range []string{"-o", "--output", "-O", "--output-document", "-P", "--directory-prefix"} {
		for _, v := range f.vals[k] {
			if v.text != "-" {
				a = merge(a, writeTarget(v))
			}
		}
	}
	for _, k := range []string{"-d", "--data", "--data-raw", "--data-binary", "-T", "--upload-file", "--post-file", "-F", "--form"} {
		for _, v := range f.vals[k] {
			if i := strings.Index(v.text, "@"); i >= 0 && secretPath(v.text[i:]) {
				a = merge(a, Action{Kind: KindSecrets, Detail: name + " uploads credentials: " + v.text})
			}
			if k == "-T" || k == "--upload-file" {
				if secretPath(v.text) {
					a = merge(a, Action{Kind: KindSecrets, Detail: name + " uploads credentials: " + v.text})
				}
			}
		}
	}
	return a
}

// publishAction recognises package and image publication.
func publishAction(name string, args []word) (Action, bool) {
	f := parseArgs(args, map[string]bool{"-C": true, "--cwd": true, "--prefix": true, "-w": true, "--workspace": true})
	var sub []string
	for i, w := range f.pos {
		if i >= 3 {
			break
		}
		sub = append(sub, w.text)
	}
	has := func(s ...string) bool {
		for _, x := range sub {
			for _, y := range s {
				if x == y {
					return true
				}
			}
		}
		return false
	}
	pub := false
	switch name {
	case "npm", "yarn", "pnpm", "bun", "cargo", "poetry", "uv":
		pub = has("publish")
	case "twine":
		pub = has("upload")
	case "gem":
		pub = has("push")
	case "docker", "podman", "buildah":
		pub = has("push") || f.has("--push")
	case "helm":
		pub = has("push", "push-artifactory")
	case "dotnet":
		pub = has("push")
	case "mvn":
		pub = has("deploy")
	}
	if !pub {
		return Action{}, false
	}
	return Action{Kind: KindPublish, Detail: name + " publishes an artifact"}, true
}

var dangerousTargets = map[string]bool{"/": true, workspaceRoot: true, workDir: true, homeMarker: true, "/workspace/.work": true}

// dangerousRm reports whether recursively removing p wipes the workspace, the
// machine or something unknowable.
func dangerousRm(w word) (string, bool) {
	t := w.text
	if w.dyn && !strings.HasPrefix(t, "$HOME") && !strings.HasPrefix(t, "${HOME}") {
		return "a path built from an expansion", true
	}
	abs, known := resolve(t)
	if !known {
		return "a path built from an expansion", true
	}
	base := path.Base(abs)
	if strings.ContainsAny(base, "*?[") {
		if dir := path.Dir(abs); dangerousTargets[dir] || !inWorkspace(dir) && !scratch(dir+"/") {
			return t, true
		}
		return "", false
	}
	if dangerousTargets[abs] || strings.HasPrefix(abs, homeMarker) || (!inWorkspace(abs) && !scratch(abs)) {
		return t, true
	}
	return "", false
}

func rmAction(args []word) Action {
	f := parseArgs(args, nil)
	if f.has("--no-preserve-root") {
		return Action{Kind: KindDestructive, Detail: "rm --no-preserve-root"}
	}
	recursive := f.short("rR") || f.has("--recursive")
	res := Action{Kind: KindRead}
	for _, w := range f.pos {
		if recursive {
			if why, bad := dangerousRm(w); bad {
				return Action{Kind: KindDestructive, Detail: "recursive removal of " + why}
			}
		}
		res = merge(res, writeTarget(w))
	}
	return res
}

func findAction(args []word, depth int) Action {
	var starts []word
	i := 0
	for ; i < len(args); i++ {
		t := args[i].text
		if strings.HasPrefix(t, "-") || t == "(" || t == "!" {
			break
		}
		starts = append(starts, args[i])
	}
	if len(starts) == 0 {
		starts = []word{{text: "."}}
	}
	res := Action{Kind: KindRead}
	for ; i < len(args); i++ {
		switch args[i].text {
		case "-delete":
			for _, s := range starts {
				if why, bad := dangerousRm(s); bad {
					return Action{Kind: KindDestructive, Detail: "find -delete on " + why}
				}
				res = merge(res, writeTarget(s))
			}
		case "-exec", "-execdir", "-ok", "-okdir":
			j := i + 1
			for j < len(args) && args[j].text != ";" && args[j].text != "+" {
				j++
			}
			res = merge(res, cmdAction(args[i+1:j], depth+1, false))
			i = j
		case "-fprint", "-fprint0", "-fprintf", "-fls":
			if i+1 < len(args) {
				res = merge(res, writeTarget(args[i+1]))
			}
		}
	}
	return res
}

func chmodAction(name string, args []word) Action {
	f := parseArgs(args, nil)
	recursive := f.short("R") || f.has("--recursive")
	if len(f.pos) == 0 {
		return Action{Kind: KindExec}
	}
	mode := f.pos[0].text
	if recursive && name == "chmod" && (mode == "777" || mode == "0777" || strings.Contains(mode, "a+rwx") || mode == "ugo+rwx" || mode == "o+w" || mode == "a+w") {
		return Action{Kind: KindDestructive, Detail: "chmod -R " + mode + " opens permissions to everyone"}
	}
	res := Action{Kind: KindExec}
	for _, w := range f.pos[1:] {
		if recursive {
			if why, bad := dangerousRm(w); bad {
				return Action{Kind: KindDestructive, Detail: name + " -R on " + why}
			}
		}
		res = merge(res, writeTarget(w))
	}
	return res
}

func writeCommand(name string, args []word) Action {
	f := parseArgs(args, map[string]bool{"-s": true, "--size": true, "-r": true, "--reference": true, "-d": true, "-t": true, "-m": true, "--mode": true})
	res := Action{Kind: KindRead}
	if name != "tee" {
		res.Kind = KindExec
	}
	for _, w := range f.pos {
		res = merge(res, writeTarget(w))
	}
	return res
}

func copyAction(name string, args []word) Action {
	f := parseArgs(args, map[string]bool{"-t": true, "--target-directory": true, "-S": true, "--suffix": true, "-m": true, "--mode": true})
	res := Action{Kind: KindExec}
	var targets []word
	for _, k := range []string{"-t", "--target-directory"} {
		targets = append(targets, f.vals[k]...)
	}
	if len(targets) == 0 && len(f.pos) > 0 {
		targets = f.pos[len(f.pos)-1:]
	}
	if name == "mv" {
		targets = append(targets, f.pos...)
	}
	for _, w := range targets {
		res = merge(res, writeTarget(w))
	}
	return res
}

func sedAction(args []word) Action {
	f := parseArgs(args, map[string]bool{"-e": true, "-f": true, "--expression": true, "--file": true, "-l": true})
	inPlace := f.short("i") || f.has("--in-place")
	for _, fl := range f.flags {
		if strings.HasPrefix(fl, "--in-place") {
			inPlace = true
		}
	}
	if !inPlace {
		return Action{Kind: KindRead}
	}
	files := f.pos
	if len(f.vals["-e"])+len(f.vals["--expression"])+len(f.vals["-f"])+len(f.vals["--file"]) == 0 && len(files) > 0 {
		files = files[1:]
	}
	res := Action{Kind: KindWriteWorkspace}
	for _, w := range files {
		res = merge(res, writeTarget(w))
	}
	return res
}

func perlAction(args []word, fetch bool) Action {
	res := interpreterAction("perl", args, fetch)
	f := parseArgs(args, map[string]bool{"-e": true, "-E": true, "-I": true, "-M": true, "-m": true, "-F": true})
	if !f.short("i") {
		return res
	}
	if res.Kind == KindExec { // an in-place edit is a write; the paths below are checked against the protected ones
		res.Kind = KindWriteWorkspace
	}
	files := f.pos
	if len(f.vals["-e"])+len(f.vals["-E"]) == 0 && len(files) > 0 {
		files = files[1:]
	}
	for _, w := range files {
		res = merge(res, writeTarget(w))
	}
	return res
}

func ddAction(args []word) Action {
	for _, w := range args {
		if of, ok := strings.CutPrefix(w.text, "of="); ok {
			if strings.HasPrefix(of, "/dev/") && of != "/dev/null" && of != "/dev/stdout" && of != "/dev/stderr" {
				return Action{Kind: KindDestructive, Detail: "dd writes to the device " + of}
			}
			return writeTarget(word{text: of, dyn: w.dyn})
		}
	}
	return Action{Kind: KindExec}
}

// inWorkspaceArg reports whether a path given to git -C / --git-dir / --work-tree stays in the workspace: a relative
// path without "..", or an absolute one under /workspace.
func inWorkspaceArg(p string) bool {
	p = strings.TrimSpace(p)
	if p == "" || strings.HasPrefix(p, "~") || strings.HasPrefix(p, "$") {
		return false
	}
	if !strings.HasPrefix(p, "/") {
		return !strings.Contains("/"+path.Clean(p)+"/", "/../")
	}
	return inWorkspace(path.Clean(p))
}
