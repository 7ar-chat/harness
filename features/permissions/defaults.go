package permissions

import (
	"regexp"
	"strings"
)

// readOnlyProgs never change anything (their arguments are checked where a flag
// can make them write: find, sort, sed, env).
var readOnlyProgs = setOf("ls", "cat", "head", "tail", "wc", "grep", "egrep", "fgrep", "rg", "ag", "tree", "stat", "file", "pwd", "echo",
	"printf", "true", "false", "test", "[", "[[", "date", "uname", "whoami", "id", "hostname", "which", "type", "whereis", "basename",
	"dirname", "realpath", "readlink", "du", "df", "uniq", "cut", "tr", "diff", "cmp", "comm", "jq", "awk", "xxd", "hexdump", "od",
	"strings", "nl", "column", "fold", "tac", "rev", "seq", "sleep", "printenv", "ps", "lsof", "cd", "pushd", "popd", "export",
	"unset", "set", "md5sum", "sha1sum", "sha256sum", "shasum", "cksum", "less", "more", "man", "expr", "bc", "paste", "join", "expand",
	"unexpand", "nproc", "arch", "uptime", "free", "locale", "tput", "clear", ":", "exit", "return", "wait", "declare", "local",
	"typeset", "shift", "break", "continue", "read", "let", "ulimit", "umask", "alias", "hash", "help", "history", "jobs", "fg", "bg",
	"mapfile", "readarray", "getopts", "tty", "groups", "logname", "yes", "tsort", "diff3", "sdiff", "gofmt")

// buildProgs run a project's own build, test or lint tooling.
var buildProgs = setOf("make", "gmake", "ninja", "just", "ctest", "pytest", "py.test", "tox", "nox", "ruff", "mypy", "flake8", "pylint",
	"black", "isort", "pyright", "tsc", "eslint", "prettier", "jest", "vitest", "mocha", "rspec", "rubocop", "rustfmt", "goimports",
	"golangci-lint", "staticcheck", "shellcheck", "gotestsum", "clang-format", "cmake")

var downloaders = setOf("curl", "wget", "http", "https", "xh", "aria2c", "fetch", "lynx", "links")

var networkProgs = setOf("ssh", "scp", "sftp", "rsync", "nc", "ncat", "netcat", "telnet", "ftp", "socat", "nmap", "ping", "traceroute",
	"dig", "nslookup", "ssh-keygen", "ssh-add", "mosh")

var shells = setOf("sh", "bash", "zsh", "dash", "ksh", "ash", "fish", "python", "python2", "python3", "node", "perl", "ruby", "php", "deno", "bun", "pwsh", "sudo", "doas", "source")

var pathMutators = setOf("rm", "rmdir", "mkdir", "touch", "cp", "mv", "ln", "install", "tee", "truncate", "chmod", "chown", "chgrp", "unlink")

var systemCmds = setOf("shutdown", "reboot", "halt", "poweroff", "init", "systemctl", "service", "launchctl", "kill", "pkill", "killall",
	"mount", "umount", "fdisk", "parted", "sfdisk", "diskutil", "crontab", "useradd", "userdel", "usermod", "passwd", "visudo", "iptables",
	"ufw", "sysctl", "mkswap", "swapon", "swapoff", "losetup", "cryptsetup", "chroot", "su")

var packageProgs = setOf("apt", "apt-get", "aptitude", "yum", "dnf", "brew", "port", "apk", "pacman", "zypper", "snap", "flatpak", "choco",
	"scoop", "winget", "gem", "bundle", "composer", "poetry", "pipx", "uv", "conda", "mamba", "nix", "nix-env", "asdf", "mise", "rustup", "nvm", "sdk")

func setOf(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, i := range items {
		m[i] = true
	}
	return m
}

// splitArgs separates flags from positional arguments ("--" ends the flags).
func splitArgs(args []string) (flags, pos []string) {
	for i, a := range args {
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			return
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			flags = append(flags, a)
		} else {
			pos = append(pos, a)
		}
	}
	return
}

