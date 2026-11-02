package permissions

import (
	"path"
	"strings"
)

// Redirect is a file redirection of a command (> >> >|, &>); input redirections
// and descriptor duplications (2>&1) are not kept.
type Redirect struct {
	Op     string
	Target string
}

// Simple is one simple command of a shell line, as far as it can be judged
// without running the shell.
type Simple struct {
	// Words are the program and its arguments, after env prefixes and wrappers
	// (sudo, env, nohup, time, timeout, xargs, bash -c …) are removed.
	Words []string
	// Raw is the original text of the words (before wrappers were removed).
	Raw string
	// Env are the leading NAME=value assignments.
	Env []string
	// Elevated: run through sudo/doas.
	Elevated bool
	// Dynamic: some arguments come from a pipe (xargs), so their values are unknown.
	Dynamic bool
	// Dyn marks words (by index in Words) that contain a variable or command
	// substitution, whose value is unknown.
	Dyn map[int]bool
	// Redirects are the output redirections.
	Redirects []Redirect
	// Pipeline identifies the pipeline of the line this command is part of.
	Pipeline int
}

// Prog is the program name without its directory.
func (s Simple) Prog() string {
	if len(s.Words) == 0 {
		return ""
	}
	return path.Base(s.Words[0])
}

// Text is the command as one string (the unit user rules are matched against).
func (s Simple) Text() string { return strings.Join(s.Words, " ") }

// Parsed is the result of splitting a shell line.
type Parsed struct {
	// Commands are all simple commands, including those inside $(...), backticks,
	// subshells, bash -c strings and pipelines.
	Commands []Simple
	// Pipelines are the texts of the multi-command pipelines ("curl x | sh"),
	// by Pipeline index of their commands (single commands have none).
	Pipelines map[int]string
	// Incomplete: the line could not be parsed with confidence (unbalanced quote,
	// too deep nesting). Callers must treat it as unknown.
	Incomplete bool
}

const maxDepth = 8

type parser struct {
	out      *Parsed
	nextPipe int
}

// Parse splits a shell command line into the simple commands it would run.
// It is a conservative approximation of bash, enough to judge each command: it
// understands quotes, escapes, ; && || | & newlines, subshells and groups,
// $(...) and backticks (also inside double quotes), heredocs, redirections,
// env prefixes and the wrappers sudo, env, nohup, time, nice, timeout, xargs,
// command, exec, eval and `sh -c '…'`.
func Parse(line string) Parsed {
	p := &parser{out: &Parsed{Pipelines: map[int]string{}}}
	p.parse(line, 0)
	return *p.out
}

type word struct {
	text string
	dyn  bool
}

type cmdState struct {
	words     []word
	redirects []Redirect
}

