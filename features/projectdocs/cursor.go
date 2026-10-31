package projectdocs

import (
	"context"
	"path"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/7ar-chat/harness/core/sandbox"
)

// cursorRule is a Cursor project rule (.cursor/rules/*.mdc): a markdown body
// with an optional YAML header (description, globs, alwaysApply).
type cursorRule struct {
	// rel is the path relative to the workspace.
	rel    string
	always bool
	globs  []string
	re     []*regexp.Regexp
	body   string
}

const cursorRulesDir = sandbox.Workspace + "/.cursor/rules"

// cursorRules lists and parses the rules of the workspace, sorted by path.
func (l *loader) cursorRules(ctx context.Context) []cursorRule {
	sbSrc, ok := l.ws.(*sandboxSource)
	if !ok {
		return nil
	}
	files, err := sbSrc.sb.List(ctx, cursorRulesDir, 3)
	if err != nil {
		return nil
	}
	var paths []string
	for _, f := range files {
		if f.Type == "file" && (strings.HasSuffix(f.Path, ".mdc") || strings.HasSuffix(f.Path, ".md")) {
			paths = append(paths, f.Path)
		}
	}
	sort.Strings(paths)
	var out []cursorRule
	for _, p := range paths {
		if !strings.HasPrefix(p, "/") {
			p = path.Join(cursorRulesDir, p)
		}
		data, size, ok := l.ws.read(ctx, p, MaxBytes)
		if !ok {
			continue
		}
		r := parseMDC(string(data), size > int64(len(data)))
		r.rel = l.ws.rel(p)
		out = append(out, r)
	}
	return out
}

// parseMDC reads a rule file.
func parseMDC(text string, cut bool) cursorRule {
	var r cursorRule
	body := text
	if rest, ok := strings.CutPrefix(text, "---\n"); ok {
		if i := strings.Index(rest, "\n---"); i >= 0 {
			var head struct {
				Globs       any  `yaml:"globs"`
				AlwaysApply bool `yaml:"alwaysApply"`
			}
			if err := yaml.Unmarshal([]byte(rest[:i]), &head); err == nil {
				r.always = head.AlwaysApply
				r.globs = globList(head.Globs)
			}
			body = strings.TrimPrefix(rest[i+len("\n---"):], "\n")
		}
	}
	r.body = strings.ToValidUTF8(strings.TrimSpace(body), "")
	if cut {
		r.body += "\n[…cut: the rule file is longer]"
	}
	for _, g := range r.globs {
		if re, err := regexp.Compile(globRegexp(g)); err == nil {
			r.re = append(r.re, re)
		}
	}
	return r
}

func globList(v any) []string {
	var out []string
	switch t := v.(type) {
	case string:
		out = splitGlobs(t)
	case []any:
		for _, x := range t {
			if s, ok := x.(string); ok {
				if s = strings.Trim(strings.TrimSpace(s), `"'`); s != "" {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

// splitGlobs splits "a, b" at the commas that are not inside {braces}.
func splitGlobs(s string) []string {
	var out []string
	depth, start := 0, 0
	flush := func(end int) {
		if g := strings.Trim(strings.TrimSpace(s[start:end]), `"'`); g != "" {
			out = append(out, g)
		}
	}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				flush(i)
				start = i + 1
			}
		}
	}
	flush(len(s))
	return out
}

// matches reports whether the rule's globs cover the workspace-relative path.
func (r cursorRule) matches(rel string) bool {
	for _, re := range r.re {
		if re.MatchString(rel) {
			return true
		}
	}
	return false
}

// globRegexp turns a glob (**, *, ?, {a,b}) into an anchored regular
// expression over a slash separated path. A glob without a slash matches at any depth.
func globRegexp(g string) string {
	g = strings.TrimPrefix(g, "./")
	if !strings.Contains(g, "/") {
		g = "**/" + g
	}
	var b strings.Builder
	b.WriteString("^")
	depth := 0
	for i := 0; i < len(g); i++ {
		switch c := g[i]; c {
		case '*':
			if i+1 < len(g) && g[i+1] == '*' {
				i++
				if i+1 < len(g) && g[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '{':
			depth++
			b.WriteString("(?:")
		case '}':
			if depth > 0 {
				depth--
				b.WriteString(")")
			} else {
				b.WriteString(`\}`)
			}
		case ',':
			if depth > 0 {
				b.WriteString("|")
			} else {
				b.WriteString(",")
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	for ; depth > 0; depth-- {
		b.WriteString(")")
	}
	b.WriteString("$")
	return b.String()
}
