package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/GhaziBenDahmane/harness/config"
)

// subcommand runs `harness auth ...` and `harness config ...` (handled
// before the flags of the main command); ok is false for anything else.
func subcommand(args []string, stdin io.Reader, stdout, stderr io.Writer, isTTY bool) (code int, ok bool) {
	if len(args) == 0 {
		return 0, false
	}
	switch args[0] {
	case "auth":
		return runAuth(args[1:], stdin, stdout, stderr, isTTY), true
	case "config":
		return runConfig(args[1:], stdout, stderr), true
	case "trust", "untrust":
		return runTrust(args[0], args[1:], stdout, stderr), true
	}
	return 0, false
}

// manageDir is the user directory the commands manage: it exists even when
// HARNESS_NO_USER_DIR keeps the harness from reading it.
func manageDir() (string, error) {
	d := config.HomeDir()
	if d == "" {
		return "", errors.New("no home directory: set HAR_HOME")
	}
	return d, nil
}

const authUsage = `usage:
  harness auth login <provider> [-key-stdin]   store the API key of a provider in auth.json
  harness auth list                            list the providers that have a key (masked)
  harness auth remove <provider>               delete the key of a provider`

func runAuth(args []string, stdin io.Reader, stdout, stderr io.Writer, isTTY bool) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, authUsage)
		return 2
	}
	dir, err := manageDir()
	if err != nil {
		return fail(stderr, err)
	}
	switch args[0] {
	case "login":
		fs := flag.NewFlagSet("harness auth login", flag.ContinueOnError)
		fs.SetOutput(stderr)
		keyStdin := fs.Bool("key-stdin", false, "read the key from stdin instead of prompting")
		// Accept the flag before or after the provider name.
		var name string
		var rest []string
		for _, a := range args[1:] {
			if !strings.HasPrefix(a, "-") && name == "" {
				name = a
			} else {
				rest = append(rest, a)
			}
		}
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if name == "" || fs.NArg() > 0 {
			fmt.Fprintln(stderr, "usage: harness auth login <provider> [-key-stdin]")
			return 2
		}
		if !config.ValidProviderName(name) {
			return fail(stderr, fmt.Errorf("invalid provider name %q (letters, digits, '.', '_' and '-')", name))
		}
		key, err := readKey(*keyStdin, name, stdin, stderr, isTTY)
		if err != nil {
			return fail(stderr, err)
		}
		if err := config.SetAuthKey(dir, name, key); err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stdout, "saved the key of %s (%s) in %s\n", name, config.MaskKey(key), config.AuthPath(dir))
		return 0
	case "list":
		keys, err := config.ReadAuth(dir)
		if err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintln(stdout, config.AuthPath(dir))
		for _, n := range config.AuthProviders(keys) {
			fmt.Fprintf(stdout, "%-20s %s\n", n, config.MaskKey(keys[n]))
		}
		if len(keys) == 0 {
			fmt.Fprintln(stdout, "no keys stored (harness auth login <provider>)")
		}
		return 0
	case "remove":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "usage: harness auth remove <provider>")
			return 2
		}
		removed, err := config.RemoveAuthKey(dir, args[1])
		if err != nil {
			return fail(stderr, err)
		}
		if !removed {
			return fail(stderr, fmt.Errorf("no key stored for %q in %s", args[1], config.AuthPath(dir)))
		}
		fmt.Fprintf(stdout, "removed the key of %s from %s\n", args[1], config.AuthPath(dir))
		return 0
	}
	fmt.Fprintln(stderr, authUsage)
	return 2
}

// readKey reads the key from stdin (-key-stdin, or when stdin is not a
// terminal) or prompts for it without echo.
func readKey(fromStdin bool, provider string, stdin io.Reader, stderr io.Writer, isTTY bool) (string, error) {
	var line string
	if fromStdin || !isTTY {
		l, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && l == "" {
			return "", errors.New("no key on stdin")
		}
		line = l
	} else {
		f, ok := stdin.(*os.File)
		if !ok {
			return "", errors.New("cannot prompt: use -key-stdin")
		}
		l, err := readSecret(f, "API key for "+provider+": ", stderr)
		if err != nil {
			return "", err
		}
		line = l
	}
	key := strings.TrimSpace(line)
	if key == "" {
		return "", errors.New("empty API key")
	}
	return key, nil
}

