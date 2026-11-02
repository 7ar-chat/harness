package policy

import (
	"strings"
	"unicode"
)

// word is one shell word with quotes removed. dyn is set when the word holds
// an expansion ($var, $(...), backticks, ANSI-C quoting) whose value is not
// known statically.
type word struct {
	text string
	dyn  bool
}

// simple is one simple command of a script: its words, the files it writes
// and reads through redirections, and whether its stdin is a pipe.
type simple struct {
	words  []word
	writes []word
	reads  []word
	pipeIn bool
}

// parsed is the result of splitting a script.
type parsed struct {
	cmds []simple
	// subs are the bodies of command substitutions, backticks and process
	// substitutions: they run as commands too.
	subs []string
	// uncertain is set when the script could not be split with confidence
	// (an unterminated quote or substitution).
	uncertain bool
}

type heredoc struct {
	delim string
	strip bool
}

const (
	redirNone = iota
	redirWrite
	redirRead
	redirDiscard
)

type shellParser struct {
	src      []rune
	i        int
	cur      strings.Builder
	inWord   bool
	dyn      bool
	words    []word
	writes   []word
	reads    []word
	redir    int
	pipeIn   bool
	heredocs []heredoc
	out      parsed
}

// parseShell splits a script into simple commands on && || ; | & and
// newlines, respecting quotes and backslashes. It does not recurse: callers
// classify the returned substitution bodies themselves.
func parseShell(script string) parsed {
	p := &shellParser{src: []rune(script)}
	p.run()
	p.endCmd(";")
	return p.out
}

func (p *shellParser) peek(off int) rune {
	if p.i+off < len(p.src) {
		return p.src[p.i+off]
	}
	return 0
}

func (p *shellParser) endWord() {
	if !p.inWord {
		return
	}
	w := word{text: p.cur.String(), dyn: p.dyn}
	p.cur.Reset()
	p.inWord, p.dyn = false, false
	switch p.redir {
	case redirWrite:
		p.writes = append(p.writes, w)
	case redirRead:
		p.reads = append(p.reads, w)
	case redirDiscard:
	default:
		p.words = append(p.words, w)
	}
	p.redir = redirNone
}

func (p *shellParser) endCmd(op string) {
	p.endWord()
	if len(p.words) > 0 || len(p.writes) > 0 || len(p.reads) > 0 {
		p.out.cmds = append(p.out.cmds, simple{words: p.words, writes: p.writes, reads: p.reads, pipeIn: p.pipeIn})
	}
	p.words, p.writes, p.reads = nil, nil, nil
	p.redir = redirNone
	p.pipeIn = op == "|"
}

func (p *shellParser) add(r rune) {
	p.inWord = true
	p.cur.WriteRune(r)
}

func (p *shellParser) run() {
	for p.i < len(p.src) {
		c := p.src[p.i]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			p.endWord()
			p.i++
		case c == '\n':
			p.endCmd(";")
			p.i++
			p.skipHeredocs()
		case c == '\\':
			switch n := p.peek(1); {
			case n == '\n':
				p.i += 2
			case n == 0:
				p.i++
			default:
				p.add(n)
				p.i += 2
			}
		case c == '\'':
			p.inWord = true
			p.i++
			start := p.i
			for p.i < len(p.src) && p.src[p.i] != '\'' {
				p.i++
			}
			p.cur.WriteString(string(p.src[start:p.i]))
			if p.i >= len(p.src) {
				p.out.uncertain = true
			} else {
				p.i++
			}
		case c == '"':
			p.dquote()
		case c == '`':
			p.backtick()
		case c == '$':
			p.dollar()
		case c == '#' && !p.inWord:
			for p.i < len(p.src) && p.src[p.i] != '\n' {
				p.i++
			}
		case c == ';':
			p.endCmd(";")
			p.i++
			if p.peek(0) == ';' {
				p.i++
			}
		case c == '&':
			switch p.peek(1) {
			case '&':
				p.endCmd("&&")
				p.i += 2
			case '>':
				p.endWord()
				p.i += 2
				if p.peek(0) == '>' {
					p.i++
				}
				p.redir = redirWrite
			default:
				p.endCmd("&")
				p.i++
			}
		case c == '|':
			switch p.peek(1) {
			case '|':
				p.endCmd("||")
				p.i += 2
			case '&':
				p.endCmd("|")
				p.i += 2
			default:
				p.endCmd("|")
				p.i++
			}
		case c == '(' || c == ')':
			p.endCmd(";")
			p.i++
		case c == '>':
			p.redirOut()
		case c == '<':
			p.redirIn()
		default:
			p.add(c)
			p.i++
		}
	}
}

