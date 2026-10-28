package sandboxtools

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/GhaziBenDahmane/harness/engine/tools"
	"github.com/GhaziBenDahmane/harness/sandbox"
)

const editDesc = `Replace text in an existing file. oldText is matched exactly first; when it is not found, a match that ignores
trailing spaces and indentation is used, but only if it is unique (the replacement then keeps the file's indentation).
oldText must appear exactly once unless replaceAll is true (replaceAll needs an exact match). Returns JSON {path, replacements, matched}.
Read the file with read_file first and copy oldText from it: an edit is refused when you have not read the file in this
conversation, or when it changed since you last read or wrote it (re-read it then). On a miss the error shows the closest lines.`

// StaleFileError is the edit_file error for a file changed since the model last saw it.
const StaleFileError = "file changed since you last read it; read it again"

type editInput struct {
	Path       string `json:"path" jsonschema_description:"File path, absolute or relative to /workspace"`
	OldText    string `json:"oldText" jsonschema_description:"The text to replace, copied from the file (include enough surrounding lines to make it unique)"`
	NewText    string `json:"newText" jsonschema_description:"The replacement text"`
	ReplaceAll bool   `json:"replaceAll,omitempty" jsonschema_description:"Replace every exact occurrence instead of requiring exactly one"`
}

type editOutput struct {
	Path         string `json:"path"`
	Replacements int    `json:"replacements"`
	// Matched is "exact" or "whitespace" (indentation-tolerant unique match).
	Matched string `json:"matched,omitempty"`
	Notice  string `json:"notice,omitempty"`
}

func (b *binding) edit(ctx context.Context, in editInput) (*editOutput, error) {
	p, err := resolve(in.Path)
	if err != nil {
		return nil, err
	}
	if in.OldText == "" {
		return nil, tools.Errorf("oldText must not be empty")
	}
	var out *editOutput
	if err := b.withSandbox(ctx, func(sb sandbox.Sandbox) error {
		var err error
		out, err = b.replaceIn(ctx, sb, p, in)
		return err
	}); err != nil {
		return nil, err
	}
	out.Path = p
	out.Notice = joinNotes(out.Notice, b.pathNote(ctx, p))
	return out, nil
}

// replaceIn applies an edit to the file at p.
func (b *binding) replaceIn(ctx context.Context, sb sandbox.Sandbox, p string, in editInput) (*editOutput, error) {
	data, size, err := sb.ReadFile(ctx, p, maxReadBytes)
	if err != nil {
		return nil, mapFileErr(err, p)
	}
	if size > int64(len(data)) {
		return nil, tools.Errorf("%s is too large to edit (%d bytes, max %d); use execute_command", p, size, maxReadBytes)
	}
	content := string(data)
	switch state := b.p.seen.check(b.rc.ThreadID, p, data); state {
	case notSeen, stale:
		why := fmt.Sprintf("you have not read %s in this conversation", p)
		if state == stale {
			why = fmt.Sprintf("%s: %s", p, StaleFileError)
		}
		if !utf8.ValidString(content) {
			return nil, tools.Errorf("%s; read it with read_file before editing it", why)
		}
		// Round 3 (A6): the refusal shows the file and counts as a read, so the
		// retried edit works in one step.
		b.p.seen.record(b.rc.ThreadID, p, data)
		return nil, tools.Errorf("not edited: %s. Its current content is below (this counts as a read): copy oldText from it and retry the edit.\n%s", why, editView(content, in.OldText))
	}
	out := &editOutput{Matched: "exact"}
	n := strings.Count(content, in.OldText)
	switch {
	case n > 1 && !in.ReplaceAll:
		return nil, tools.Errorf("oldText appears %d times in %s; include more surrounding lines or set replaceAll=true", n, p)
	case n >= 1 && in.ReplaceAll:
		content = strings.ReplaceAll(content, in.OldText, in.NewText)
		out.Replacements = n
	case n == 1:
		content = strings.Replace(content, in.OldText, in.NewText, 1)
		out.Replacements = 1
	case in.ReplaceAll:
		return nil, tools.Errorf("oldText was not found exactly in %s (replaceAll needs an exact match).%s", p, closest(content, in.OldText))
	default:
		replaced, matches := tolerantReplace(content, in.OldText, in.NewText)
		switch {
		case matches == 0:
			return nil, tools.Errorf("oldText was not found in %s.%s", p, closest(content, in.OldText))
		case matches > 1:
			return nil, tools.Errorf("oldText was not found exactly in %s and matches %d places when whitespace is ignored; copy the exact text from read_file", p, matches)
		}
		content, out.Replacements, out.Matched = replaced, 1, "whitespace"
	}
	if err := writeNoFollow(ctx, sb, p, []byte(content)); err != nil {
		return nil, err
	}
	b.afterWrite(ctx, sb, p, []byte(content))
	if note := b.checkWrite(ctx, sb, p); note != "" {
		out.Notice = note
	}
	b.p.seen.record(b.rc.ThreadID, p, []byte(content))
	return out, nil
}

