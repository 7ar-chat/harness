package memory

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// pathPrefix is the memory root of Work's file layout; paths given with it are accepted.
const pathPrefix = ".astraforge/memory/"

// SemanticKinds are the folders of memories/semantic/ (and shared/memories/semantic/).
var SemanticKinds = []string{"decisions", "facts", "constraints", "preferences", "procedures", "notes"}

// NormalizePath applies Work's _normalize_relative_path: a relative path
// without "..", not under proxy/ (the root prefix .astraforge/memory/ is stripped).
func NormalizePath(raw string) (string, *Error) {
	p := strings.ReplaceAll(strings.TrimSpace(raw), "\\", "/")
	p = strings.TrimPrefix(p, pathPrefix)
	if strings.HasPrefix(p, "/") {
		return "", newError("invalid_path", "Absolute paths are not allowed", map[string]any{"path": raw})
	}
	var parts []string
	for _, part := range strings.Split(p, "/") {
		switch part {
		case "", ".":
			continue
		case "..":
			return "", newError("invalid_path", "Path traversal is not allowed", map[string]any{"path": raw})
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return "", newError("invalid_path", "Path must not be empty", map[string]any{"path": raw})
	}
	out := strings.Join(parts, "/")
	if strings.HasPrefix(out, "proxy/") {
		return "", newError("invalid_path", "Proxy internal paths cannot be modified", map[string]any{"path": raw})
	}
	if utf8.RuneCountInString(out) > MaxPathChars || strings.ContainsRune(out, 0) {
		return "", newError("invalid_path", fmt.Sprintf("Path must be at most %d characters", MaxPathChars), map[string]any{"path": raw})
	}
	return out, nil
}

// normalizePrefix is a path_prefix filter without its trailing slash.
func normalizePrefix(raw string) (string, *Error) {
	p, err := NormalizePath(raw)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(p, "/"), nil
}

// assertDurable rejects the transient runs/ and handoff/ trees (Work's _assert_durable_mutation_path).
func assertDurable(path string) *Error {
	top, _, _ := strings.Cut(path, "/")
	if top == "runs" || top == "handoff" {
		return newError("transient_path", "Transient runs/ and handoff/ artifacts must use workspace file tools", map[string]any{"path": path})
	}
	return nil
}

var kindAliases = map[string]string{
	"decision": "decisions", "decisions": "decisions", "accepted_decision": "decisions", "architecture_decision": "decisions",
	"fact": "facts", "facts": "facts", "discovery": "facts", "discoveries": "facts", "discovered_fact": "facts",
	"constraint": "constraints", "constraints": "constraints",
	"preference": "preferences", "preferences": "preferences", "durable_preference": "preferences",
	"procedure": "procedures", "procedures": "procedures", "investigation": "procedures", "playbook": "procedures", "runbook": "procedures",
	"note": "notes", "notes": "notes",
}

func kindFromHints(hints []string) string {
	for _, h := range hints {
		n := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(h)), "-", "_")
		if k, ok := kindAliases[n]; ok {
			return k
		}
	}
	return ""
}

type kindPattern struct {
	kind string
	re   *regexp.Regexp
}

var contentKindPatterns = []kindPattern{
	{"decisions", regexp.MustCompile(`\b(decided|decision|architecture decision|approved)\b`)},
	{"facts", regexp.MustCompile(`\b(discovered|found|fact|observed)\b`)},
	{"constraints", regexp.MustCompile(`\b(constraint|must|must not|cannot|required)\b`)},
	{"preferences", regexp.MustCompile(`\b(prefer|preference|default to|always use|never use)\b`)},
	{"procedures", regexp.MustCompile(`\b(procedure|playbook|steps|workflow|runbook)\b`)},
	{"facts", regexp.MustCompile(`\b(discovered|observed|found)\b`)},
	{"procedures", regexp.MustCompile(`\b(investigat(?:e|ion)|root cause|reproduced)\b`)},
}

// DefaultKind is Work's _default_semantic_kind: metadata hints (kind, type,
// category, memory_type), then tags, then the trigger, then the content.
func DefaultKind(content string, metadata map[string]any, tags []string, trigger string) string {
	var meta []string
	for _, k := range []string{"kind", "type", "category", "memory_type"} {
		meta = append(meta, metaString(metadata, k))
	}
	for _, hints := range [][]string{meta, tags, {trigger}} {
		if k := kindFromHints(hints); k != "" {
			return k
		}
	}
	c := strings.ToLower(strings.TrimSpace(content))
	for _, p := range contentKindPatterns {
		if p.re.MatchString(c) {
			return p.kind
		}
	}
	return "notes"
}

// DefaultPath is memories/semantic/{kind}/{YYYY-MM-DD}-{id}.md.
func DefaultPath(id, kind string, now time.Time) string {
	return "memories/semantic/" + kind + "/" + now.UTC().Format("2006-01-02") + "-" + id + ".md"
}

// NormalizeTags trims, redacts and de-duplicates (case-insensitively) tags.
func NormalizeTags(tags []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		t, _ = RedactText(t)
		if seen[strings.ToLower(t)] {
			continue
		}
		seen[strings.ToLower(t)] = true
		out = append(out, t)
	}
	return out
}

var headingMarks = regexp.MustCompile(`^(#{1,6}\s+|[-*]\s+)`)

// DeriveTitle is the first non-empty line of content without markdown marks,
// whitespace collapsed, at most MaxTitleChars characters.
func DeriveTitle(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.Join(strings.Fields(headingMarks.ReplaceAllString(strings.TrimSpace(line), "")), " ")
		if line != "" {
			return truncateRunes(line, MaxTitleChars)
		}
	}
	return ""
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return strings.TrimSpace(string([]rune(s)[:n]))
}
