package forge

import (
	"regexp"
	"strings"
)

// mask replaces a secret in text the model or a human will read.
const mask = "***"

// minSecret is the shortest literal secret worth replacing: shorter strings
// would also mangle ordinary text.
const minSecret = 6

var secretPatterns = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`), mask},
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`), mask},
	{regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`), mask},
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`), mask},
	{regexp.MustCompile(`\b(?:A3T|AKIA|ASIA)[A-Z0-9]{16}\b`), mask},
	{regexp.MustCompile(`(?i)(authorization:\s*(?:(?:bearer|basic|token)\s+)?)\S+`), "${1}" + mask},
	{regexp.MustCompile(`(https?://[^/\s:@]+:)[^@\s/]+@`), "${1}" + mask + "@"},
	{regexp.MustCompile(`(?i)\b(api[_-]?key|token|secret|password|passwd)\b(\s*[:=]\s*)([^\s,;]+)`), "${1}${2}" + mask},
}

// Redact masks secrets in s: every literal in secrets (the token that was just
// used) and anything shaped like a credential (GitHub tokens, API keys,
// private keys, authorization headers, passwords in URLs or assignments).
func Redact(s string, secrets ...string) string {
	for _, sec := range secrets {
		if len(sec) >= minSecret {
			s = strings.ReplaceAll(s, sec, mask)
		}
	}
	for _, p := range secretPatterns {
		s = p.re.ReplaceAllString(s, p.with)
	}
	return s
}

var (
	reANSI      = regexp.MustCompile("\x1b(?:\\[[0-?]*[ -/]*[@-~]|\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|[@-Z\\\\-_])")
	reLogTimeLn = regexp.MustCompile(`(?m)^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?Z `)
)

// cleanLog makes a CI log readable for a model: no colour codes, no carriage
// returns, no per-line timestamps.
func cleanLog(s string) string {
	s = reANSI.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return reLogTimeLn.ReplaceAllString(s, "")
}

// tailRunes returns the last n runes of s; a cut is marked with an ellipsis line.
func tailRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…\n" + string(r[len(r)-n:])
}

// clipRunes returns the first n runes of s with an ellipsis when it was cut.
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