const trustUsage = `usage:
  harness trust [path]      trust a project (default: the current one): it may add permission allow rules and MCP servers
  harness trust list        list the trusted projects
  harness trust mcp [name]  approve the exact command of the project's stdio MCP server(s) (all pending ones without a name)
  harness untrust mcp name  withdraw that approval
  harness untrust [path]    stop trusting a project (default: the current one)`

// projectRootArg is the project a trust command is about: the path given, else the project of the working directory.
func projectRootArg(args []string) (string, error) {
	if len(args) > 1 {
		return "", errors.New("too many arguments")
	}
	if len(args) == 1 {
		st, err := os.Stat(args[0])
		if err != nil {
			return "", err
		}
		if !st.IsDir() {
			return "", fmt.Errorf("%s is not a directory", args[0])
		}
		return config.CanonicalProject(args[0])
	}
	start := os.Getenv("LOCAL_WORKSPACE")
	if start == "" {
		start, _ = os.Getwd()
	}
	_, root, _ := config.FindProjectDir(start)
	if root == "" {
		return "", errors.New("no project here: pass a path")
	}
	return config.CanonicalProject(root)
}

func runTrust(cmd string, args []string, stdout, stderr io.Writer) int {
	dir, err := manageDir()
	if err != nil {
		return fail(stderr, err)
	}
	if cmd == "trust" && len(args) == 1 && args[0] == "list" {
		list, err := config.ReadTrust(dir)
		if err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintln(stdout, config.TrustPath(dir))
		for _, p := range list {
			fmt.Fprintln(stdout, p)
		}
		if len(list) == 0 {
			fmt.Fprintln(stdout, "no trusted projects (harness trust [path])")
		}
		return 0
	}
	if len(args) == 1 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help") {
		fmt.Fprintln(stdout, trustUsage)
		return 0
	}
	if len(args) >= 1 && args[0] == "mcp" {
		return runTrustMCP(cmd, dir, args[1:], stdout, stderr)
	}
	root, err := projectRootArg(args)
	if err != nil {
		fmt.Fprintln(stderr, "harness:", err)
		fmt.Fprintln(stderr, trustUsage)
		return 2
	}
	if cmd == "untrust" {
		removed, err := config.RemoveTrust(dir, root)
		if err != nil {
			return fail(stderr, err)
		}
		if !removed {
			return fail(stderr, fmt.Errorf("%s is not trusted (see: harness trust list)", root))
		}
		fmt.Fprintf(stdout, "no longer trusting %s\n", root)
		return 0
	}
	added, err := config.AddTrust(dir, root)
	if err != nil {
		return fail(stderr, err)
	}
	if !added {
		fmt.Fprintf(stdout, "%s is already trusted\n", root)
		return 0
	}
	fmt.Fprintf(stdout, "trusting %s (listed in %s): its permission allow rules and its MCP servers (.mcp.json, .har/mcp.json, mcp.servers) now apply\n", root, config.TrustPath(dir))
	return 0
}

