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
# API keys never go here: run "harness auth login <provider>" (stored in auth.json),
# or name an environment variable with api_key_env.
# Precedence: flag > environment > <repo>/.har/config.yml > this file > default.

# provider: opencode            # a name defined below, or the built-ins openai / anthropic
# default_model: gpt-5

# providers:
#   opencode:
#     type: openai              # openai | anthropic
#     base_url: https://opencode.ai/zen/go/v1
#     session_header: x-opencode-session
#     models: [deepseek-v4.1-flash]
#     default_model: deepseek-v4.1-flash
#     api_key_env: OPENCODE_KEY # optional; otherwise the key of auth.json named "opencode"
#   anthropic:
#     type: anthropic

# sandbox:
#   backend: local

# mcp:
#   allow_stdio: false
`