// editView is the file as an edit refusal shows it: numbered lines like
// read_file when it fits one read_file page, else the regions closest to
// oldText.
func editView(content, old string) string {
	lines, _ := splitLines(strings.TrimSuffix(content, "\n"))
	if len(content) <= maxReadChars && len(lines) <= defaultReadLimit {
		var b strings.Builder
		for i, l := range lines {
			if len(l) > maxLineChars {
				l = truncateUTF8(l, maxLineChars) + " [line truncated]"
			}
			fmt.Fprintf(&b, "%6d\t%s\n", i+1, l)
		}
		return strings.TrimRight(b.String(), "\n")
	}
	return fmt.Sprintf("(%d lines; the regions closest to oldText — read_file shows the rest)%s", len(lines), closest(content, old))
}

// splitLines splits text into lines without their terminator, reporting the
// file's line ending ("\r\n" when its first line uses one).
func splitLines(s string) ([]string, string) {
	eol := "\n"
	if i := strings.IndexByte(s, '\n'); i > 0 && s[i-1] == '\r' {
		eol = "\r\n"
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines, eol
}

func indentOf(s string) string { return s[:len(s)-len(strings.TrimLeft(s, " \t"))] }

// tolerantReplace replaces the line window of content whose lines equal
// old's lines once trimmed (trailing spaces, CR and indentation ignored) when
// there is exactly one such window; matches counts the windows. The new lines
// are re-indented by the indentation difference of the first non-blank line.
func tolerantReplace(content, old, repl string) (string, int) {
	lines, eol := splitLines(content)
	oldLines, _ := splitLines(strings.Trim(old, "\r\n"))
	for len(oldLines) > 0 && strings.TrimSpace(oldLines[len(oldLines)-1]) == "" {
		oldLines = oldLines[:len(oldLines)-1]
	}
	if len(oldLines) == 0 || len(oldLines) > len(lines) {
		return content, 0
	}
	norm := func(s string) string { return strings.TrimSpace(s) }
	var starts []int
	for i := 0; i+len(oldLines) <= len(lines); i++ {
		ok := true
		for j, ol := range oldLines {
			if norm(lines[i+j]) != norm(ol) {
				ok = false
				break
			}
		}
		if ok {
			starts = append(starts, i)
		}
	}
	if len(starts) != 1 {
		return content, len(starts)
	}
	at := starts[0]
	first := 0
	for first < len(oldLines) && strings.TrimSpace(oldLines[first]) == "" {
		first++
	}
	oldIndent, fileIndent := indentOf(oldLines[first]), indentOf(lines[at+first])
	newLines, _ := splitLines(strings.Trim(repl, "\r\n"))
	if repl == "" {
		newLines = nil
	}
	for i, l := range newLines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if strings.HasPrefix(l, oldIndent) {
			newLines[i] = fileIndent + l[len(oldIndent):]
		}
	}
	out := append(append(append([]string{}, lines[:at]...), newLines...), lines[at+len(oldLines):]...)
	return strings.Join(out, eol), 1
}

