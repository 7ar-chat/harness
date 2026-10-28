package memory

import (
	"regexp"
	"strings"
	"unicode"
)

// Redacted replaces secrets in memory content, metadata, tags and queries.
const Redacted = "[REDACTED]"

// Secret patterns of Work's SecretRedactor (codex-memory-proxy).
var (
	inlineKeyPattern   = regexp.MustCompile(`(?i)\b(api[_-]?key|token|secret|password|passwd)\b\s*[:=]\s*([^\s,;]+)`)
	openAIKeyPattern   = regexp.MustCompile(`\bsk-[A-Za-z0-9]{20,}\b`)
	githubTokenPattern = regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`)
	awsAccessPattern   = regexp.MustCompile(`\b(A3T|AKIA|ASIA)[A-Z0-9]{16}\b`)
	privateKeyPattern  = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)
)

var sensitiveMetadataKeys = map[string]bool{
	"api_key": true, "apikey": true, "authorization": true, "credential": true, "credentials": true,
	"password": true, "passwd": true, "private_key": true, "secret": true, "token": true,
}

var sensitiveMetadataSuffixes = []string{
	"_api_key", "_authorization", "_credential", "_credentials", "_password", "_passwd", "_private_key", "_secret", "_token",
}

// RedactText replaces secrets in text; it returns the text and the number of replacements.
func RedactText(text string) (string, int) {
	hits := 0
	text = inlineKeyPattern.ReplaceAllStringFunc(text, func(m string) string {
		hits++
		sub := inlineKeyPattern.FindStringSubmatch(m)
		sep := "="
		if strings.Contains(m, ":") {
			sep = ":"
		}
		return sub[1] + sep + " " + Redacted
	})
	for _, p := range []*regexp.Regexp{openAIKeyPattern, githubTokenPattern, awsAccessPattern, privateKeyPattern} {
		n := len(p.FindAllStringIndex(text, -1))
		if n > 0 {
			text = p.ReplaceAllString(text, Redacted)
			hits += n
		}
	}
	return text, hits
}

// normalizeMetadataKey turns "apiKey" / "API-Key" into "api_key".
func normalizeMetadataKey(key string) string {
	var b strings.Builder
	runes := []rune(key)
	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) && r < unicode.MaxASCII {
			prev := runes[i-1]
			if (prev >= 'a' && prev <= 'z') || (prev >= '0' && prev <= '9') {
				b.WriteByte('_')
			}
		}
		b.WriteRune(r)
	}
	lowered := strings.ToLower(b.String())
	var out strings.Builder
	sep := false
	for _, r := range lowered {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if sep && out.Len() > 0 {
				out.WriteByte('_')
			}
			sep = false
			out.WriteRune(r)
			continue
		}
		sep = true
	}
	return out.String()
}

func sensitiveKey(key string) bool {
	n := normalizeMetadataKey(key)
	if sensitiveMetadataKeys[n] {
		return true
	}
	for _, s := range sensitiveMetadataSuffixes {
		if strings.HasSuffix(n, s) {
			return true
		}
	}
	return false
}

// RedactValue redacts every string of a JSON-like value and the values of
// sensitive keys (password, token, *_secret…); it returns the count of replacements.
func RedactValue(v any) (any, int) {
	switch x := v.(type) {
	case string:
		return RedactText(x)
	case []any:
		out := make([]any, len(x))
		total := 0
		for i, item := range x {
			r, n := RedactValue(item)
			out[i] = r
			total += n
		}
		return out, total
	case map[string]any:
		out := make(map[string]any, len(x))
		total := 0
		for k, item := range x {
			if item != nil && sensitiveKey(k) {
				out[k] = Redacted
				total++
				continue
			}
			r, n := RedactValue(item)
			out[k] = r
			total += n
		}
		return out, total
	default:
		return v, 0
	}
}

// Write modes of the policy: warn records violations, strict blocks them.
const (
	WriteWarn   = "warn"
	WriteStrict = "strict"
)

// Delete modes: soft keeps the entry as status deleted, hard removes it.
const (
	DeleteSoft = "soft"
	DeleteHard = "hard"
)

// Write triggers (metadata.trigger) of Work's memory policy engine.
var triggerValues = map[string]bool{
	"explicit_remember": true, "durable_preference": true, "accepted_decision": true, "stale_correction": true,
	"explicit_revocation": true, "explicit_contradiction": true, "discovered_fact": true, "investigation": true,
}

type triggerPattern struct {
	name string
	re   *regexp.Regexp
}

// triggerPatterns infer the trigger from reason / user_message / content, in Work's order.
var triggerPatterns = []triggerPattern{
	{"explicit_remember", regexp.MustCompile(`(?i)\bremember\s+(this|that|it)\b`)},
	{"durable_preference", regexp.MustCompile(`(?i)\b(prefer|always|never|must|constraint|default)\b`)},
	{"accepted_decision", regexp.MustCompile(`(?i)\b(decision|decided|architecture|approved|standardize)\b`)},
	{"stale_correction", regexp.MustCompile(`(?i)\b(correction|stale|outdated|update\s+memory|actually)\b`)},
	{"explicit_revocation", regexp.MustCompile(`(?i)\b(forget|revoke|remove\s+memory)\b`)},
	{"explicit_contradiction", regexp.MustCompile(`(?i)\b(contradict|no\s+longer|instead\s+of|supersede)\b`)},
	{"discovered_fact", regexp.MustCompile(`(?i)\b(discovered|observed|found)\b`)},
	{"investigation", regexp.MustCompile(`(?i)\b(investigat(?:e|ion)|root\s+cause|reproduced)\b`)},
}

// Precedence is the conflict precedence reported with every policy decision.
var Precedence = []string{"current_user_message", "system_developer_policy", "repo_policy_files", "stored_memory"}

// Decision is the outcome of the write policy for one mutation.
type Decision struct {
	Allowed          bool
	Trigger          string
	WriteMode        string
	Violations       []string
	Warnings         []string
	RedactedContent  string
	RedactedMetadata map[string]any
}

// Payload is the "policy" object of the tool results.
func (d Decision) Payload() map[string]any {
	var trigger any
	if d.Trigger != "" {
		trigger = d.Trigger
	}
	return map[string]any{
		"write_mode": d.WriteMode, "trigger": trigger, "violations": nonNil(d.Violations), "warnings": nonNil(d.Warnings),
		"allowed": d.Allowed, "precedence": append([]string{}, Precedence...),
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Evaluate applies Work's MemoryPolicyEngine: redaction of content and
// metadata, trigger resolution (create/update need one, delete needs a
// revocation, contradiction or stale correction) and, in strict mode, blocking.
func Evaluate(writeMode, action, content string, metadata map[string]any) Decision {
	redactedContent, contentHits := RedactText(content)
	redactedMeta, metaHits := RedactValue(metadata)
	meta, _ := redactedMeta.(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	d := Decision{WriteMode: writeMode, RedactedContent: redactedContent, RedactedMetadata: meta, Allowed: true}
	d.Trigger = resolveTrigger(action, metadata, content)
	if (action == "create" || action == "update") && d.Trigger == "" {
		d.Violations = append(d.Violations, "write_trigger_missing")
	}
	if action == "delete" && d.Trigger != "explicit_revocation" && d.Trigger != "explicit_contradiction" && d.Trigger != "stale_correction" {
		d.Violations = append(d.Violations, "delete_trigger_missing")
	}
	if contentHits+metaHits > 0 {
		d.Warnings = append(d.Warnings, "secret_redacted")
	}
	if writeMode == WriteStrict && len(d.Violations) > 0 {
		d.Allowed = false
	}
	return d
}

func metaString(m map[string]any, key string) string {
	if m == nil || m[key] == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return strings.TrimSpace(stringify(m[key]))
}

func resolveTrigger(action string, metadata map[string]any, content string) string {
	if t := strings.ToLower(strings.TrimSpace(metaString(metadata, "trigger"))); triggerValues[t] {
		return t
	}
	reason := strings.ToLower(strings.TrimSpace(metaString(metadata, "reason")))
	userMessage := strings.TrimSpace(metaString(metadata, "user_message"))
	var parts []string
	for _, p := range []string{reason, userMessage, content} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	text := strings.Join(parts, "\n")
	for _, p := range triggerPatterns {
		if p.re.MatchString(text) {
			return p.name
		}
	}
	if action == "delete" && reason != "" {
		return "explicit_revocation"
	}
	return ""
}
