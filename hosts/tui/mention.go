package tui

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
)

// Limits of @file mentions and of the file index.
const (
	maxMentionFile  = 64 << 10  // bytes attached per file
	maxMentionTotal = 256 << 10 // bytes attached per message
	maxIndexFiles   = 20000
	mentionRows     = 6
)

// filesMsg carries the file list of the workspace.
type filesMsg struct{ files []string }

// fileIndex is the list of workspace files @ completes from, loaded on first use.
type fileIndex struct {
	list    []string
	loading bool
	loaded  bool
}

func (f *fileIndex) done(m filesMsg) { f.list, f.loading, f.loaded = m.files, false, true }

// mentionState is the open @file menu.
type mentionState struct {
	sel       int
	dismissed string // input value at which esc closed the menu
}

// listFiles lists the files under root, relative with slashes: git's view
// (tracked and untracked files minus .gitignore) in a repository, else a
// bounded walk that skips .git, node_modules and vendor.
func listFiles(ctx context.Context, root string) []string {
	if _, err := os.Stat(filepath.Join(root, ".git")); err == nil {
		if _, err := exec.LookPath("git"); err == nil {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, "git", "-C", root, "ls-files", "-co", "--exclude-standard", "-z").Output()
			if err == nil {
				var files []string
				for _, f := range bytes.Split(out, []byte{0}) {
					if len(f) > 0 && len(files) < maxIndexFiles {
						files = append(files, string(f))
					}
				}
				sort.Strings(files)
				return files
			}
		}
	}
	var files []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if len(files) >= maxIndexFiles {
			return filepath.SkipAll
		}
		if rel, err := filepath.Rel(root, p); err == nil {
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	return files
}

// indexFiles starts loading the file list when the user typed an "@" token.
func (m *Model) indexFiles() tea.Cmd {
	f := &m.inp.files
	if m.inp.workspace == "" || f.loaded || f.loading {
		return nil
	}
	if _, _, ok := atToken(m.in.Value()); !ok {
		return nil
	}
	f.loading = true
	root, ctx := m.inp.workspace, m.ctx
	return func() tea.Msg { return filesMsg{listFiles(ctx, root)} }
}

// atToken finds the "@query" the input ends with: the byte offset of the "@"
// and the query. The "@" must start the input or follow whitespace.
func atToken(v string) (start int, query string, ok bool) {
	i := strings.LastIndexAny(v, " \t\n") + 1
	tok := v[i:]
	if !strings.HasPrefix(tok, "@") {
		return 0, "", false
	}
	return i, tok[1:], true
}

// fuzzyFiles ranks files for a query: every query rune must occur in order;
// matches in the base name, consecutive runs and short paths rank first.
func fuzzyFiles(files []string, q string, n int) []string {
	q = strings.ToLower(q)
	type hit struct {
		path  string
		score int
	}
	var hits []hit
	for _, f := range files {
		lf := strings.ToLower(f)
		sc, ok := fuzzyScore(lf, q)
		if !ok {
			continue
		}
		if q != "" && strings.Contains(strings.ToLower(filepath.Base(f)), q) {
			sc += 200
		}
		hits = append(hits, hit{f, sc - len(f)})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	out := make([]string, 0, min(n, len(hits)))
	for i := 0; i < len(hits) && i < n; i++ {
		out = append(out, hits[i].path)
	}
	return out
}

func fuzzyScore(s, q string) (int, bool) {
	score, run, pos := 0, 0, 0
	for _, r := range q {
		i := strings.IndexRune(s[pos:], r)
		if i < 0 {
			return 0, false
		}
		if i == 0 {
			run++
			score += 10 * run
		} else {
			run = 0
			score++
		}
		pos += i + utf8.RuneLen(r)
	}
	return score, true
}

// mentionMatches are the files the open menu offers; nil when there is no menu.
func (m *Model) mentionMatches() []string {
	if m.inp.workspace == "" || m.picker != nil || m.pending != nil || !m.inp.files.loaded {
		return nil
	}
	v := m.in.Value()
	if v == m.inp.mention.dismissed {
		return nil
	}
	_, q, ok := atToken(v)
	if !ok {
		return nil
	}
	ms := fuzzyFiles(m.inp.files.list, q, mentionRows)
	if len(ms) > 0 && ms[0] == q {
		return nil // the path is complete: enter sends
	}
	return ms
}

// mentionKey drives the menu: up/down move, tab and enter insert the path, esc closes.
func (m *Model) mentionKey(s string) (tea.Cmd, bool) {
	ms := m.mentionMatches()
	if len(ms) == 0 {
		return nil, false
	}
	st := &m.inp.mention
	st.sel = min(st.sel, len(ms)-1)
	switch s {
	case "up":
		st.sel = (st.sel + len(ms) - 1) % len(ms)
	case "down":
		st.sel = (st.sel + 1) % len(ms)
	case "tab", "enter":
		v := m.in.Value()
		start, _, _ := atToken(v)
		m.in.SetValue(v[:start] + "@" + ms[st.sel] + " ")
		m.in.CursorEnd()
		st.sel = 0
	case "esc":
		st.dismissed = m.in.Value()
	default:
		return nil, false
	}
	return nil, true
}

func (m *Model) mentionView(w int) string {
	if m.inp.workspace != "" && m.inp.files.loading && m.picker == nil {
		if _, _, ok := atToken(m.in.Value()); ok {
			return styleDim.Render("  indexing files…")
		}
	}
	ms := m.mentionMatches()
	if len(ms) == 0 {
		return ""
	}
	sel := min(m.inp.mention.sel, len(ms)-1)
	lines := make([]string, len(ms))
	for i, f := range ms {
		line := clip("  @"+f, w)
		if i == sel {
			lines[i] = styleSel.Render(line)
		} else {
			lines[i] = styleDim.Render(line)
		}
	}
	return strings.Join(lines, "\n")
}

var reMention = regexp.MustCompile(`(?:^|\s)@(\S+)`)

// resolveInWorkspace maps a mentioned path to a regular file inside root. It
// rejects "..", absolute paths outside root and symlinks that lead out.
func resolveInWorkspace(root, p string) (string, error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, p)
	}
	abs = filepath.Clean(abs)
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	if rel, err := filepath.Rel(realRoot, real); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside the workspace", p)
	}
	st, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", p)
	}
	return real, nil
}

