package review

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/7ar-chat/harness/core/factory"
)

// Severities of a finding, from the one that always blocks to the one that never does.
const (
	SeverityBlocker = "blocker"
	SeverityMajor   = "major"
	SeverityMinor   = "minor"
	SeverityNit     = "nit"
)

// finding is one problem, from the mechanical checks or the model.
type finding struct {
	Severity string
	File     string
	Line     int
	Issue    string
}

func (f finding) blocking() bool { return f.Severity == SeverityBlocker || f.Severity == SeverityMajor }

// String is the Verdict.Findings form: "file:line [severity] issue".
func (f finding) String() string {
	where := f.File
	if f.File != "" && f.Line > 0 {
		where = fmt.Sprintf("%s:%d", f.File, f.Line)
	}
	if where == "" {
		return fmt.Sprintf("[%s] %s", f.Severity, f.Issue)
	}
	return fmt.Sprintf("%s [%s] %s", where, f.Severity, f.Issue)
}

// DefaultProtected are the paths no automated change may touch: CI that holds
// the deploy credentials, secrets and keys, and the build recipe.
var DefaultProtected = []string{
	".github/workflows/**",
	".github/CODEOWNERS",
	".gitlab-ci.yml",
	"**/.env*",
	"**/*.pem",
	"**/*.key",
	"**/id_rsa*",
	"Dockerfile",
	"Dockerfile.*",
}

// matchesAny reports the first pattern that matches p. A pattern without a
// slash matches a file or directory of that name at any depth (like
// .gitignore); one with a slash is anchored at the repository root unless it
// starts with "**/". "**" spans any number of directories and a trailing "/"
// means the directory's whole content. A malformed pattern matches everything:
// a protection list that cannot be read must block, not silently let through.
func matchesAny(patterns []string, p string) (string, bool) {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	for _, pat := range patterns {
		pat = strings.TrimPrefix(strings.TrimSpace(pat), "./")
		if pat == "" {
			continue
		}
		if strings.HasSuffix(pat, "/") {
			pat += "**"
		}
		ps := strings.Split(pat, "/")
		if len(ps) == 1 {
			if anySegment(ps[0], segs) {
				return pat, true
			}
			continue
		}
		if matchSegments(ps, segs) {
			return pat, true
		}
	}
	return "", false
}

func anySegment(pat string, segs []string) bool {
	for _, s := range segs {
		ok, err := path.Match(pat, s)
		if ok || err != nil {
			return true
		}
	}
	return false
}

