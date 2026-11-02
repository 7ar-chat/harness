package policy

import (
	"encoding/json"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Kind is what a tool call does, coarsely: the unit policies decide on.
type Kind int

// Kinds, from harmless to the most severe order of Severity.
const (
	KindRead Kind = iota
	KindWriteWorkspace
	KindExec
	KindNet
	KindGitPush
	KindGitForcePush
	KindMerge
	KindSecrets
	KindDestructive
	KindOutsideWorkspace
	KindPublish
)

var kindNames = [...]string{
	KindRead:             "read",
	KindWriteWorkspace:   "write_workspace",
	KindExec:             "exec",
	KindNet:              "net",
	KindGitPush:          "git_push",
	KindGitForcePush:     "git_force_push",
	KindMerge:            "merge",
	KindSecrets:          "secrets",
	KindDestructive:      "destructive",
	KindOutsideWorkspace: "outside_workspace",
	KindPublish:          "publish",
}

func (k Kind) String() string {
	if k < 0 || int(k) >= len(kindNames) {
		return "unknown"
	}
	return kindNames[k]
}

// Severity orders kinds for combining the simple commands of one script: the
// most severe wins (Secrets, ForcePush, Merge, Publish, Destructive,
// OutsideWorkspace, GitPush, Net, Exec, WriteWorkspace, Read).
func (k Kind) Severity() int {
	switch k {
	case KindSecrets:
		return 10
	case KindGitForcePush:
		return 9
	case KindMerge:
		return 8
	case KindPublish:
		return 7
	case KindDestructive:
		return 6
	case KindOutsideWorkspace:
		return 5
	case KindGitPush:
		return 4
	case KindNet:
		return 3
	case KindExec:
		return 2
	case KindWriteWorkspace:
		return 1
	}
	return 0
}

// unknownBranch in Action.Branches stands for a pushed branch the policy
// cannot read (a variable, arguments from stdin).
const unknownBranch = "*"

// Action is what one tool call does.
type Action struct {
	Kind Kind
	// Paths are the files the call writes, as the agent wrote them (for a
	// read tool, the path it reads). Protected-path rules apply to them.
	Paths []string
	// Command is the shell command of an execute_command call.
	Command string
	// Detail says why the call has its kind.
	Detail string
	// Branches are the remote branches a git push updates; unknownBranch
	// when one cannot be read. Empty: the current branch.
	Branches []string
}

func merge(a, b Action) Action {
	if b.Kind.Severity() > a.Kind.Severity() || (a.Detail == "" && b.Kind.Severity() == a.Kind.Severity()) {
		a.Kind = b.Kind
		if b.Detail != "" {
			a.Detail = b.Detail
		}
	}
	for _, p := range b.Paths {
		if !slices.Contains(a.Paths, p) {
			a.Paths = append(a.Paths, p)
		}
	}
	for _, p := range b.Branches {
		if !slices.Contains(a.Branches, p) {
			a.Branches = append(a.Branches, p)
		}
	}
	return a
}

const maxDepth = 8

var (
	gitPushRe  = regexp.MustCompile(`(?s)\bgit\b.{0,200}?\bpush\b`)
	forceRe    = regexp.MustCompile(`(?:^|\s)(?:-[a-zA-Z]*f[a-zA-Z]*|--force\S*|--delete|--mirror|--all|\+\S+)(?:\s|$)`)
	assignRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\+?=`)
	secretVarR = regexp.MustCompile(`(?i)\$\{?(?:[A-Za-z0-9]*_)?(?:TOKEN|KEY|SECRET|PASSWORD|PASSWD|CREDENTIALS?)[A-Za-z0-9_]*`)
	mergeAPIRe = regexp.MustCompile(`/pulls/[^/\s]+/merge\b|/merges\b`)
)

var readTools = map[string]bool{
	"read_file": true, "view_file": true, "search_files": true, "list_files": true,
}

var passiveTools = map[string]bool{
	"job_output": true, "pr_status": true, "ci_log": true, "pr_comments": true,
	"delegate": true, "delegate_many": true, "ask_user": true, "update_plan": true,
	"explore": true, "load_skill": true,
}

// Classify says what a tool call does. Tools it does not know (MCP tools)
// are treated as network calls.
func Classify(toolName, argsJSON string) Action {
	args := map[string]any{}
	valid := true
	if s := strings.TrimSpace(argsJSON); s != "" {
		if err := json.Unmarshal([]byte(s), &args); err != nil {
			valid = false
		}
	}
	str := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := args[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	switch {
	case toolName == "execute_command":
		cmd := str("command", "cmd")
		if cmd == "" && !valid {
			return Action{Kind: KindExec, Command: argsJSON, Detail: "arguments are not valid JSON"}
		}
		a := classifyScript(cmd, 0)
		a.Command = cmd
		return a
	case toolName == "write_file" || toolName == "edit_file":
		p := str("path", "file_path")
		a := writeTarget(word{text: p})
		if p == "" {
			a = Action{Kind: KindOutsideWorkspace, Detail: "no path"}
		}
		if a.Kind == KindWriteWorkspace && len(a.Paths) == 0 { // scratch space
			a.Paths = []string{p}
		}
		return a
	case readTools[toolName]:
		p := str("path", "file_path", "dir", "directory")
		a := Action{Kind: KindRead}
		if p != "" {
			a.Paths = []string{p}
			if secretPath(p) {
				a.Kind, a.Detail = KindSecrets, "reads credentials: "+p
			}
		}
		return a
	case passiveTools[toolName]:
		return Action{Kind: KindRead}
	case toolName == "job_kill":
		return Action{Kind: KindExec, Detail: "stops a background job"}
	case toolName == "open_pr":
		return Action{Kind: KindNet, Detail: "opens a pull request"}
	}
	return Action{Kind: KindNet, Detail: "tool " + toolName}
}

// classifyScript classifies every simple command of a shell script and
// returns the most severe action.
func classifyScript(script string, depth int) Action {
	if depth > maxDepth {
		a := Action{Kind: KindExec, Detail: "script nested too deeply to analyse"}
		return merge(a, pushFallback(script))
	}
	p := parseShell(script)
	res := Action{Kind: KindRead}
	fetch := false
	for _, c := range p.cmds {
		if !c.pipeIn {
			fetch = false
		}
		res = merge(res, classifySimple(c, depth, fetch))
		if name, _ := cmdName(c.words); name == "curl" || name == "wget" || name == "fetch" {
			fetch = true
		}
	}
	for _, s := range p.subs {
		res = merge(res, classifyScript(s, depth+1))
	}
	if p.uncertain {
		res = merge(res, pushFallback(script))
	}
	return res
}

// pushFallback is the conservative reading of text the parser could not
// follow: "git" followed by "push" is a push.
func pushFallback(text string) Action {
	if !gitPushRe.MatchString(text) {
		return Action{Kind: KindRead}
	}
	a := Action{Kind: KindGitPush, Branches: []string{unknownBranch}, Detail: "git push in a script the policy cannot analyse"}
	if forceRe.MatchString(text) {
		a.Kind, a.Detail = KindGitForcePush, "force push in a script the policy cannot analyse"
	}
	return a
}

func classifySimple(c simple, depth int, fetch bool) Action {
	res := Action{Kind: KindRead}
	for _, w := range c.writes {
		res = merge(res, writeTarget(w))
	}
	for _, w := range c.reads {
		if secretPath(w.text) {
			res = merge(res, Action{Kind: KindSecrets, Detail: "reads credentials: " + w.text})
		}
	}
	return merge(res, cmdAction(c.words, depth, fetch))
}

var keywords = map[string]bool{
	"if": true, "then": true, "else": true, "elif": true, "do": true, "while": true,
	"until": true, "{": true, "}": true, "!": true, "fi": true, "done": true, "esac": true,
}

// stripPrefix drops leading VAR=value assignments and shell keywords.
func stripPrefix(words []word) []word {
	for len(words) > 0 {
		t := words[0].text
		if (!words[0].dyn || assignRe.MatchString(t)) && keywords[t] || assignRe.MatchString(t) {
			words = words[1:]
			continue
		}
		break
	}
	return words
}

// cmdName is the command a simple command runs (its base name) and its arguments.
func cmdName(words []word) (string, []word) {
	words = stripPrefix(words)
	if len(words) == 0 || words[0].dyn {
		return "", nil
	}
	name := words[0].text
	if strings.Contains(name, "/") {
		name = path.Base(name)
	}
	return name, words[1:]
}

var noSecretScan = map[string]bool{
	"echo": true, "printf": true, "print": true, "ls": true, "stat": true, "test": true, "[": true,
	"[[": true, "type": true, "which": true, "command": true, "cd": true, "pwd": true,
}

func cmdAction(words []word, depth int, fetch bool) Action {
	words = stripPrefix(words)
	if len(words) == 0 {
		return Action{Kind: KindRead}
	}
	if words[0].dyn {
		return dynamicCommand(words, depth)
	}
	name, args := cmdName(words)
	a := namedCommand(name, args, depth, fetch)
	if !noSecretScan[name] {
		for _, w := range args {
			if (!strings.ContainsAny(w.text, " \t\n") && secretPath(w.text)) || (w.dyn && secretText(w.text)) {
				a = merge(a, Action{Kind: KindSecrets, Detail: "reads credentials: " + w.text})
				break
			}
		}
	}
	return a
}

// dynamicCommand classifies a command whose name comes from an expansion.
func dynamicCommand(words []word, depth int) Action {
	args := words[1:]
	for i, w := range args {
		if !w.dyn && w.text == "push" {
			a := gitPush(args[i+1:])
			a.Detail = "git push built from an expansion"
			return a
		}
	}
	if a, ok := shellDashC(args, depth); ok {
		return a
	}
	return Action{Kind: KindExec, Detail: "command built from an expansion"}
}

// writeTarget classifies writing one path.
func writeTarget(w word) Action {
	abs, known := resolve(w.text)
	switch {
	case !known:
		return Action{Kind: KindWriteWorkspace, Paths: []string{w.text}, Detail: "path built from an expansion"}
	case inWorkspace(abs):
		return Action{Kind: KindWriteWorkspace, Paths: []string{w.text}}
	case scratch(abs):
		return Action{Kind: KindWriteWorkspace}
	}
	return Action{Kind: KindOutsideWorkspace, Paths: []string{w.text}, Detail: "writes " + w.text + ", outside /workspace"}
}

// argv is a command's arguments split into flags, flag values and operands.
type argv struct {
	flags []string
	vals  map[string][]word
	pos   []word
}

func (a argv) has(flags ...string) bool {
	for _, f := range flags {
		if slices.Contains(a.flags, f) {
			return true
		}
	}
	return false
}

// short reports whether a short-flag cluster (-rf) contains any of letters.
func (a argv) short(letters string) bool {
	for _, f := range a.flags {
		if len(f) > 1 && f[0] == '-' && f[1] != '-' && strings.ContainsAny(f[1:], letters) {
			return true
		}
	}
	return false
}

func parseArgs(args []word, valued map[string]bool) argv {
	r := argv{vals: map[string][]word{}}
	for i := 0; i < len(args); i++ {
		t := args[i].text
		if t == "--" {
			r.pos = append(r.pos, args[i+1:]...)
			break
		}
		if len(t) < 2 || t[0] != '-' {
			r.pos = append(r.pos, args[i])
			continue
		}
		name := t
		if strings.HasPrefix(t, "--") {
			if k := strings.IndexByte(t, '='); k >= 0 {
				name = t[:k]
				r.flags = append(r.flags, name)
				r.vals[name] = append(r.vals[name], word{text: t[k+1:], dyn: args[i].dyn})
				continue
			}
		}
		r.flags = append(r.flags, name)
		if valued[name] && i+1 < len(args) {
			i++
			r.vals[name] = append(r.vals[name], args[i])
		}
	}
	return r
}

var readOnlyCommands = map[string]bool{
	"cat": true, "less": true, "more": true, "head": true, "tail": true, "grep": true, "egrep": true,
	"fgrep": true, "rg": true, "ag": true, "ls": true, "ll": true, "tree": true, "wc": true, "echo": true,
	"printf": true, "print": true, "pwd": true, "cd": true, "pushd": true, "popd": true, "true": true,
	"false": true, ":": true, "test": true, "[": true, "[[": true, "stat": true, "file": true, "du": true,
	"df": true, "which": true, "type": true, "whoami": true, "id": true, "uname": true, "date": true,
	"hostname": true, "basename": true, "dirname": true, "realpath": true, "readlink": true, "sort": true,
	"uniq": true, "cut": true, "tr": true, "diff": true, "cmp": true, "nl": true, "tac": true, "jq": true,
	"yq": true, "xxd": true, "base64": true, "od": true, "strings": true, "hexdump": true, "sha256sum": true,
	"md5sum": true, "shasum": true, "sleep": true, "wait": true, "read": true, "seq": true, "expr": true,
	"column": true, "fold": true, "paste": true, "comm": true, "join": true, "nproc": true, "ps": true,
	"top": true, "free": true, "lsof": true, "unset": true, "alias": true, "unalias": true, "local": true,
	"return": true, "exit": true, "break": true, "continue": true, "shift": true, "umask": true, "ulimit": true,
	"getconf": true, "locale": true, "cksum": true, "whereis": true, "man": true, "help": true, "bat": true,
	"fd": true, "fdfind": true, "tput": true, "clear": true, "pgrep": true, "sed": true, "set": true,
}

var interpreters = map[string]bool{
	"python": true, "python3": true, "python2": true, "node": true, "nodejs": true, "ruby": true,
	"perl": true, "php": true, "lua": true, "deno": true, "bun": true, "osascript": true, "pwsh": true,
}

var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "ash": true, "fish": true}

func namedCommand(name string, args []word, depth int, fetch bool) Action {
	switch name {
	case "sudo", "doas", "su", "pkexec", "runuser":
		return Action{Kind: KindDestructive, Detail: name + " runs commands with elevated privileges"}
	case "ssh", "scp", "sftp", "nc", "ncat", "netcat", "socat", "telnet":
		return Action{Kind: KindDestructive, Detail: name + " opens a connection to a remote machine"}
	case "shutdown", "reboot", "halt", "poweroff", "fdisk", "parted", "shred", "wipefs", "insmod", "rmmod", "mount", "umount":
		return Action{Kind: KindDestructive, Detail: name + " changes the machine"}
	case "dd":
		return ddAction(args)
	case "git":
		return gitAction(args)
	case "gh", "glab", "hub":
		return ghAction(name, args)
	case "curl", "wget", "http", "https", "xh", "aria2c", "fetch":
		return netAction(name, args)
	case "rm", "unlink":
		return rmAction(args)
	case "find":
		return findAction(args, depth)
	case "chmod", "chown", "chgrp":
		return chmodAction(name, args)
	case "touch", "mkdir", "rmdir", "truncate", "tee", "mkfifo":
		return writeCommand(name, args)
	case "cp", "mv", "install", "ln":
		return copyAction(name, args)
	case "perl":
		return perlAction(args, fetch)
	case "env":
		return envAction(args, depth)
	case "command", "builtin", "exec", "nohup", "time", "nice", "ionice", "setsid", "stdbuf", "timeout", "chronic", "unbuffer", "caffeinate":
		if name == "command" {
			if f := parseArgs(args, nil); f.has("-v", "-V") {
				return Action{Kind: KindRead}
			}
		}
		return cmdAction(unwrap(name, args), depth+1, fetch)
	case "xargs":
		return xargsAction(args, depth)
	case "eval":
		return evalAction(args, depth)
	case "source", ".":
		return sourceAction(args, depth)
	case "security", "secret-tool", "keyctl", "op", "pass", "gopass", "bw", "vault", "pbpaste", "xclip", "wl-paste", "kwallet-query":
		// Keychains, password managers and the clipboard: where a developer's real tokens live. The GitHub
		// CLI keeps its login in the macOS keychain, so `security find-generic-password` would hand an agent
		// the token of the person who started the factory.
		return Action{Kind: KindSecrets, Detail: name + " reads the credential store of this machine"}
	case "printenv":
		return Action{Kind: KindSecrets, Detail: "printenv prints the environment, which holds secrets"}
	case "export", "declare", "typeset", "compgen":
		f := parseArgs(args, nil)
		if len(args) == 0 || f.has("-p", "-x", "-e", "-v") {
			return Action{Kind: KindSecrets, Detail: name + " lists the environment, which holds secrets"}
		}
		return Action{Kind: KindRead}
	case "echo", "printf", "print":
		for _, w := range args {
			if secretVarR.MatchString(w.text) {
				return Action{Kind: KindSecrets, Detail: "prints a secret variable: " + w.text}
			}
		}
		return Action{Kind: KindRead}
	case "npm", "yarn", "pnpm", "bun", "cargo", "twine", "gem", "docker", "podman", "buildah", "helm", "poetry", "uv", "dotnet", "mvn":
		if a, ok := publishAction(name, args); ok {
			return a
		}
		if name == "bun" {
			break
		}
		return Action{Kind: KindExec}
	}
	if shells[name] {
		return shellAction(name, args, depth, fetch)
	}
	if interpreters[name] {
		return interpreterAction(name, args, fetch)
	}
	if strings.HasPrefix(name, "mkfs") {
		return Action{Kind: KindDestructive, Detail: name + " formats a disk"}
	}
	if name == "sed" || name == "set" {
		return sedOrSet(name, args)
	}
	if readOnlyCommands[name] {
		return Action{Kind: KindRead}
	}
	return Action{Kind: KindExec}
}

// unwrap drops a wrapper command's own options and operands, leaving the
// command it runs.
func unwrap(name string, args []word) []word {
	valued := map[string]bool{}
	skip := 0
	switch name {
	case "timeout":
		valued = map[string]bool{"-s": true, "--signal": true, "-k": true, "--kill-after": true}
		skip = 1
	case "nice":
		valued = map[string]bool{"-n": true, "--adjustment": true}
	case "ionice":
		valued = map[string]bool{"-c": true, "-n": true, "-p": true, "-P": true, "-u": true}
	case "stdbuf":
		valued = map[string]bool{"-i": true, "-o": true, "-e": true}
	case "exec":
		valued = map[string]bool{"-a": true}
	}
	i := 0
	for i < len(args) {
		t := args[i].text
		if t == "--" {
			i++
			break
		}
		if len(t) < 2 || t[0] != '-' {
			break
		}
		i++
		if valued[t] && i < len(args) {
			i++
		}
	}
	i += skip
	if i > len(args) {
		i = len(args)
	}
	return args[i:]
}

func sedOrSet(name string, args []word) Action {
	if name == "set" {
		if len(args) == 0 {
			return Action{Kind: KindSecrets, Detail: "set lists the shell variables, which hold secrets"}
		}
		return Action{Kind: KindRead}
	}
	return sedAction(args)
}

func envAction(args []word, depth int) Action {
	i := 0
	for i < len(args) {
		t := args[i].text
		switch {
		case assignRe.MatchString(t):
			i++
		case t == "-u" || t == "-C" || t == "-S" || t == "--unset" || t == "--chdir" || t == "--split-string":
			i += 2
		case len(t) > 1 && t[0] == '-':
			i++
		default:
			return cmdAction(args[i:], depth+1, false)
		}
	}
	return Action{Kind: KindSecrets, Detail: "env prints the environment, which holds secrets"}
}

func xargsAction(args []word, depth int) Action {
	valued := map[string]bool{"-n": true, "-P": true, "-d": true, "-E": true, "-L": true, "-s": true, "-a": true,
		"-I": true, "--max-args": true, "--max-procs": true, "--delimiter": true, "--arg-file": true, "--max-lines": true}
	i := 0
	for i < len(args) {
		t := args[i].text
		if t == "--" {
			i++
			break
		}
		if len(t) < 2 || t[0] != '-' {
			break
		}
		i++
		if valued[t] && i < len(args) {
			i++
		}
	}
	if i >= len(args) {
		return Action{Kind: KindRead}
	}
	a := cmdAction(args[i:], depth+1, false)
	if a.Kind == KindGitPush { // its arguments come from stdin
		a.Branches = append(a.Branches, unknownBranch)
		a.Detail = "git push whose arguments come from stdin"
	}
	// A recursive removal whose targets come from stdin cannot be checked against the workspace: the paths
	// are exactly what the policy would have looked at.
	if name := args[i].text; path.Base(name) == "rm" && removesRecursively(args[i+1:]) {
		a = merge(a, Action{Kind: KindDestructive, Detail: "recursive removal of paths that come from stdin"})
	}
	return merge(a, Action{Kind: KindExec})
}

// removesRecursively reports rm's -r, -R, -rf... or --recursive among args.
func removesRecursively(args []word) bool {
	for _, w := range args {
		t := w.text
		if t == "--recursive" || len(t) > 1 && t[0] == '-' && t[1] != '-' && strings.ContainsAny(t[1:], "rR") {
			return true
		}
	}
	return false
}

func evalAction(args []word, depth int) Action {
	parts := make([]string, len(args))
	dyn := false
	for i, w := range args {
		parts[i] = w.text
		dyn = dyn || w.dyn
	}
	script := strings.Join(parts, " ")
	a := classifyScript(script, depth+1)
	if dyn {
		a = merge(a, pushFallback(script))
		if fetchesCode(script) {
			a = merge(a, Action{Kind: KindDestructive, Detail: "evaluates downloaded code"})
		}
	}
	return merge(a, Action{Kind: KindExec})
}

func sourceAction(args []word, depth int) Action {
	for _, w := range args {
		if fetchesCode(w.text) {
			return Action{Kind: KindDestructive, Detail: "runs downloaded code"}
		}
	}
	return Action{Kind: KindExec}
}

// fetchesCode reports text that downloads (an expansion of curl or wget).
func fetchesCode(s string) bool {
	return strings.Contains(s, "curl ") || strings.Contains(s, "wget ") || strings.Contains(s, "curl\t")
}

func interpreterAction(name string, args []word, fetch bool) Action {
	a := Action{Kind: KindExec}
	inline := false
	for _, w := range args {
		if w.text == "-c" || w.text == "-e" || w.text == "-E" || w.text == "-r" || w.text == "--eval" || w.text == "-p" {
			inline = true
		}
		if secretText(w.text) {
			a = merge(a, Action{Kind: KindSecrets, Detail: "reads credentials in a " + name + " program"})
		}
	}
	if fetch && !inline {
		a = merge(a, Action{Kind: KindDestructive, Detail: "downloaded code piped into " + name})
	}
	return a
}

func shellAction(name string, args []word, depth int, fetch bool) Action {
	if a, ok := shellDashC(args, depth); ok {
		return a
	}
	if fetch {
		return Action{Kind: KindDestructive, Detail: "downloaded code piped into " + name}
	}
	for _, w := range args {
		if (strings.HasPrefix(w.text, "<(") || strings.HasPrefix(w.text, "$(")) && fetchesCode(w.text) {
			return Action{Kind: KindDestructive, Detail: "downloaded code run by " + name}
		}
	}
	return Action{Kind: KindExec}
}

// shellDashC handles "sh -c script": the script is classified as a script.
func shellDashC(args []word, depth int) (Action, bool) {
	for i := 0; i < len(args); i++ {
		t := args[i].text
		if t == "-o" || t == "+o" || t == "-O" || t == "+O" {
			i++
			continue
		}
		if len(t) < 2 || t[0] != '-' || strings.HasPrefix(t, "--") {
			return Action{}, false
		}
		if !strings.Contains(t, "c") {
			continue
		}
		if i+1 >= len(args) {
			return Action{Kind: KindExec}, true
		}
		script := args[i+1]
		a := classifyScript(script.text, depth+1)
		if script.dyn {
			a = merge(a, pushFallback(script.text))
			if fetchesCode(script.text) {
				a = merge(a, Action{Kind: KindDestructive, Detail: "downloaded code run by a shell"})
			}
		}
		return merge(a, Action{Kind: KindExec}), true
	}
	return Action{}, false
}

// commandTexts lists the simple commands of a script as text, for config
// rules: assignments and wrappers removed, git's global options dropped.
func commandTexts(script string, depth int) []string {
	if depth > maxDepth {
		return nil
	}
	p := parseShell(script)
	var out []string
	for _, c := range p.cmds {
		words := stripPrefix(c.words)
		if len(words) == 0 {
			continue
		}
		out = append(out, joinWords(words))
		peeled := peel(words)
		if len(peeled) > 0 {
			t := joinWords(peeled)
			if !slices.Contains(out, t) {
				out = append(out, t)
			}
			name, args := cmdName(peeled)
			switch {
			case shells[name]:
				for i := 0; i < len(args); i++ {
					if t := args[i].text; len(t) > 1 && t[0] == '-' && !strings.HasPrefix(t, "--") && strings.Contains(t, "c") && i+1 < len(args) {
						out = append(out, commandTexts(args[i+1].text, depth+1)...)
						break
					}
				}
			case name == "eval":
				out = append(out, commandTexts(joinWords(args), depth+1)...)
			}
		}
	}
	for _, s := range p.subs {
		out = append(out, commandTexts(s, depth+1)...)
	}
	return out
}

func joinWords(ws []word) string {
	parts := make([]string, len(ws))
	for i, w := range ws {
		parts[i] = w.text
	}
	return strings.Join(parts, " ")
}

// peel removes wrappers and git's global options from a command's words.
func peel(words []word) []word {
	for range maxDepth {
		words = stripPrefix(words)
		name, args := cmdName(words)
		switch name {
		case "command", "builtin", "exec", "nohup", "time", "nice", "ionice", "setsid", "stdbuf", "timeout", "sudo", "doas":
			words = unwrap(name, args)
			continue
		case "env":
			i := 0
			for i < len(args) && (assignRe.MatchString(args[i].text) || (len(args[i].text) > 1 && args[i].text[0] == '-')) {
				i++
			}
			words = args[i:]
			continue
		case "git":
			i := 0
			for i < len(args) && len(args[i].text) > 1 && args[i].text[0] == '-' {
				switch args[i].text {
				case "-C", "-c", "--git-dir", "--work-tree", "--namespace":
					i++
				}
				i++
			}
			if i > len(args) {
				i = len(args)
			}
			return append([]word{words[0]}, args[i:]...)
		}
		return words
	}
	return words
}
