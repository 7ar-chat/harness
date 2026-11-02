package permissions

import (
	"regexp"
	"strings"
	"sync"
)

var globCache sync.Map // pattern → *regexp.Regexp

// globCommand matches a command pattern: "*" is any text (spaces and slashes
// included), "?" one character. The whole text must match.
func globCommand(pattern, text string) bool {
	return compile("c:"+pattern, func() string {
		var b strings.Builder
		for _, r := range pattern {
			switch r {
			case '*':
				b.WriteString(`.*`)
			case '?':
				b.WriteString(`.`)
			case '|':
				b.WriteString(`\s*\|\s*`) // a pipeline pattern: spaces around the pipe are optional
			default:
				b.WriteString(regexp.QuoteMeta(string(r)))
			}
		}
		return b.String()
	}).MatchString(text)
}

// globPath matches a path pattern: "*" and "?" stay inside one path element,
// "**" crosses elements and "**/" also matches no directory at all.
func globPath(pattern, text string) bool {
	return compile("p:"+pattern, func() string {
		var b strings.Builder
		rs := []rune(pattern)
		for i := 0; i < len(rs); i++ {
			switch rs[i] {
			case '*':
				if i+1 < len(rs) && rs[i+1] == '*' {
					i++
					if i+1 < len(rs) && rs[i+1] == '/' {
						i++
						b.WriteString(`(?:.*/)?`)
					} else {
						b.WriteString(`.*`)
					}
				} else {
					b.WriteString(`[^/]*`)
				}
			case '?':
				b.WriteString(`[^/]`)
			default:
				b.WriteString(regexp.QuoteMeta(string(rs[i])))
			}
		}
		return b.String()
	}).MatchString(text)
}

func compile(key string, body func() string) *regexp.Regexp {
	if v, ok := globCache.Load(key); ok {
		return v.(*regexp.Regexp)
	}
	re := regexp.MustCompile(`(?s)^` + body() + `$`)
	globCache.Store(key, re)
	return re
}
