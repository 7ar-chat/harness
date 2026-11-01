package factory

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

var (
	reHex    = regexp.MustCompile(`\b[0-9a-f]{7,64}\b`)
	reNum    = regexp.MustCompile(`\d+`)
	reTime   = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[t ]\d{2}:\d{2}:\d{2}(\.\d+)?z?`)
	reSpaces = regexp.MustCompile(`\s+`)
)

// fingerprintChars is how much of the normalised text is hashed: the head of an
// error message identifies it, the tail is often a changing stack or log.
const fingerprintChars = 600

// Fingerprint is a stable signature of a failure text: the same error with
// different line numbers, hashes, timestamps or spacing gives the same value.
// "" for blank text.
func Fingerprint(text string) string {
	t := strings.ToLower(strings.TrimSpace(text))
	if t == "" {
		return ""
	}
	t = reTime.ReplaceAllString(t, "T")
	t = reHex.ReplaceAllString(t, "H")
	t = reNum.ReplaceAllString(t, "N")
	t = strings.TrimSpace(reSpaces.ReplaceAllString(t, " "))
	if r := []rune(t); len(r) > fingerprintChars {
		t = string(r[:fingerprintChars])
	}
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:6])
}

// Repeating reports whether the last n signatures are the same non-empty value:
// the task keeps failing the same way and another attempt will not help.
func Repeating(fingerprints []string, n int) bool {
	if n < 2 || len(fingerprints) < n {
		return false
	}
	last := fingerprints[len(fingerprints)-1]
	if last == "" {
		return false
	}
	for _, f := range fingerprints[len(fingerprints)-n:] {
		if f != last {
			return false
		}
	}
	return true
}
