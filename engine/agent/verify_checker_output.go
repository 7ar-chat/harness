package agent

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/7ar-chat/harness/ai"
)

// checkerOutput is the checker contract's stdout (also when the model runs a checker itself).
type checkerOutput struct {
	OK     *bool  `json:"ok"`
	File   string `json:"file"`
	Checks []struct {
		Name   string `json:"name"`
		OK     bool   `json:"ok"`
		Detail string `json:"detail"`
	} `json:"checks"`
	NextStep string `json:"next_step"`
}

func summarize(checker string, out checkerOutput) (bool, string) {
	ok := out.OK != nil && *out.OK
	var failing []string
	for _, c := range out.Checks {
		if !c.OK {
			d := strings.Join(strings.Fields(c.Detail), " ")
			if d != "" {
				failing = append(failing, c.Name+": "+d)
			} else {
				failing = append(failing, c.Name)
			}
		}
	}
	var b strings.Builder
	if ok {
		fmt.Fprintf(&b, "%s: ok (%d checks passed)", checker, len(out.Checks))
	} else {
		fmt.Fprintf(&b, "%s: NOT OK", checker)
		if len(failing) > 0 {
			b.WriteString(" — " + strings.Join(failing, "; "))
		}
	}
	return ok, truncate(b.String(), 700)
}

// ParseCheckerOutput reads a checker run: the last checker-contract JSON
// object on stdout ({ok, file, checks, next_step?}) and the exit code (0 ok,
// 2 not ok, 1 or other: the checker could not run). found is false when
// stdout holds no contract object.
func ParseCheckerOutput(checker, stdout string, exitCode int) (r CheckResult, found bool) {
	r, _, found = parseChecker(checker, stdout, exitCode)
	return r, found
}

// parseChecker is ParseCheckerOutput with the reported file and the number of checks.
func parseChecker(checker, stdout string, exitCode int) (r CheckResult, out checkerOutput, found bool) {
	text := strings.TrimSpace(stdout)
	for i := strings.LastIndex(text, "{"); i >= 0; {
		out = checkerOutput{}
		if json.Unmarshal([]byte(text[i:]), &out) == nil && out.OK != nil && out.Checks != nil {
			ok, summary := summarize(checker, out)
			r = CheckResult{Path: path.Clean(out.File), Checker: checker, OK: ok && exitCode == 0, Summary: summary, NextStep: out.NextStep}
			if exitCode != 0 && exitCode != 2 {
				r.OK, r.Failed, r.Summary = false, true, fmt.Sprintf("%s could not run (exit %d)", checker, exitCode)
			}
			return r, out, true
		}
		if i == 0 {
			break
		}
		i = strings.LastIndex(text[:i], "{")
	}
	return CheckResult{}, checkerOutput{}, false
}

// modelRanChecker finds a checker result printed by a command the model ran.
// It counts only when strict: the command ran a script (.py or .sh), its
// stdout ends with the checker contract JSON with a non-empty checks list, and
// the reported file resolves to a deliverable of the turn.
func (v *verifyState) modelRanChecker(tc ai.ToolCall, content string) (CheckResult, bool) {
	if tc.Function.Name != "execute_command" {
		return CheckResult{}, false
	}
	var res struct {
		Stdout   string `json:"stdout"`
		ExitCode *int   `json:"exitCode"`
	}
	if json.Unmarshal([]byte(content), &res) != nil || res.ExitCode == nil {
		return CheckResult{}, false
	}
	name := ""
	var args struct {
		Command string `json:"command"`
	}
	if json.Unmarshal([]byte(tc.Function.Arguments), &args) == nil {
		for _, f := range strings.Fields(args.Command) {
			f = strings.Trim(f, `"';&|()`)
			if ext := path.Ext(f); ext == ".py" || ext == ".sh" {
				name = strings.TrimSuffix(path.Base(f), ext)
			}
		}
	}
	if name == "" {
		return CheckResult{}, false // no checker script ran (e.g. cat of an old report)
	}
	r, out, ok := parseChecker(name, res.Stdout, *res.ExitCode)
	if !ok || len(out.Checks) == 0 {
		return CheckResult{}, false
	}
	if r.Path = v.tracked(out.File); r.Path == "" {
		return CheckResult{}, false
	}
	return r, true
}

// checkedLast reports whether a command's last step is exactly the checker
// command of p (CheckCommander; its words in any order, quotes removed, no
// pipe, redirection or background job) and nothing before it can change what
// that step runs (unsafeShell): the check saw p's final content.
func (st *runState) checkedLast(tc ai.ToolCall, p string) bool {
	cc, ok := st.a.Harness.Checker.(CheckCommander)
	if !ok || tc.Function.Name != "execute_command" {
		return false
	}
	want := cc.CheckCommand(p)
	var args struct {
		Command    string `json:"command"`
		Background bool   `json:"background"`
	}
	if want == "" || json.Unmarshal([]byte(tc.Function.Arguments), &args) != nil || args.Background || unsafeShell(args.Command) {
		return false
	}
	for i, w := range shellWords(want) {
		// The interpreter and the checker script may not be written or
		// replaced by an earlier step (cp fake.py …/inspect_docx.py, a
		// …/python3 shim): the script is named once, the interpreter never as a path.
		if ext := path.Ext(w); (ext == ".py" || ext == ".sh") && strings.Count(args.Command, w) != 1 || i == 0 && strings.Contains(args.Command, "/"+w) {
			return false
		}
	}
	steps := strings.FieldsFunc(strings.NewReplacer("&&", "\n", "||", "\n", ";", "\n").Replace(args.Command), func(r rune) bool { return r == '\n' })
	if len(steps) == 0 {
		return false
	}
	last := strings.TrimSpace(steps[len(steps)-1])
	if strings.ContainsAny(last, "|<>&`$") {
		return false
	}
	return sameWords(shellWords(last), shellWords(want))
}

// shellRedefines matches what can make a later step of a command run
// something other than its words say (final review of round 3, F5): a shell
// function or alias, a changed PATH (or another variable that decides what
// runs: PYTHON*, BASH_ENV, ENV, LD_*), a sourced file, eval, exec, a builtin
// switched off, a hashed command path, or a Python start-up hook.
var shellRedefines = regexp.MustCompile(`(?:^|[^\w./-])(?:function|alias|eval|exec|source|enable|hash|builtin|command|declare|typeset|trap|shopt)(?:$|[^\w./-])` +
	`|(?:^|[\s;&|(){}])\.\s` +
	`|\w\s*\(\s*\)\s*[{(]` +
	`|(?:^|[^\w])(?:PATH|PYTHON\w*|BASH_ENV|ENV|LD_\w+|SHELLOPTS|BASHOPTS|IFS)\+?=` +
	`|sitecustomize|usercustomize|\.pth\b`)

// unsafeShell reports a command whose checker step cannot be trusted (when
// in doubt, the runtime runs its own check).
func unsafeShell(command string) bool { return shellRedefines.MatchString(command) }

// shellWords splits a simple command into words, removing quotes.
func shellWords(s string) []string {
	var out []string
	var cur strings.Builder
	quote, word := rune(0), false
	for _, r := range s {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			quote, word = r, true
		case r == ' ' || r == '\t':
			if word {
				out = append(out, cur.String())
				cur.Reset()
				word = false
			}
		default:
			cur.WriteRune(r)
			word = true
		}
	}
	if word {
		out = append(out, cur.String())
	}
	return out
}

func sameWords(a, b []string) bool {
	if len(a) != len(b) || len(a) == 0 {
		return false
	}
	a, b = append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return a[0] != "" && len(a) >= 2
}
