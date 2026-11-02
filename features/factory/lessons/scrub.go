package lessons

import (
	"regexp"
	"strings"
)

// redacted replaces anything that looks like a credential.
const redacted = "[REDACTED]"

// The first five patterns are those of features/memory/policy.go (RedactText);
// they are replicated here because a feature must not import another feature.
// The rest cover what CI logs and agents' output carry that a memory entry must not keep.
var (
	inlineKeyPattern   = regexp.MustCompile(`(?i)\b(api[_-]?key|access[_-]?key|token|secret|password|passwd|authorization)\b\s*[:=]\s*("[^"]*"|'[^']*'|[^\s,;]+)`)
	openAIKeyPattern   = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`)
	githubTokenPattern = regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`)
	awsAccessPattern   = regexp.MustCompile(`\b(A3T|AKIA|ASIA)[A-Z0-9]{16}\b`)
	privateKeyPattern  = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(-----END [A-Z ]*PRIVATE KEY-----|$)`)
	slackTokenPattern  = regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`)
	bearerPattern      = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{16,}`)
	jwtPattern         = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]*`)
	urlCredPattern     = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^\s/@:]+:[^\s/@]+@`)
	hexBlobPattern     = regexp.MustCompile(`\b[0-9a-fA-F]{32,}\b`)
	blobPattern        = regexp.MustCompile(`[A-Za-z0-9+/_=-]{40,}`)
)

// scrub replaces secrets and opaque blobs in s. It runs over what is sent to
// the model and over what the model answers, before anything is stored.
func scrub(s string) string {
	s = inlineKeyPattern.ReplaceAllString(s, "$1="+redacted)
	for _, p := range []*regexp.Regexp{privateKeyPattern, openAIKeyPattern, githubTokenPattern, awsAccessPattern,
		slackTokenPattern, bearerPattern, jwtPattern, hexBlobPattern} {
		s = p.ReplaceAllString(s, redacted)
	}
	s = urlCredPattern.ReplaceAllString(s, "${1}"+redacted+"@")
	// A long run of base64-ish characters is a key unless it is a path or an
	// identifier: those are single-case or have no digit.
	return blobPattern.ReplaceAllStringFunc(s, func(m string) string {
		if strings.ContainsAny(m, "0123456789") && strings.ToLower(m) != m && strings.ToUpper(m) != m {
			return redacted
		}
		return m
	})
}