// fdPrefix drops a bare numeric word ("2" of "2>") that belongs to a redirection.
func (p *shellParser) fdPrefix() {
	if p.inWord && p.redir == redirNone {
		s := p.cur.String()
		if s != "" && strings.Trim(s, "0123456789") == "" {
			p.cur.Reset()
			p.inWord, p.dyn = false, false
			return
		}
	}
	p.endWord()
}

func (p *shellParser) redirOut() {
	if p.peek(1) == '(' { // >( process substitution
		p.procSub()
		return
	}
	p.fdPrefix()
	p.i++
	switch p.peek(0) {
	case '>', '|':
		p.i++
	case '&':
		p.i++
		if r := p.peek(0); r == '-' || unicode.IsDigit(r) {
			for p.i < len(p.src) && (p.src[p.i] == '-' || unicode.IsDigit(p.src[p.i])) {
				p.i++
			}
			return
		}
	}
	p.redir = redirWrite
}

func (p *shellParser) redirIn() {
	if p.peek(1) == '(' { // <( process substitution
		p.procSub()
		return
	}
	p.fdPrefix()
	switch {
	case p.peek(1) == '<' && p.peek(2) == '<':
		p.i += 3
		p.redir = redirDiscard
	case p.peek(1) == '<':
		p.i += 2
		h := heredoc{}
		if p.peek(0) == '-' {
			h.strip = true
			p.i++
		}
		for p.peek(0) == ' ' || p.peek(0) == '\t' {
			p.i++
		}
		var d strings.Builder
		for p.i < len(p.src) {
			r := p.src[p.i]
			if r == ' ' || r == '\t' || r == '\n' || r == ';' || r == '&' || r == '|' || r == ')' || r == '<' || r == '>' {
				break
			}
			if r != '\'' && r != '"' && r != '\\' {
				d.WriteRune(r)
			}
			p.i++
		}
		h.delim = d.String()
		p.heredocs = append(p.heredocs, h)
	case p.peek(1) == '&':
		p.i += 2
		for p.i < len(p.src) && (p.src[p.i] == '-' || unicode.IsDigit(p.src[p.i])) {
			p.i++
		}
	default:
		p.i++
		p.redir = redirRead
	}
}

// skipHeredocs consumes the bodies of the here-documents opened on the line
// that just ended: they are data, not commands.
func (p *shellParser) skipHeredocs() {
	for _, h := range p.heredocs {
		for p.i < len(p.src) {
			end := p.i
			for end < len(p.src) && p.src[end] != '\n' {
				end++
			}
			line := string(p.src[p.i:end])
			if end < len(p.src) {
				end++
			}
			p.i = end
			if h.strip {
				line = strings.TrimLeft(line, "\t")
			}
			if line == h.delim {
				break
			}
		}
	}
	p.heredocs = nil
}