func (p *parser) parse(src string, depth int) {
	if depth > maxDepth {
		p.out.Incomplete = true
		return
	}
	rs := []rune(src)
	var (
		cur      cmdState
		pipeline []Simple
		pipeRaw  []string
		w        strings.Builder
		wDyn     bool
		inWord   bool
		quoted   bool // current word had quotes (so an empty word still counts)
		pendRed  string
		ampRed   bool // pendRed came from ">&": its target is a descriptor (>&2, >&-) or, otherwise, a file
		pendHere []heredoc
	)
	flushWord := func() {
		if !inWord {
			return
		}
		t := w.String()
		w.Reset()
		d := wDyn
		inWord, wDyn, quoted = false, false, false
		if pendRed != "" {
			op := pendRed
			pendRed = ""
			dup := ampRed && (isDigits(strings.TrimSuffix(t, "-")) || t == "-")
			ampRed = false
			if !dup && op != "<" && op != "<<<" && t != "" {
				cur.redirects = append(cur.redirects, Redirect{Op: op, Target: t})
			}
			return
		}
		cur.words = append(cur.words, word{t, d})
	}
	endCommand := func() {
		flushWord()
		if len(cur.words) > 0 || len(cur.redirects) > 0 {
			if s, ok := p.build(cur, depth); ok {
				pipeline = append(pipeline, s...)
				pipeRaw = append(pipeRaw, rawOf(cur))
			}
		}
		cur = cmdState{}
	}
	endPipeline := func() {
		endCommand()
		if len(pipeline) > 0 {
			id := p.nextPipe
			p.nextPipe++
			for i := range pipeline {
				pipeline[i].Pipeline = id
			}
			if len(pipeRaw) > 1 {
				p.out.Pipelines[id] = strings.Join(pipeRaw, " | ")
			}
			p.out.Commands = append(p.out.Commands, pipeline...)
		}
		pipeline, pipeRaw = nil, nil
	}
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == '\\':
			if i+1 < len(rs) {
				i++
				if rs[i] != '\n' {
					w.WriteRune(rs[i])
					inWord = true
				}
			}
		case c == '\'':
			j := i + 1
			for j < len(rs) && rs[j] != '\'' {
				j++
			}
			if j >= len(rs) {
				p.out.Incomplete = true
			}
			w.WriteString(string(rs[i+1 : min(j, len(rs))]))
			inWord, quoted = true, true
			i = j
		case c == '"':
			j := i + 1
			for j < len(rs) && rs[j] != '"' {
				switch {
				case rs[j] == '\\' && j+1 < len(rs):
					if n := rs[j+1]; n == '"' || n == '\\' || n == '$' || n == '`' {
						w.WriteRune(n)
					} else if n != '\n' {
						w.WriteRune('\\')
						w.WriteRune(n)
					}
					j += 2
				case rs[j] == '$' && j+1 < len(rs) && rs[j+1] == '(':
					end := matchParen(rs, j+1)
					p.subst(rs, j+2, end, depth, &wDyn)
					w.WriteString("$(…)")
					j = end + 1
				case rs[j] == '`':
					end := indexRune(rs, j+1, '`')
					p.subst(rs, j+1, end, depth, &wDyn)
					w.WriteString("`…`")
					j = end + 1
				case rs[j] == '$' && j+1 < len(rs) && isVarStart(rs[j+1]):
					wDyn = true
					w.WriteRune(rs[j])
					j++
				default:
					w.WriteRune(rs[j])
					j++
				}
			}
			if j >= len(rs) {
				p.out.Incomplete = true
			}
			inWord, quoted = true, true
			i = j
		case c == '$' && i+2 < len(rs) && rs[i+1] == '(' && rs[i+2] == '(':
			end := matchParen(rs, i+1) // $(( arithmetic ))
			w.WriteString("$((…))")
			inWord, wDyn = true, true
			i = end
		case c == '$' && i+1 < len(rs) && rs[i+1] == '(':
			end := matchParen(rs, i+1)
			p.subst(rs, i+2, end, depth, &wDyn)
			w.WriteString("$(…)")
			inWord = true
			i = end
		case c == '`':
			end := indexRune(rs, i+1, '`')
			p.subst(rs, i+1, end, depth, &wDyn)
			w.WriteString("`…`")
			inWord = true
			i = end
		case (c == '<' || c == '>') && i+1 < len(rs) && rs[i+1] == '(':
			end := matchParen(rs, i+1) // process substitution
			p.subst(rs, i+2, end, depth, &wDyn)
			w.WriteString("<(…)")
			inWord = true
			i = end
		case c == '$':
			w.WriteRune(c)
			inWord = true
			if i+1 < len(rs) && isVarStart(rs[i+1]) {
				wDyn = true
			}
		case c == '#' && !inWord:
			for i < len(rs) && rs[i] != '\n' {
				i++
			}
			i--
		case c == ' ' || c == '\t' || c == '\r':
			flushWord()
		case c == '\n':
			endPipeline()
			for _, h := range pendHere {
				i = p.heredocBody(rs, i+1, h, depth) - 1
			}
			pendHere = nil
		case c == ';':
			endPipeline()
			if i+1 < len(rs) && rs[i+1] == ';' {
				i++
			}
		case c == '&' && i+1 < len(rs) && rs[i+1] == '&':
			endPipeline()
			i++
		case c == '&' && i+1 < len(rs) && rs[i+1] == '>': // &> file, &>> file
			flushWord()
			op := "&>"
			i++
			if i+1 < len(rs) && rs[i+1] == '>' {
				op, i = "&>>", i+1
			}
			pendRed, ampRed = op, false
		case c == '&':
			endPipeline()
		case c == '|' && i+1 < len(rs) && rs[i+1] == '|':
			endPipeline()
			i++
		case c == '|':
			endCommand()
			if i+1 < len(rs) && rs[i+1] == '&' {
				i++
			}
		case c == '(' || c == ')':
			endPipeline()
		case c == '<' || c == '>':
			// A word made only of digits right before the operator is a descriptor.
			if inWord && !quoted && isDigits(w.String()) {
				w.Reset()
				inWord = false
			} else {
				flushWord()
			}
			op := string(c)
			heredocOp := false
			if i+1 < len(rs) && rs[i+1] == c {
				op += string(c)
				i++
				if c == '<' && i+1 < len(rs) && rs[i+1] == '<' {
					op = "<<<"
					i++
				} else if c == '<' {
					heredocOp = true
				}
			} else if c == '>' && i+1 < len(rs) && rs[i+1] == '|' {
				i++
			}
			if heredocOp {
				// The delimiter word follows; the body starts on the next line.
				j := i + 1
				dash := false
				if j < len(rs) && rs[j] == '-' {
					dash = true
					j++
				}
				for j < len(rs) && (rs[j] == ' ' || rs[j] == '\t') {
					j++
				}
				delim, quotedDelim, end := readDelim(rs, j)
				pendHere = append(pendHere, heredoc{delim: delim, quoted: quotedDelim, dash: dash})
				i = end - 1
				break
			}
			if c == '>' && i+1 < len(rs) && rs[i+1] == '&' {
				i++
				pendRed, ampRed = op, true
				break
			}
			pendRed, ampRed = op, false
		default:
			w.WriteRune(c)
			inWord = true
		}
	}
	endPipeline()
}

