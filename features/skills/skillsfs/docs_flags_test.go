package skillsfs_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// Review round 2 (A7): every command-line flag a built-in skill documents
// (SKILL.md and reference/*.md) must be accepted by the script it is written
// for, so a doc/CLI drift (inspect_xlsx.py --formulas) fails CI.

var (
	docScript = regexp.MustCompile(`(?:/skills/([\w-]+)/scripts/)?\b([\w-]+\.(?:py|sh))\b`)
	// Flags after one of these belong to another program.
	docExternal = regexp.MustCompile(`(?:^|[^\w/.-])(soffice|libreoffice|pandoc|qpdf|pdftoppm|pdftotext|pdfinfo|npm|npx|node|pip|git|curl)\b`)
	docFlag     = regexp.MustCompile(`(?:^|[^\w-])(--[a-z][a-z0-9-]*)`)
)

// documentedFlag is one flag the docs give for a script.
type documentedFlag struct {
	script, flag, where string
}

// skillDocs lists the built-in skills' docs.
func skillDocs(t *testing.T) []string {
	t.Helper()
	docs, _ := filepath.Glob("builtin/*/SKILL.md")
	refs, _ := filepath.Glob("builtin/*/reference/*.md")
	docs = append(docs, refs...)
	if len(docs) < 6 {
		t.Fatalf("docs %v", docs)
	}
	sort.Strings(docs)
	return docs
}

// resolveScript finds the script a doc of skill means by name (its own scripts first).
func resolveScript(skill, explicit, name string) (string, error) {
	if explicit != "" {
		return filepath.Join("builtin", explicit, "scripts", name), nil
	}
	own := filepath.Join("builtin", skill, "scripts", name)
	if _, err := os.Stat(own); err == nil {
		return own, nil
	}
	found, _ := filepath.Glob(filepath.Join("builtin", "*", "scripts", name))
	if len(found) == 1 {
		return found[0], nil
	}
	return "", fmt.Errorf("script %s: %d candidates %v", name, len(found), found)
}

type docEvent struct {
	pos  int
	kind byte // 's' script, 'x' external program, 'f' flag
	m    []int
}

