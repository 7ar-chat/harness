package executors

import (
	"regexp"
	"strings"
)

// mask replaces a secret in text a model or a human will read.
const mask = "***"

// minSecret is the shortest literal secret worth replacing: shorter strings would also mangle ordinary text.
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
	{regexp.MustCompile(`(?i)(x-api-key:\s*)\S+`), "${1}" + mask},
	{regexp.MustCompile(`(https?://[^/\s:@]+:)[^@\s/]+@`), "${1}" + mask + "@"},
	{regexp.MustCompile(`(?i)(api[_-]?key|token|secret|password|passwd)\b("?\s*[:=]\s*"?)([^\s,;"]+)`), "${1}${2}" + mask},
}

// Redact masks secrets in s: every literal in secrets (the credentials of the run) and anything shaped like a
// credential (tokens, API keys, private keys, authorization headers, passwords in URLs or assignments).
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