type heredoc struct {
	delim  string
	quoted bool
	dash   bool
}

func readDelim(rs []rune, i int) (delim string, quoted bool, end int) {
	var b strings.Builder
	for i < len(rs) {
		c := rs[i]
		switch {
		case c == '\'' || c == '"':
			quoted = true
			j := i + 1
			for j < len(rs) && rs[j] != c {
				b.WriteRune(rs[j])
				j++
			}
			i = j + 1
		case c == '\\':
			quoted = true
			if i+1 < len(rs) {
				b.WriteRune(rs[i+1])
			}
			i += 2
		case c == ' ' || c == '\t' || c == '\n' || c == ';' || c == '&' || c == '|' || c == ')' || c == '<' || c == '>':
			return b.String(), quoted, i
		default:
			b.WriteRune(c)
			i++
		}
	}
	return b.String(), quoted, min(i, len(rs))
}

// heredocBody skips the body of a heredoc starting at rs[i] and returns the
// index after its closing line. The body of an unquoted heredoc can hold
// $(...) and backticks, which run.
func (p *parser) heredocBody(rs []rune, i int, h heredoc, depth int) int {
	start := i
	for i < len(rs) {
		j := i
		for j < len(rs) && rs[j] != '\n' {
			j++
		}
		line := string(rs[i:j])
		if h.dash {
			line = strings.TrimLeft(line, "\t")
		}
		if line == h.delim {
			if !h.quoted {
				p.bodySubst(string(rs[start:i]), depth)
			}
			return min(j+1, len(rs))
		}
		i = j + 1
	}
	if !h.quoted && start < len(rs) {
		p.bodySubst(string(rs[start:]), depth)
	}
	return len(rs)
}