// documentedFlags reads the flags of every doc. A flag belongs to the last
// script named before it on its line; a line without one continues the
// previous line's command (trailing backslash) or the section's last script.
func documentedFlags(t *testing.T) (flags []documentedFlag, scripts map[string]bool, problems []string) {
	t.Helper()
	scripts = map[string]bool{}
	for _, doc := range skillDocs(t) {
		data, err := os.ReadFile(doc)
		if err != nil {
			t.Fatal(err)
		}
		skill := strings.Split(filepath.ToSlash(doc), "/")[1]
		section, cont, fence := "", "", false
		for n, line := range strings.Split(string(data), "\n") {
			where := fmt.Sprintf("%s:%d", doc, n+1)
			if strings.HasPrefix(line, "```") {
				fence = !fence
			}
			if !fence && regexp.MustCompile(`^#{1,4} `).MatchString(line) {
				section = "" // a new section: flags no longer belong to the last script named
			}
			var evs []docEvent
			for _, m := range docScript.FindAllStringSubmatchIndex(line, -1) {
				evs = append(evs, docEvent{m[0], 's', m})
			}
			for _, m := range docExternal.FindAllStringSubmatchIndex(line, -1) {
				evs = append(evs, docEvent{m[2], 'x', m})
			}
			for _, m := range docFlag.FindAllStringSubmatchIndex(line, -1) {
				evs = append(evs, docEvent{m[2], 'f', m})
			}
			sort.SliceStable(evs, func(i, j int) bool { return evs[i].pos < evs[j].pos })
			cur := section
			if cont != "" {
				cur = cont
			}
			for _, e := range evs {
				switch e.kind {
				case 's':
					explicit := ""
					if e.m[2] >= 0 {
						explicit = line[e.m[2]:e.m[3]]
					}
					p, err := resolveScript(skill, explicit, line[e.m[4]:e.m[5]])
					if err != nil && explicit == "" && e.m[4] > 0 && line[e.m[4]-1] == '/' {
						cur = "-" // a script the agent writes itself (/workspace/.work/make_doc.py)
						continue
					}
					if err != nil {
						problems = append(problems, where+": "+err.Error())
						cur = ""
						continue
					}
					scripts[p] = true
					cur, section = p, p
				case 'x':
					cur = "-"
				case 'f':
					flag := line[e.m[2]:e.m[3]]
					switch cur {
					case "-":
					case "":
						problems = append(problems, fmt.Sprintf("%s: %s is not after a script name: name the script it belongs to", where, flag))
					default:
						flags = append(flags, documentedFlag{cur, flag, where})
					}
				}
			}
			cont = ""
			if strings.HasSuffix(strings.TrimSpace(line), `\`) {
				cont = cur
			}
		}
	}
	return flags, scripts, problems
}

// acceptedFlags lists, one per line, the options a script accepts, read
// statically (no interpreter runs, so nothing is written into the skill tree):
// a Python script's argparse definitions — the option strings ("-x",
// "--name") that open each add_argument(...) call, subcommands and groups
// included — and --help; a shell script's --name) case patterns and
// [[ $x == --name ]] tests. The skills suite (test_packages.py) checks the same
// docs against each script's --help inside the sandbox image.
func acceptedFlags(t *testing.T, script string) string {
	t.Helper()
	src, err := os.ReadFile(script)
	if err != nil {
		return ""
	}
	var b strings.Builder
	if strings.HasSuffix(script, ".sh") {
		for _, m := range shCaseFlag.FindAllStringSubmatch(string(src), -1) {
			b.WriteString(m[1] + "\n")
		}
		for _, m := range shTestFlag.FindAllStringSubmatch(string(src), -1) {
			b.WriteString(m[1] + "\n")
		}
		return b.String()
	}
	code := string(src)
	if strings.Contains(code, "argparse") {
		b.WriteString("-h\n--help\n")
	}
	for _, loc := range addArgument.FindAllStringIndex(code, -1) {
		rest := code[loc[1]:]
		for {
			m := pyStringArg.FindStringSubmatch(rest)
			if m == nil {
				break
			}
			if lit := m[1] + m[2]; strings.HasPrefix(lit, "-") { // one of them is empty
				b.WriteString(lit + "\n")
			}
			if strings.HasSuffix(m[0], ")") {
				break // the call ends
			}
			rest = rest[len(m[0]):]
		}
	}
	return b.String()
}

var (
	// addArgument opens an argparse option definition (parser, group or subparser).
	addArgument = regexp.MustCompile(`\.add_argument\(`)
	// pyStringArg is one leading positional string argument of a call, then its comma.
	pyStringArg = regexp.MustCompile(`^\s*(?:[rRuU]?(?:"((?:[^"\\\n]|\\.)*)"|'((?:[^'\\\n]|\\.)*)'))\s*(?:,|\))`)
	shCaseFlag  = regexp.MustCompile(`(?m)^\s*(?:[^)\n]*\|)?\s*(--[a-z][a-z0-9-]*)\)`)
	shTestFlag  = regexp.MustCompile(`==\s*"?(--[a-z][a-z0-9-]*)"?`)
)

func TestEveryDocumentedScriptFlagIsAccepted(t *testing.T) {
	// Every --flag that a built-in skill's SKILL.md or reference documents for one of its scripts is accepted by that script (its --help lists it), and every script the docs name exists.
	// Arrange
	flags, scripts, problems := documentedFlags(t)
	if len(flags) < 50 {
		t.Fatalf("only %d documented flags found: the doc parser is broken", len(flags))
	}

	// Act
	accepted := map[string]string{}
	for s := range scripts {
		if _, err := os.Stat(s); err != nil {
			problems = append(problems, "documented script missing: "+s)
			continue
		}
		accepted[s] = acceptedFlags(t, s)
	}

	// Assert
	for _, f := range flags {
		help, ok := accepted[f.script]
		if !ok {
			continue
		}
		if !regexp.MustCompile(`(^|[^\w-])` + regexp.QuoteMeta(f.flag) + `($|[^\w-])`).MatchString(help) {
			problems = append(problems, fmt.Sprintf("%s: %s does not accept %s", f.where, filepath.Base(f.script), f.flag))
		}
	}
	for _, p := range problems {
		t.Error(p)
	}
}

func TestSharedScriptCopiesMatchTheirSource(t *testing.T) {
	// The helper scripts several skills ship (cross_check.py, lo.py, blocks.py, opserr.py) have one source of truth, shared/<name>: every skill's copy is byte-identical to it (bash sync_shared.sh copies it), so a skill never depends on another one being loaded and a fix reaches all of them.
	// Arrange
	want := map[string][]string{ // the skills that must ship each helper
		"cross_check.py": {"docx", "pptx", "xlsx", "pdf"},
		"lo.py":          {"docx", "pptx", "pdf"},
		"blocks.py":      {"docx", "pdf"},
		"opserr.py":      {"docx", "xlsx"},
	}
	sources, _ := filepath.Glob(filepath.Join("shared", "*"))

	// Act
	copies := map[string][]string{}
	for _, src := range sources {
		found, _ := filepath.Glob(filepath.Join("builtin", "*", "scripts", filepath.Base(src)))
		copies[src] = found
	}

	// Assert
	if len(sources) != len(want) {
		t.Errorf("shared/ holds %v, want %v", sources, want)
	}
	for _, src := range sources {
		name := filepath.Base(src)
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		var skills []string
		for _, c := range copies[src] {
			skills = append(skills, strings.Split(filepath.ToSlash(c), "/")[1])
			if got, _ := os.ReadFile(c); !bytes.Equal(got, data) {
				t.Errorf("%s differs from %s: run bash work/skills/sync_shared.sh", c, src)
			}
		}
		for _, s := range want[name] {
			if !slices.Contains(skills, s) {
				t.Errorf("builtin/%s/scripts/%s is missing (copy it from %s)", s, name, src)
			}
		}
	}
}

func TestTheFlagParserReadsOnlyArgparseDefinitions(t *testing.T) {
	// A script accepts exactly the option strings of its add_argument calls (subcommands and groups included) and --help: an option-like string in a help text, a default, a docstring or a constant is not an option.
	// Arrange
	script := filepath.Join("testdata", "flags", "fixture.py")

	// Act
	accepted := acceptedFlags(t, script)

	// Assert
	has := func(flag string) bool {
		return regexp.MustCompile(`(^|[^\w-])` + regexp.QuoteMeta(flag) + `($|[^\w-])`).MatchString(accepted)
	}
	for _, f := range []string{"--help", "-o", "--out", "--single-quoted", "--in-group", "--alias-in-group", "--dpi"} {
		if !has(f) {
			t.Errorf("%s is defined but not accepted", f)
		}
	}
	for _, f := range []string{"--not-an-option", "--quoted-elsewhere", "--mentioned-in-help", "--a-default", "--not-a-flag"} {
		if has(f) {
			t.Errorf("%s is not an option but is accepted", f)
		}
	}
}