func matchSegments(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			if len(pat) == 1 {
				return true
			}
			for i := 0; i <= len(segs); i++ {
				if matchSegments(pat[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		ok, err := path.Match(pat[0], segs[0])
		if err != nil {
			return true
		}
		if !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}

// protectedFindings lists the touched protected paths (both sides of a rename).
func protectedFindings(files []FileChange, patterns []string) []finding {
	var out []finding
	for _, f := range files {
		for _, p := range []string{f.Path, f.OldPath} {
			if p == "" {
				continue
			}
			if pat, ok := matchesAny(patterns, p); ok {
				out = append(out, finding{SeverityBlocker, p, 0, "protected path (matches " + pat + "): no automated change may touch it"})
				break
			}
		}
	}
	return out
}

type secretRule struct {
	name string
	re   *regexp.Regexp
}

var secretRules = []secretRule{
	{"AWS access key id", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"private key block", regexp.MustCompile(`-----BEGIN (?:[A-Z]+ )*PRIVATE KEY(?: BLOCK)?-----`)},
	{"GitHub token", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`)},
	{"API key assignment", regexp.MustCompile(`(?i)api[_-]?key\s*[:=]\s*['"][A-Za-z0-9_\-]{16,}`)},
}

// fixtureMarker excuses a line as an obvious test fixture.
var fixtureMarker = regexp.MustCompile(`(?i)example|dummy|fake`)

// secretFindings scans the lines a diff adds (a secret that is only removed is
// being cleaned up, not leaked). The value is never echoed: the finding goes
// into the PR comment and the next attempt's feedback.
func secretFindings(files []FileChange) []finding {
	var out []finding
	for _, f := range files {
		for _, l := range f.AddedLines {
			if fixtureMarker.MatchString(l.Text) {
				continue
			}
			for _, r := range secretRules {
				if r.re.MatchString(l.Text) {
					out = append(out, finding{SeverityBlocker, f.Path, l.Line, "added line looks like a secret (" + r.name + ", value redacted)"})
					break
				}
			}
		}
	}
	return out
}

// injection matches text that talks to the reviewer instead of to a reader.
// It is a tripwire on top of the prompt's own instructions, not the defence.
var injection = regexp.MustCompile(`(?i)` + strings.Join([]string{
	`(?:ignore|disregard|forget|override)\s+(?:all\s+|any\s+|the\s+|your\s+)*(?:previous|prior|above|earlier|preceding|system)\s+(?:instructions?|prompts?|messages?|rules?|guidelines?)`,
	`(?:please\s+|you\s+must\s+|just\s+)?approve\s+(?:this|the|my)\s+(?:pr|pull\s+request|change|changes|diff|patch|review|code|merge)`,
	`\b(?:ai|llm|automated)\s+reviewers?\b.{0,60}\b(?:approve|lgtm|skip|ignore)`,
	`do\s+not\s+(?:flag|report|mention|raise)\s+(?:this|any|these)`,
	`"approve"\s*:\s*true`,
	`<\|(?:im_start|im_end|system)\|>`,
	`\bnew\s+instructions?\s*:`,
}, "|"))

func injectionFindings(files []FileChange, prTitle string) []finding {
	var out []finding
	if injection.MatchString(prTitle) {
		out = append(out, finding{SeverityBlocker, "", 0, "pull request title contains text addressed to the reviewer (prompt injection attempt)"})
	}
	for _, f := range files {
		for _, l := range f.AddedLines {
			if injection.MatchString(l.Text) {
				out = append(out, finding{SeverityBlocker, f.Path, l.Line, "added text addresses the reviewer (prompt injection attempt)"})
			}
		}
	}
	return out
}

var sourceExts = map[string]bool{
	".go": true, ".py": true, ".js": true, ".jsx": true, ".mjs": true, ".cjs": true, ".ts": true, ".tsx": true,
	".rs": true, ".java": true, ".kt": true, ".scala": true, ".rb": true, ".php": true, ".cs": true,
	".c": true, ".cc": true, ".cpp": true, ".h": true, ".hpp": true, ".swift": true, ".ex": true, ".exs": true,
	".vue": true, ".svelte": true, ".dart": true, ".lua": true, ".sh": true,
}

var testName = regexp.MustCompile(`(?i)(?:_test\.\w+$|\.(?:test|spec)\.\w+$|^test_.+\.py$|_spec\.rb$|(?:Test|Tests|IT)\.(?:java|kt|cs|scala)$|^Test.+\.(?:java|kt|cs|php)$)`)

var testDirs = map[string]bool{"test": true, "tests": true, "__tests__": true, "spec": true, "specs": true, "testdata": true, "e2e": true, "fixtures": true}

func isTestFile(p string) bool {
	segs := strings.Split(p, "/")
	for _, s := range segs[:len(segs)-1] {
		if testDirs[strings.ToLower(s)] {
			return true
		}
	}
	return testName.MatchString(segs[len(segs)-1])
}

func isSourceFile(p string) bool { return sourceExts[strings.ToLower(path.Ext(p))] && !isTestFile(p) }

// docsOrChore reports tasks that ask for no behaviour change: their title or
// first spec line opens with docs/chore ("docs: ...", "[chore] ...") or a
// label says so.
var docsOrChore = regexp.MustCompile(`(?i)^\W*(?:docs?|documentation|chore)\b`)

func exemptFromTests(t factory.Task) bool {
	for _, l := range t.Labels {
		switch strings.ToLower(l) {
		case "docs", "doc", "documentation", "chore":
			return true
		}
	}
	first, _, _ := strings.Cut(strings.TrimSpace(t.Spec), "\n")
	return docsOrChore.MatchString(t.Title) || docsOrChore.MatchString(first)
}

// testFindings flags source changes that bring no test change along.
func testFindings(files []FileChange) []finding {
	var src []string
	for _, f := range files {
		if isTestFile(f.Path) {
			return nil
		}
		if isSourceFile(f.Path) && !f.Deleted && !f.Binary && f.Added+f.Removed > 0 {
			src = append(src, f.Path)
		}
	}
	if len(src) == 0 {
		return nil
	}
	list := strings.Join(src[:min(len(src), 5)], ", ")
	if len(src) > 5 {
		list += fmt.Sprintf(" and %d more", len(src)-5)
	}
	return []finding{{SeverityMajor, "", 0, "source changed without any test that proves it (" + list + "): add or update tests"}}
}