// runTrustMCP approves (trust mcp) or withdraws (untrust mcp) a stdio MCP server of the current project. The
// approval is bound to the server's command, arguments and environment as the project defines them now.
func runTrustMCP(cmd, dir string, args []string, stdout, stderr io.Writer) int {
	if len(args) > 1 || (cmd == "untrust" && len(args) != 1) {
		fmt.Fprintln(stderr, trustUsage)
		return 2
	}
	// The project's servers are only read when it is trusted: trust it first (`harness trust`).
	cfg, err := config.LoadWith(config.LoadOptions{})
	if err != nil {
		return fail(stderr, err)
	}
	ly := cfg.Layers
	if ly == nil || ly.ProjectRoot == "" {
		return fail(stderr, errors.New("no project here"))
	}
	if cmd == "untrust" {
		removed, err := config.RevokeMCP(dir, ly.ProjectRoot, args[0])
		if err != nil {
			return fail(stderr, err)
		}
		if !removed {
			return fail(stderr, fmt.Errorf("no approved MCP server %q for %s", args[0], ly.ProjectRoot))
		}
		fmt.Fprintf(stdout, "withdrew the approval of MCP server %s\n", args[0])
		return 0
	}
	if !ly.Trusted {
		return fail(stderr, fmt.Errorf("%s is not trusted: run `harness trust` first", ly.ProjectRoot))
	}
	n := 0
	for _, s := range ly.MCPPending {
		if len(args) == 1 && s.Name != args[0] {
			continue
		}
		if _, err := config.ApproveMCP(dir, ly.ProjectRoot, s); err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stdout, "approved MCP server %s (%s)\n", s.Name, s.Describe())
		n++
	}
	if n == 0 {
		return fail(stderr, errors.New("no project stdio MCP server is waiting for approval (it may already be approved, or not allowed: stdio needs the local backend and mcp.allow_stdio)"))
	}
	return 0
}

func runConfig(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage:\n  harness config path   print the user directory and the project directory in use\n  harness config init   write a commented starter config.yml in the user directory")
		return 2
	}
	switch args[0] {
	case "path":
		user := config.UserDir()
		if user == "" {
			if h := config.HomeDir(); h != "" {
				user = h + " (not read: HARNESS_NO_USER_DIR is set)"
			} else {
				user = "(none)"
			}
		}
		start := os.Getenv("LOCAL_WORKSPACE")
		if start == "" {
			start, _ = os.Getwd()
		}
		project, _, found := config.FindProjectDir(start)
		if !found {
			project = "(none)"
		}
		fmt.Fprintf(stdout, "user dir:    %s\nproject dir: %s\n", user, project)
		return 0
	case "init":
		dir, err := manageDir()
		if err != nil {
			return fail(stderr, err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fail(stderr, err)
		}
		file := filepath.Join(dir, config.ConfigFile)
		f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			fmt.Fprintf(stdout, "%s already exists; left as it is\n", file)
			return 0
		}
		if err != nil {
			return fail(stderr, err)
		}
		_, werr := f.WriteString(starterConfig)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return fail(stderr, werr)
		}
		fmt.Fprintf(stdout, "wrote %s\nstore API keys with: harness auth login <provider>\n", file)
		return 0
	}
	fmt.Fprintln(stderr, "unknown command: harness config", args[0])
	return 2
}

const starterConfig = `# ~/.har/config.yml: your settings for the harness.
# You own the endpoints, the keys and the model catalog; a project's .har/config.yml can only choose among
# what is defined here (provider, models, agents) and tune behaviour. See "What a project file may and may not do".
# API keys never go here: run "harness auth login <provider>" (stored in auth.json),
# or name an environment variable with api_key_env.
# Precedence: flag > environment > <repo>/.har/config.yml > this file > default.

# provider: opencode            # a name defined below, or the built-ins openai / anthropic
# default_model: deepseek-v4.1-flash

# providers:
#   opencode:
#     type: openai              # openai | anthropic
#     base_url: https://opencode.ai/zen/go/v1
#     session_header: x-opencode-session
#     models: [deepseek-v4.1-flash, deepseek-v4.1-pro]   # the models a project may select
#     default_model: deepseek-v4.1-flash
#     api_key_env: OPENCODE_KEY # optional; otherwise the key of auth.json named "opencode"
#   anthropic:
#     type: anthropic

# sandbox:
#   backend: local

# mcp:
#   allow_stdio: false          # MCP servers that are local programs; local backend only
#   servers: {}                 # your own servers; a project's need "harness trust"

# Roles and the models they use (a project file may select these too, among the models above):
# agents:
#   default:    {model: deepseek-v4.1-pro, profile: coding}
#   explore:    {model: deepseek-v4.1-flash}
#   compaction: {model: deepseek-v4.1-flash}
`