// bodySubst runs the substitutions of a heredoc body through the parser.
func (p *parser) bodySubst(body string, depth int) {
	rs := []rune(body)
	for i := 0; i < len(rs); i++ {
		switch {
		case rs[i] == '\\':
			i++
		case rs[i] == '$' && i+1 < len(rs) && rs[i+1] == '(':
			end := matchParen(rs, i+1)
			p.parse(string(rs[i+2:min(end, len(rs))]), depth+1)
			i = end
		case rs[i] == '`':
			end := indexRune(rs, i+1, '`')
			p.parse(string(rs[i+1:min(end, len(rs))]), depth+1)
			i = end
		}
	}
}

// subst parses the inside of a $(...) or backtick substitution as commands of their own.
func (p *parser) subst(rs []rune, from, to, depth int, dyn *bool) {
	*dyn = true
	if to >= len(rs) {
		to = len(rs)
		p.out.Incomplete = true
	}
	if from > to {
		return
	}
	p.parse(string(rs[from:to]), depth+1)
}

// matchParen returns the index of the ")" matching the "(" at rs[open]
// (len(rs) when unbalanced).
func matchParen(rs []rune, open int) int {
	depth := 0
	for i := open; i < len(rs); i++ {
		switch rs[i] {
		case '\\':
			i++
		case '\'':
			j := i + 1
			for j < len(rs) && rs[j] != '\'' {
				j++
			}
			i = j
		case '"':
			j := i + 1
			for j < len(rs) && rs[j] != '"' {
				if rs[j] == '\\' {
					j++
				}
				j++
			}
			i = j
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return len(rs)
}

func indexRune(rs []rune, from int, r rune) int {
	for i := from; i < len(rs); i++ {
		if rs[i] == '\\' {
			i++
			continue
		}
		if rs[i] == r {
			return i
		}
	}
	return len(rs)
}

func isVarStart(r rune) bool {
	return r == '_' || r == '{' || r == '@' || r == '*' || r == '#' || r == '?' || r == '!' || r == '$' ||
		(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func rawOf(c cmdState) string {
	parts := make([]string, 0, len(c.words))
	for _, w := range c.words {
		parts = append(parts, w.text)
	}
	return strings.Join(parts, " ")
}

func isAssign(s string) bool {
	i := strings.IndexByte(s, '=')
	if i <= 0 {
		return false
	}
	for j, r := range s[:i] {
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (j > 0 && r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// compoundHeaders open a construct whose own line holds no command.
var compoundHeaders = map[string]bool{"for": true, "select": true, "case": true, "function": true, "fi": true, "done": true, "esac": true}

// skipWords are shell keywords ignored in front of a command.
var skipWords = map[string]bool{"if": true, "then": true, "else": true, "elif": true, "do": true, "while": true, "until": true, "!": true, "{": true, "}": true}

// build turns the words of a command into simple commands (several when a
// wrapper such as bash -c holds a script).
func (p *parser) build(c cmdState, depth int) ([]Simple, bool) {
	words := c.words
	for len(words) > 0 && skipWords[words[0].text] {
		words = words[1:]
	}
	if len(words) > 0 && compoundHeaders[words[0].text] {
		words = nil
	}
	var env []string
	for len(words) > 0 && isAssign(words[0].text) {
		env = append(env, words[0].text)
		words = words[1:]
	}
	if len(words) == 0 {
		if len(c.redirects) == 0 {
			return nil, false
		}
		return []Simple{{Redirects: c.redirects, Env: env}}, true // `> file` alone truncates it
	}
	s := Simple{Env: env, Redirects: c.redirects, Raw: rawOf(cmdState{words: words}), Dyn: map[int]bool{}}
	var extra []Simple
	for {
		next, done := p.unwrap(&s, words, &extra, depth)
		if done {
			if next == nil {
				words = nil // a nested script stands for the wrapper
			}
			break
		}
		words = next
	}
	for i := range extra {
		extra[i].Elevated = extra[i].Elevated || s.Elevated
	}
	for i, w := range words {
		s.Words = append(s.Words, w.text)
		if w.dyn {
			s.Dyn[i] = true
		}
	}
	if len(s.Words) == 0 {
		// Only a nested script (bash -c) was left: its commands stand for the line;
		// redirections of the wrapper apply to them.
		if len(extra) > 0 && len(s.Redirects) > 0 {
			extra = append(extra, Simple{Redirects: s.Redirects})
		}
		return extra, len(extra) > 0
	}
	return append([]Simple{s}, extra...), true
}

// sudoArgOpts are the sudo options that take a value.
var sudoArgOpts = map[string]bool{"-u": true, "-g": true, "-C": true, "-h": true, "-p": true, "-r": true, "-t": true, "-U": true, "-D": true, "-R": true, "-T": true}

// unwrap removes one wrapper from the front of words; done when none is left
// (words was a plain command, or a nested script was handed to extra).
func (p *parser) unwrap(s *Simple, words []word, extra *[]Simple, depth int) (next []word, done bool) {
	if len(words) == 0 {
		return nil, true
	}
	prog := path.Base(words[0].text)
	rest := words[1:]
	skipOpts := func(withArg map[string]bool) []word {
		for len(rest) > 0 {
			t := rest[0].text
			switch {
			case t == "--":
				return rest[1:]
			case withArg[t]:
				if len(rest) < 2 {
					return nil
				}
				rest = rest[2:]
			case strings.HasPrefix(t, "-") && len(t) > 1:
				rest = rest[1:]
			default:
				return rest
			}
		}
		return rest
	}
	nested := func(script string) {
		n := len(p.out.Commands)
		p.parse(script, depth+1)
		*extra = append(*extra, p.out.Commands[n:]...)
		p.out.Commands = p.out.Commands[:n]
	}
	switch prog {
	case "sudo", "doas":
		s.Elevated = true
		r := skipOpts(sudoArgOpts)
		for len(r) > 0 && isAssign(r[0].text) { // sudo VAR=value cmd
			s.Env = append(s.Env, r[0].text)
			r = r[1:]
		}
		return r, false
	case "env":
		for len(rest) > 0 {
			t := rest[0].text
			switch {
			case t == "-u" || t == "-C" || t == "-S":
				if len(rest) < 2 {
					return nil, false
				}
				rest = rest[2:]
			case strings.HasPrefix(t, "-") && len(t) > 1:
				rest = rest[1:]
			case isAssign(t):
				s.Env = append(s.Env, t)
				rest = rest[1:]
			default:
				return rest, false
			}
		}
		return rest, false
	case "command", "builtin", "nohup", "exec", "time", "setsid", "stdbuf", "ionice", "caffeinate":
		if prog == "command" && len(rest) > 0 && (rest[0].text == "-v" || rest[0].text == "-V") {
			return words, true // lookup only
		}
		return skipOpts(map[string]bool{"-n": true, "-c": true, "-i": true, "-o": true, "-e": true}), false
	case "nice":
		return skipOpts(map[string]bool{"-n": true}), false
	case "timeout":
		r := skipOpts(map[string]bool{"-k": true, "-s": true})
		if len(r) > 0 { // the duration
			r = r[1:]
		}
		return r, false
	case "xargs":
		s.Dynamic = true
		return skipOpts(map[string]bool{"-I": true, "-n": true, "-P": true, "-L": true, "-d": true, "-s": true, "-E": true, "-a": true}), false
	case "bash", "sh", "zsh", "dash", "ksh", "ash":
		for i := 0; i < len(rest); i++ {
			t := rest[i].text
			if strings.HasPrefix(t, "-") && !strings.HasPrefix(t, "--") && strings.Contains(t, "c") && len(t) <= 4 {
				if i+1 < len(rest) {
					nested(rest[i+1].text)
					return nil, true
				}
				break
			}
			if !strings.HasPrefix(t, "-") {
				break
			}
		}
	case "eval":
		var parts []string
		for _, w := range rest {
			parts = append(parts, w.text)
		}
		nested(strings.Join(parts, " "))
		return nil, true
	}
	return words, true
}