func hasFlag(flags []string, short string, long ...string) bool {
	for _, f := range flags {
		if strings.HasPrefix(f, "--") {
			for _, l := range long {
				if f == l || strings.HasPrefix(f, l+"=") {
					return true
				}
			}
			continue
		}
		if short != "" && strings.ContainsAny(f[1:], short) && !strings.ContainsAny(f[1:], "0123456789=") {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

var mainBranch = regexp.MustCompile(`(^|[:/+])(main|master)$`)

// hardDeny reports the catastrophic commands no rule can allow.
func (p *Policy) hardDeny(s Simple) (Decision, bool) {
	deny := func(why string) (Decision, bool) { return Decision{Action: Deny, Hard: true, Reason: why}, true }
	prog := s.Prog()
	flags, pos := splitArgs(s.Words[1:])
	switch {
	case prog == "rm":
		if hasFlag(flags, "rR", "--recursive") || hasFlag(flags, "", "--no-preserve-root") {
			for _, t := range pos {
				if info := p.classify(t); info.class == pathOutside && info.system {
					return deny("rm -rf of " + t + " would destroy a system or home directory")
				}
			}
			if hasFlag(flags, "", "--no-preserve-root") {
				return deny("rm --no-preserve-root")
			}
		}
	case prog == "chmod" || prog == "chown" || prog == "chgrp":
		if hasFlag(flags, "R", "--recursive") && len(pos) > 1 {
			for _, t := range pos[1:] {
				if info := p.classify(t); info.class == pathOutside && info.system {
					return deny(prog + " -R of " + t + " would break a system or home directory")
				}
			}
		}
	case strings.HasPrefix(prog, "mkfs") || prog == "wipefs" || prog == "shred" || prog == "blkdiscard":
		for _, a := range s.Words[1:] {
			if blockDevice.MatchString(a) {
				return deny(prog + " on the device " + a)
			}
		}
	case prog == "dd":
		for _, a := range s.Words[1:] {
			if strings.HasPrefix(a, "of=") && blockDevice.MatchString(strings.TrimPrefix(a, "of=")) {
				return deny("dd onto the device " + strings.TrimPrefix(a, "of="))
			}
		}
	case prog == "git":
		if sub, rest := gitSub(s.Words[1:]); sub == "push" {
			f, pos := splitArgs(rest)
			force := hasFlag(f, "f", "--force", "--force-with-lease", "--force-if-includes", "--mirror")
			for _, a := range pos {
				if strings.HasPrefix(a, "+") {
					force = true
				}
			}
			if force {
				for _, a := range pos {
					if mainBranch.MatchString(a) {
						return deny("git push --force to " + a + " rewrites shared history")
					}
				}
			}
		}
	}
	return Decision{}, false
}

// gitSub finds the subcommand of a git command line (after global options).
func gitSub(args []string) (sub string, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-C" || a == "-c" || a == "--git-dir" || a == "--work-tree" || a == "--namespace":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			return a, args[i+1:]
		}
	}
	return "", nil
}

func ask(why string) Decision { return Decision{Action: Ask, Reason: why} }
func allow(why string) Decision {
	return Decision{Action: Allow, Reason: why}
}
func allowRO(why string) Decision { return Decision{Action: Allow, Reason: why, ReadOnly: true} }

// builtin is the default policy for a command no user rule covers.
func (p *Policy) builtin(s Simple) Decision {
	d := p.builtinCommand(s)
	if s.Elevated && d.Action == Allow {
		return ask("runs with elevated privileges (sudo)")
	}
	return d
}

func (p *Policy) builtinCommand(s Simple) Decision {
	if s.Dyn[0] {
		return ask("the program name comes from a variable or substitution")
	}
	prog := s.Prog()
	args := s.Words[1:]
	flags, pos := splitArgs(args)
	switch {
	case prog == "find" || prog == "fd" || prog == "fdfind":
		for _, a := range args {
			switch a {
			case "-exec", "-execdir", "-ok", "-okdir", "-delete", "-fprint", "-fprint0", "-fprintf", "-fls", "-x", "-X", "--exec", "--exec-batch":
				return ask(prog + " " + a + " runs commands or deletes files")
			}
		}
		return allowRO("read-only search")
	case prog == "sort":
		if hasFlag(flags, "o", "--output") {
			return ask("sort -o writes a file")
		}
		return allowRO("read-only")
	case prog == "sed":
		return p.sed(s, flags, pos)
	case prog == "env" || prog == "printenv":
		return allowRO("read-only")
	case readOnlyProgs[prog]:
		return allowRO("read-only command")
	case prog == "git":
		return p.git(s)
	case prog == "go":
		return goCmd(pos)
	case buildProgs[prog]:
		return allow("build/test/lint tool")
	case prog == "npm" || prog == "pnpm" || prog == "yarn" || prog == "bun":
		return nodeCmd(prog, pos)
	case prog == "cargo":
		return subAllowed(prog, pos, "test", "build", "check", "clippy", "fmt", "doc", "bench", "run", "tree", "metadata", "version", "t", "b", "c")
	case prog == "python" || prog == "python3" || prog == "python2":
		return pythonCmd(args)
	case prog == "pip" || prog == "pip3":
		return subAllowed(prog, pos, "list", "show", "freeze", "check", "--version")
	case prog == "mvn" || prog == "mvnw" || prog == "gradle" || prog == "gradlew":
		return subAllowed(prog, pos, "test", "compile", "verify", "package", "build", "check", "clean", "assemble", "testClasses", "classes")
	case prog == "dotnet":
		return subAllowed(prog, pos, "test", "build", "format", "--version", "--info")
	case prog == "bazel" || prog == "bazelisk":
		return subAllowed(prog, pos, "test", "build", "query", "cquery", "info", "version")
	case prog == "docker" || prog == "podman" || prog == "nerdctl":
		return dockerCmd(prog, pos)
	case prog == "kubectl" || prog == "helm":
		return subAllowed(prog, pos, "get", "describe", "logs", "top", "version", "config", "api-resources", "explain", "list", "status", "template", "lint")
	case downloaders[prog]:
		return ask(prog + " uses the network")
	case networkProgs[prog]:
		return ask(prog + " uses the network")
	case packageProgs[prog]:
		return ask(prog + " installs or changes software")
	case prog == "npx" || prog == "pnpx" || prog == "bunx" || prog == "uvx" || prog == "pipx":
		return ask(prog + " downloads and runs a package")
	case prog == "sudo" || prog == "doas":
		return ask("runs with elevated privileges (sudo)")
	case systemCmds[prog]:
		return ask(prog + " changes the system")
	case prog == "dd":
		return ask("dd writes raw data")
	case strings.HasPrefix(prog, "mkfs"):
		return ask(prog + " creates a filesystem")
	case prog == "open" || prog == "xdg-open":
		return ask(prog + " opens an application")
	case prog == "tar" || prog == "zip" || prog == "unzip" || prog == "gzip" || prog == "gunzip" || prog == "bzip2" || prog == "xz" || prog == "7z":
		return ask(prog + " writes archive contents")
	case pathMutators[prog]:
		return p.mutator(s, prog, flags, pos)
	}
	return ask(prog + " is not a known safe command")
}

// subAllowed allows a tool when its subcommand is in the list.
func subAllowed(prog string, pos []string, subs ...string) Decision {
	if len(pos) == 0 {
		return ask(prog + " without a subcommand")
	}
	for _, s := range subs {
		if pos[0] == s {
			return allow(prog + " " + s)
		}
	}
	return ask(prog + " " + pos[0] + " may change the system or use the network")
}

func goCmd(pos []string) Decision {
	if len(pos) == 0 {
		return ask("go without a subcommand")
	}
	switch pos[0] {
	case "test", "build", "vet", "fmt", "list", "env", "version", "doc", "run", "help", "tool":
		if pos[0] == "tool" {
			return ask("go tool runs a tool")
		}
		return allow("go " + pos[0])
	case "mod":
		if len(pos) > 1 && (pos[1] == "verify" || pos[1] == "why" || pos[1] == "graph" || pos[1] == "edit") {
			return allow("go mod " + pos[1])
		}
		return ask("go mod " + strings.Join(pos[1:min(len(pos), 2)], " ") + " may use the network")
	}
	return ask("go " + pos[0] + " may use the network or generate code")
}

func nodeCmd(prog string, pos []string) Decision {
	if len(pos) == 0 {
		return ask(prog + " without a subcommand installs dependencies")
	}
	switch pos[0] {
	case "test", "t", "tst", "build", "lint", "ls", "list", "run", "run-script", "--version", "-v", "typecheck", "check", "why", "exec":
		if pos[0] == "exec" {
			return ask(prog + " exec runs a program")
		}
		if pos[0] == "run" || pos[0] == "run-script" {
			if len(pos) < 2 {
				return ask(prog + " run: no script")
			}
			for _, pre := range []string{"test", "build", "lint", "check", "typecheck", "type-check", "fmt", "format", "tsc", "vet", "compile"} {
				if strings.HasPrefix(pos[1], pre) {
					return allow(prog + " run " + pos[1])
				}
			}
			return ask(prog + " run " + pos[1] + " runs an arbitrary script")
		}
		return allow(prog + " " + pos[0])
	}
	return ask(prog + " " + pos[0] + " may install packages or run a script")
}

var pythonTools = setOf("pytest", "unittest", "mypy", "ruff", "black", "flake8", "pylint", "isort", "pyright", "compileall", "doctest", "tox")

func pythonCmd(args []string) Decision {
	if len(args) >= 2 && args[0] == "-m" && pythonTools[args[1]] {
		return allow("python -m " + args[1])
	}
	if len(args) == 1 && (args[0] == "--version" || args[0] == "-V") {
		return allowRO("python version")
	}
	return ask("runs an arbitrary Python program")
}

func dockerCmd(prog string, pos []string) Decision {
	if len(pos) > 0 && pos[0] == "compose" {
		pos = pos[1:]
		return subAllowed(prog+" compose", pos, "ps", "logs", "config", "ls", "images", "version")
	}
	return subAllowed(prog, pos, "ps", "images", "logs", "inspect", "version", "info", "stats", "top", "ls", "history", "port", "diff")
}

func (p *Policy) sed(s Simple, flags, pos []string) Decision {
	inPlace := hasFlag(flags, "i", "--in-place")
	if !inPlace {
		return allowRO("read-only sed")
	}
	files := pos
	if !hasFlag(flags, "ef", "--expression", "--file") && len(files) > 0 {
		files = files[1:] // the script
	}
	if len(files) == 0 {
		return ask("sed -i without a file")
	}
	var c combiner
	for _, f := range files {
		c.add(p.pathDecision(f))
	}
	d := c.result()
	d.ReadOnly = false
	return d
}

// mutator judges commands that create, change or remove files: allowed inside
// the workspace, asked elsewhere.
func (p *Policy) mutator(s Simple, prog string, flags, pos []string) Decision {
	if s.Dynamic {
		return ask(prog + " on paths read from a pipe")
	}
	recursive := hasFlag(flags, "rR", "--recursive")
	targets := pos
	switch prog {
	case "chmod", "chown", "chgrp":
		if recursive {
			return ask(prog + " -R changes many files")
		}
		if len(targets) > 0 {
			targets = targets[1:] // the mode or owner
		}
	case "cp", "install", "ln":
		if !hasFlag(flags, "t", "--target-directory") && len(targets) > 0 {
			targets = targets[len(targets)-1:] // the destination
		}
	case "truncate":
		if hasFlag(flags, "s", "--size") && len(targets) > 0 && isDigits(strings.TrimLeft(targets[0], "+-<>/%")) {
			targets = targets[1:]
		}
	}
	if len(targets) == 0 {
		return allow(prog + " without a target")
	}
	var c combiner
	for _, t := range targets {
		dyn := s.Dyn[indexOf(s.Words, t)]
		info := p.classify(t)
		if dyn || info.class == pathUnknown {
			c.add(ask(prog + " on " + t + ": the path depends on a variable or substitution"))
			continue
		}
		if prog == "rm" || prog == "rmdir" || prog == "mv" || prog == "unlink" {
			if info.root {
				c.add(ask(prog + " of the whole workspace"))
				continue
			}
		}
		d := p.pathDecision(t)
		if d.Action != Allow {
			d.Reason = prog + ": " + d.Reason
		}
		d.ReadOnly = false
		c.add(d)
	}
	return c.result()
}

func indexOf(list []string, s string) int {
	for i, x := range list {
		if x == s {
			return i
		}
	}
	return -1
}

// git judges git commands.
func (p *Policy) git(s Simple) Decision {
	args := s.Words[1:]
	for _, a := range args {
		if a == "--git-dir" || strings.HasPrefix(a, "--git-dir=") || strings.HasPrefix(a, "--work-tree") {
			return ask("git with --git-dir or --work-tree acts on another repository")
		}
	}
	sub, rest := gitSub(args)
	flags, pos := splitArgs(rest)
	switch sub {
	case "":
		return allowRO("git without a subcommand")
	case "status", "diff", "log", "show", "rev-parse", "rev-list", "ls-files", "ls-tree", "blame", "grep", "describe", "shortlog", "cat-file",
		"name-rev", "for-each-ref", "diff-tree", "diff-files", "diff-index", "merge-base", "whatchanged", "count-objects", "version", "help",
		"check-ignore", "show-ref", "show-branch", "var", "annotate", "range-diff", "verify-commit", "fsck", "reflog", "ls-remote-local":
		if sub == "reflog" && len(pos) > 0 && pos[0] != "show" {
			return ask("git reflog " + pos[0] + " changes the reflog")
		}
		if hasFlag(flags, "", "--output") {
			return ask("git " + sub + " --output writes a file")
		}
		return allowRO("read-only git")
	case "config":
		if hasFlag(flags, "", "--get", "--get-all", "--list", "--get-regexp", "--show-origin") || hasFlag(flags, "l") {
			return allowRO("read-only git config")
		}
		return ask("git config changes configuration")
	case "remote":
		if len(pos) == 0 || (len(pos) == 1 && (pos[0] == "show" || pos[0] == "get-url")) {
			return allowRO("read-only git remote")
		}
		return ask("git remote " + pos[0] + " changes the remotes")
	case "branch":
		if hasFlag(flags, "dDmMcCf", "--delete", "--move", "--copy", "--force", "--set-upstream-to", "--unset-upstream", "--edit-description") {
			return ask("git branch changes or deletes branches")
		}
		if len(pos) == 0 || hasFlag(flags, "", "--list") || hasFlag(flags, "lavr") {
			return allowRO("lists branches")
		}
		return allow("creates a branch")
	case "tag":
		if hasFlag(flags, "d", "--delete", "--force") || hasFlag(flags, "f") {
			return ask("git tag deletes or moves a tag")
		}
		if len(pos) == 0 || hasFlag(flags, "l", "--list") {
			return allowRO("lists tags")
		}
		return allow("creates a tag")
	case "stash":
		if len(pos) == 0 {
			return allow("git stash")
		}
		switch pos[0] {
		case "list", "show":
			return allowRO("read-only git stash")
		case "push", "save", "apply", "pop", "create", "store":
			return allow("git stash " + pos[0])
		}
		return ask("git stash " + pos[0] + " discards stashed work")
	case "add", "commit", "merge", "cherry-pick", "revert", "init", "am", "apply", "notes", "bisect", "worktree-list", "submodule-status":
		if sub == "merge" && hasFlag(flags, "", "--no-edit") {
			return allow("git merge")
		}
		return allow("git " + sub + " (local)")
	case "switch":
		if hasFlag(flags, "", "--discard-changes", "--force") || hasFlag(flags, "f") {
			return ask("git switch --discard-changes drops local changes")
		}
		return allow("git switch")
	case "checkout":
		if contains(rest, "--") || contains(rest, ".") || hasFlag(flags, "f", "--force") || hasFlag(flags, "", "--ours", "--theirs") {
			return ask("git checkout can overwrite working tree files")
		}
		return allow("git checkout of a branch")
	case "restore":
		if hasFlag(flags, "S", "--staged") && !hasFlag(flags, "W", "--worktree") {
			return allow("git restore --staged")
		}
		return ask("git restore overwrites working tree files")
	case "reset":
		if hasFlag(flags, "", "--hard", "--merge", "--keep") {
			return ask("git reset --hard discards local changes")
		}
		return allow("git reset (keeps the working tree)")
	case "clean":
		if hasFlag(flags, "n", "--dry-run") {
			return allowRO("git clean dry run")
		}
		return ask("git clean deletes untracked files")
	case "rebase":
		return ask("git rebase rewrites history")
	case "mv", "rm":
		var c combiner
		for _, f := range pos {
			c.add(p.pathDecision(f))
		}
		d := c.result()
		d.ReadOnly = false
		return d
	case "push", "pull", "fetch", "clone", "ls-remote", "submodule", "lfs", "request-pull", "send-email", "archive-remote":
		return ask("git " + sub + " uses the network")
	case "worktree":
		return ask("git worktree creates or removes working trees")
	case "gc", "prune", "filter-branch", "filter-repo", "update-ref", "replace":
		return ask("git " + sub + " rewrites repository data")
	}
	return ask("git " + sub + " is not a known safe command")
}

// sed applies to isDigits via shell.go. pipelineToShell flags a download piped into an interpreter.
func (p *Policy) pipelineToShell(parsed Parsed, pipeAllowed map[int]bool) (Decision, bool) {
	byPipe := map[int][]Simple{}
	for _, s := range parsed.Commands {
		if _, multi := parsed.Pipelines[s.Pipeline]; multi && !pipeAllowed[s.Pipeline] {
			byPipe[s.Pipeline] = append(byPipe[s.Pipeline], s)
		}
	}
	for id, cmds := range byPipe {
		seenDownload := false
		for _, s := range cmds {
			prog := s.Prog()
			if seenDownload && (shells[prog] || s.Elevated) {
				return ask("pipes a download into " + prog + " (" + parsed.Pipelines[id] + ")"), true
			}
			if downloaders[prog] {
				seenDownload = true
			}
		}
	}
	return Decision{}, false
}