// mentionedFile is one @file of a message, read for the model.
type mentionedFile struct {
	path    string
	content string
	note    string // truncation or refusal
}

// readMentions finds the @paths of text that name files of the workspace and
// reads them (capped); a token that is not a file is left alone, one that
// escapes the workspace is reported in a note.
func readMentions(root, text string) []mentionedFile {
	var out []mentionedFile
	seen := map[string]bool{}
	total := 0
	for _, sm := range reMention.FindAllStringSubmatch(text, -1) {
		p := strings.TrimRight(sm[1], ".,;:!?)")
		if seen[p] {
			continue
		}
		seen[p] = true
		abs, err := resolveInWorkspace(root, p)
		if err != nil {
			if strings.Contains(err.Error(), "outside the workspace") {
				out = append(out, mentionedFile{path: p, note: "not attached: " + err.Error()})
			}
			continue
		}
		f := mentionedFile{path: p}
		data, err := readCapped(abs, min(maxMentionFile, max(maxMentionTotal-total, 0)))
		switch {
		case err != nil:
			f.note = "not attached: " + err.Error()
		case bytes.IndexByte(data.b, 0) >= 0:
			f.note = "not attached: binary file"
		default:
			f.content = string(data.b)
			total += len(data.b)
			if data.truncated {
				f.note = fmt.Sprintf("truncated to the first %d bytes of %d", len(data.b), data.size)
			}
		}
		out = append(out, f)
	}
	return out
}

type cappedRead struct {
	b         []byte
	size      int64
	truncated bool
}

func readCapped(path string, limit int) (cappedRead, error) {
	f, err := os.Open(path)
	if err != nil {
		return cappedRead{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return cappedRead{}, err
	}
	buf := make([]byte, limit)
	n, _ := f.Read(buf)
	buf = buf[:n]
	if limit > 0 && n == limit {
		// cut at a rune boundary
		for len(buf) > 0 && !utf8.Valid(buf) {
			buf = buf[:len(buf)-1]
		}
	}
	return cappedRead{b: buf, size: st.Size(), truncated: st.Size() > int64(len(buf))}, nil
}

// mentionBlocks renders the files as the fenced blocks appended to the
// message, and the notice lines shown in the transcript.
func mentionBlocks(fs []mentionedFile) (payload string, notices []string) {
	var b strings.Builder
	for _, f := range fs {
		if f.note != "" && f.content == "" {
			notices = append(notices, fmt.Sprintf("@%s %s", f.path, f.note))
			continue
		}
		fmt.Fprintf(&b, "\n\nCalled Read on %s:\n```\n%s\n```", f.path, strings.TrimRight(f.content, "\n"))
		n := "Called Read on " + f.path
		if f.note != "" {
			fmt.Fprintf(&b, "\n[%s]", f.note)
			n += " (" + f.note + ")"
		}
		notices = append(notices, n)
	}
	return b.String(), notices
}
