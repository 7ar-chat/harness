// Package codecheck gives the agent immediate syntax feedback on the files it
// writes: after write_file or edit_file, a cheap per-extension command checks
// the file and its complaint is appended to the tool result, so a broken edit
// is fixed in the next step instead of surfacing at the end of a test run.
// It is deliberately not a language server: no project build, no type check.
package codecheck

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/7ar-chat/harness/core/sandbox"
)

// Timeout bounds one check.
const Timeout = 20 * time.Second

// maxOutput bounds the complaint shown to the model.
const maxOutput = 1500

// Defaults are syntax-only checks; {path} is the shell-quoted file. A check
// whose first word is not installed is skipped silently.
var Defaults = map[string]string{
	".go":   "gofmt -e {path} >/dev/null",
	".py":   "python3 -m py_compile {path}",
	".json": "python3 -m json.tool {path} >/dev/null",
	".js":   "node --check {path}",
	".mjs":  "node --check {path}",
	".sh":   "bash -n {path}",
	".rb":   "ruby -c {path} >/dev/null",
	".php":  "php -l {path} >/dev/null",
}

// Checker runs the checks.
type Checker struct {
	// Commands maps a lower-case extension (".go") to a shell command with {path}.
	Commands map[string]string
}

// New returns a Checker with the Defaults overridden by extra (an entry with an empty command disables an extension).
func New(extra map[string]string) *Checker {
	m := map[string]string{}
	for k, v := range Defaults {
		m[k] = v
	}
	for k, v := range extra {
		m[strings.ToLower(k)] = v
	}
	return &Checker{Commands: m}
}

// Parse reads "ext=command;ext=command" (CODE_CHECKS).
func Parse(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, entry := range strings.Split(s, ";") {
		if strings.TrimSpace(entry) == "" {
			continue
		}
		ext, cmd, ok := strings.Cut(entry, "=")
		ext = strings.ToLower(strings.TrimSpace(ext))
		if !ok || !strings.HasPrefix(ext, ".") {
			return nil, fmt.Errorf("CODE_CHECKS: %q is not .ext=command", entry)
		}
		out[ext] = strings.TrimSpace(cmd)
	}
	return out, nil
}

// Check runs the command of p's extension in sb and returns the notice for the
// model: "" when the file is fine, has no check, or the tool is not installed.
func (c *Checker) Check(ctx context.Context, sb sandbox.Sandbox, p string) string {
	cmd := c.Commands[strings.ToLower(path.Ext(p))]
	if cmd == "" {
		return ""
	}
	first, _, _ := strings.Cut(strings.TrimSpace(cmd), " ")
	script := fmt.Sprintf("command -v %s >/dev/null 2>&1 || exit 127\n%s", first, strings.ReplaceAll(cmd, "{path}", shellQuote(p)))
	var out strings.Builder
	collect := func(s string) {
		if out.Len() < 4*maxOutput {
			out.WriteString(s)
		}
	}
	res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: script, Cwd: sandbox.Workspace, Timeout: Timeout, OnStdout: collect, OnStderr: collect})
	if err != nil || res.ExitCode == 0 || res.ExitCode == 127 {
		return ""
	}
	text := strings.TrimSpace(out.String())
	if len(text) > maxOutput {
		text = text[:maxOutput] + "…"
	}
	return fmt.Sprintf("Syntax check failed (%s, exit %d); fix it before going on:\n%s", strings.ReplaceAll(cmd, "{path}", p), res.ExitCode, text)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