// matchParen returns the index of the ")" closing a "(" whose body starts at
// i, or -1.
func matchParen(src []rune, i int) int {
	depth := 1
	for ; i < len(src); i++ {
		switch src[i] {
		case '\\':
			i++
		case '\'':
			for i++; i < len(src) && src[i] != '\''; i++ {
			}
		case '"':
			for i++; i < len(src) && src[i] != '"'; i++ {
				if src[i] == '\\' {
					i++
				}
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func (p *shellParser) procSub() {
	// p.src[p.i] is < or >, p.src[p.i+1] is (.
	end := matchParen(p.src, p.i+2)
	p.inWord, p.dyn = true, true
	if end < 0 {
		p.out.uncertain = true
		p.out.subs = append(p.out.subs, string(p.src[p.i+2:]))
		p.cur.WriteString(string(p.src[p.i:]))
		p.i = len(p.src)
		return
	}
	p.out.subs = append(p.out.subs, string(p.src[p.i+2:end]))
	p.cur.WriteString(string(p.src[p.i : end+1]))
	p.i = end + 1
}

func (p *shellParser) backtick() {
	start := p.i
	p.i++
	var body strings.Builder
	for p.i < len(p.src) && p.src[p.i] != '`' {
		if p.src[p.i] == '\\' && p.i+1 < len(p.src) {
			p.i++
		}
		body.WriteRune(p.src[p.i])
		p.i++
	}
	p.inWord, p.dyn = true, true
	p.out.subs = append(p.out.subs, body.String())
	if p.i >= len(p.src) {
		p.out.uncertain = true
	} else {
		p.i++
	}
	p.cur.WriteString(string(p.src[start:p.i]))
}

func (p *shellParser) dollar() {
	start := p.i
	p.inWord, p.dyn = true, true
	switch n := p.peek(1); {
	case n == '(' && p.peek(2) == '(': // arithmetic
		end := matchParen(p.src, p.i+2)
		if end < 0 {
			p.out.uncertain = true
			end = len(p.src) - 1
		}
		p.i = end + 1
	case n == '(':
		end := matchParen(p.src, p.i+2)
		if end < 0 {
			p.out.uncertain = true
			p.out.subs = append(p.out.subs, string(p.src[p.i+2:]))
			p.i = len(p.src)
		} else {
			p.out.subs = append(p.out.subs, string(p.src[p.i+2:end]))
			p.i = end + 1
		}
	case n == '{':
		for p.i < len(p.src) && p.src[p.i] != '}' {
			p.i++
		}
		if p.i >= len(p.src) {
			p.out.uncertain = true
		} else {
			p.i++
		}
	case n == '\'': // $'...' ANSI-C quoting: escapes may spell anything
		p.i += 2
		for p.i < len(p.src) && p.src[p.i] != '\'' {
			if p.src[p.i] == '\\' {
				p.i++
			}
			p.i++
		}
		if p.i >= len(p.src) {
			p.out.uncertain = true
		} else {
			p.i++
		}
	case unicode.IsLetter(n) || n == '_' || unicode.IsDigit(n):
		p.i++
		for p.i < len(p.src) && (unicode.IsLetter(p.src[p.i]) || unicode.IsDigit(p.src[p.i]) || p.src[p.i] == '_') {
			p.i++
		}
	case n == '@' || n == '*' || n == '#' || n == '?' || n == '$' || n == '!' || n == '-':
		p.i += 2
	default:
		p.dyn = false
		p.i++
	}
	if p.i > len(p.src) {
		p.i = len(p.src)
	}
	p.cur.WriteString(string(p.src[start:p.i]))
}

func (p *shellParser) dquote() {
	p.inWord = true
	p.i++
	for p.i < len(p.src) && p.src[p.i] != '"' {
		switch c := p.src[p.i]; c {
		case '\\':
			if n := p.peek(1); n == '$' || n == '`' || n == '"' || n == '\\' {
				p.cur.WriteRune(n)
				p.i += 2
			} else if n == '\n' {
				p.i += 2
			} else {
				p.cur.WriteRune(c)
				p.i++
			}
		case '$':
			p.dollar()
		case '`':
			p.backtick()
		default:
			p.cur.WriteRune(c)
			p.i++
		}
	}
	if p.i >= len(p.src) {
		p.out.uncertain = true
	} else {
		p.i++
	}
}