// closest lists the (at most 3) regions of content most similar to old, with line numbers.
func closest(content, old string) string {
	lines, _ := splitLines(content)
	oldLines, _ := splitLines(strings.Trim(old, "\r\n"))
	probe := ""
	for _, l := range oldLines {
		if strings.TrimSpace(l) != "" {
			probe = strings.TrimSpace(l)
			break
		}
	}
	if probe == "" || len(lines) == 0 {
		return " Read the file and copy the exact text."
	}
	type cand struct {
		line  int
		score float64
	}
	pb := bigrams(probe)
	var cands []cand
	for i, l := range lines {
		if s := dice(pb, bigrams(strings.TrimSpace(l))); s > 0.3 {
			cands = append(cands, cand{i, s})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	span := max(len(oldLines), 1)
	var b strings.Builder
	var used []int
	for _, c := range cands {
		if len(used) == 3 {
			break
		}
		overlap := false
		for _, u := range used {
			if c.line >= u-span && c.line <= u+span {
				overlap = true
			}
		}
		if overlap {
			continue
		}
		used = append(used, c.line)
		from, to := c.line, min(c.line+span, len(lines))
		fmt.Fprintf(&b, "\n--- lines %d-%d ---\n", from+1, to)
		for i := from; i < to; i++ {
			l := lines[i]
			if len(l) > 300 {
				l = truncateUTF8(l, 300) + " [line truncated]"
			}
			fmt.Fprintf(&b, "%6d\t%s\n", i+1, l)
		}
	}
	if len(used) == 0 {
		return " No similar lines found; read the file and copy the exact text."
	}
	return " The closest regions are below; copy the exact text from them (or read the file again):" + strings.TrimRight(b.String(), "\n")
}

func bigrams(s string) map[string]int {
	r := []rune(strings.ToLower(s))
	out := map[string]int{}
	for i := 0; i+1 < len(r); i++ {
		out[string(r[i:i+2])]++
	}
	return out
}

// dice is the Sørensen–Dice coefficient of two bigram multisets.
func dice(a, b map[string]int) float64 {
	na, nb, common := 0, 0, 0
	for k, v := range a {
		na += v
		common += min(v, b[k])
	}
	for _, v := range b {
		nb += v
	}
	if na+nb == 0 {
		return 0
	}
	return 2 * float64(common) / float64(na+nb)
}

// ---- read-before-edit ----

const (
	maxObservedThreads = 2000
	maxObservedFiles   = 500
)

type seenState int

const (
	current seenState = iota
	notSeen
	stale
)

// observations hold, per thread, the sha256 of each file's content as the
// model last read or wrote it. They live in the runtime process: a run
// resumed on another replica re-reads before its first edit.
type observations struct {
	mu      sync.Mutex
	threads map[string]map[string][32]byte
	order   []string
}

func newObservations() *observations { return &observations{threads: map[string]map[string][32]byte{}} }

func (o *observations) record(threadID, p string, data []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	files := o.threads[threadID]
	if files == nil {
		if len(o.order) >= maxObservedThreads {
			delete(o.threads, o.order[0])
			o.order = o.order[1:]
		}
		files = map[string][32]byte{}
		o.threads[threadID] = files
		o.order = append(o.order, threadID)
	}
	if _, ok := files[p]; !ok && len(files) >= maxObservedFiles {
		for k := range files { // drop an arbitrary one: it only costs a re-read
			delete(files, k)
			break
		}
	}
	files[p] = sha256.Sum256(data)
}

func (o *observations) check(threadID, p string, data []byte) seenState {
	o.mu.Lock()
	defer o.mu.Unlock()
	sum, ok := o.threads[threadID][p]
	switch {
	case !ok:
		return notSeen
	case sum != sha256.Sum256(data):
		return stale
	}
	return current
}
