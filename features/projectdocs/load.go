package projectdocs

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/GhaziBenDahmane/harness/core/sandbox"
)

// loaded is one instruction file, imports included.
type loaded struct {
	// label is the path shown to the model, relative to the workspace (or "~/.har/AGENTS.md").
	label   string
	text    string
	trusted bool
}

// source reads files and decides which paths @imports may reach.
type source interface {
	// read returns at most max bytes of the file and its size; ok is false when it cannot be used.
	read(ctx context.Context, p string, max int64) (data []byte, size int64, ok bool)
	// within reports whether p (cleaned) is inside the area imports may reach.
	within(p string) bool
	join(dir, rel string) string
	dir(p string) string
	rel(p string) string
}

// loader reads the instruction files of one sandbox (and of the user's host directory).
type loader struct {
	ws      source
	user    source
	userDir string
}

func newLoader(sb sandbox.Sandbox, userDir string) *loader {
	l := &loader{ws: &sandboxSource{sb: sb, real: map[string]bool{}}, userDir: userDir}
	if userDir != "" {
		l.user = hostSource{root: userDir}
	}
	return l
}

// ---- the sandbox (the repository) ----

type sandboxSource struct {
	sb   sandbox.Sandbox
	real map[string]bool // paths whose real location was checked -> inside the workspace
}

const wsPrefix = sandbox.Workspace + "/"

func (s *sandboxSource) within(p string) bool { return strings.HasPrefix(p, wsPrefix) }
func (s *sandboxSource) join(dir, rel string) string {
	if strings.HasPrefix(rel, "/") {
		return path.Clean(rel)
	}
	return path.Join(dir, rel)
}
func (s *sandboxSource) dir(p string) string { return path.Dir(p) }
func (s *sandboxSource) rel(p string) string { return strings.TrimPrefix(p, wsPrefix) }

// read reads a regular file whose real path (symbolic links resolved) is in
// the workspace: a link in the repository must not pull in a file of the host.
func (s *sandboxSource) read(ctx context.Context, p string, max int64) ([]byte, int64, bool) {
	if !s.within(p) {
		return nil, 0, false
	}
	data, size, err := s.sb.ReadFile(ctx, p, max)
	if err != nil || len(data) == 0 {
		if err != nil && !errors.Is(err, sandbox.ErrNotFound) {
			slog.Debug("projectdocs: cannot read", "path", p, "error", err)
		}
		return nil, 0, false
	}
	ok, seen := s.real[p]
	if !seen {
		real, err := sandbox.RealPath(ctx, s.sb, p)
		ok = err == nil && s.within(real)
		if err != nil {
			slog.Warn("projectdocs: cannot resolve the real path, file ignored", "path", p, "error", err)
		} else if !ok {
			slog.Warn("projectdocs: file resolves outside the workspace, ignored", "path", p, "real", real)
		}
		s.real[p] = ok
	}
	if !ok {
		return nil, 0, false
	}
	return data, size, true
}

// ---- the host (the user's ~/.har) ----

type hostSource struct{ root string }

func (h hostSource) within(p string) bool {
	return p == h.root || strings.HasPrefix(p, h.root+string(filepath.Separator))
}
func (h hostSource) join(dir, rel string) string {
	if filepath.IsAbs(rel) {
		return filepath.Clean(rel)
	}
	return filepath.Join(dir, rel)
}
func (h hostSource) dir(p string) string { return filepath.Dir(p) }
func (h hostSource) rel(p string) string {
	if r, err := filepath.Rel(h.root, p); err == nil {
		return r
	}
	return p
}

func (h hostSource) read(_ context.Context, p string, max int64) ([]byte, int64, bool) {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return nil, 0, false
	}
	rootReal, err := filepath.EvalSymlinks(h.root)
	if err != nil || !(real == rootReal || strings.HasPrefix(real, rootReal+string(filepath.Separator))) {
		return nil, 0, false
	}
	f, err := os.Open(real)
	if err != nil {
		return nil, 0, false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, 0, false
	}
	buf := make([]byte, max)
	n, _ := f.Read(buf)
	if n == 0 {
		return nil, 0, false
	}
	return buf[:n], st.Size(), true
}

// ---- files ----

// text returns the file as text with its imports, cut at MaxBytes with a marker.
func (l *loader) text(ctx context.Context, src source, p, label string, limit int64) (string, bool) {
	data, size, ok := src.read(ctx, p, limit)
	if !ok {
		return "", false
	}
	body := strings.ToValidUTF8(string(data), "")
	if size > int64(len(data)) {
		body = strings.TrimRight(body, "\n") + "\n[…cut: read_file " + readPath(src, p) + " for the rest]"
	}
	budget := MaxTotalBytes
	body = expandImports(ctx, src, p, body, 0, map[string]bool{p: true}, &budget)
	return body, true
}

func readPath(src source, p string) string {
	if _, ok := src.(*sandboxSource); ok {
		return p
	}
	return p
}

func (l *loader) projectFile(ctx context.Context, abs, label string) (loaded, bool) {
	text, ok := l.text(ctx, l.ws, abs, label, MaxBytes)
	if !ok || strings.TrimSpace(text) == "" {
		return loaded{}, false
	}
	return loaded{label: label, text: text}, true
}

// rootFile is the first of Files that exists at the root of the workspace.
func (l *loader) rootFile(ctx context.Context) (loaded, string) {
	for _, name := range Files {
		if doc, ok := l.projectFile(ctx, wsPrefix+name, name); ok {
			return doc, name
		}
	}
	return loaded{}, ""
}

// userFile is the user's own AGENTS.md (trusted).
func (l *loader) userFile(ctx context.Context) (loaded, bool) {
	if l.user == nil {
		return loaded{}, false
	}
	p := filepath.Join(l.userDir, "AGENTS.md")
	text, ok := l.text(ctx, l.user, p, "", MaxBytes)
	if !ok || strings.TrimSpace(text) == "" {
		return loaded{}, false
	}
	return loaded{label: "~/" + filepath.Base(l.userDir) + "/AGENTS.md", text: text, trusted: true}, true
}

// companionFiles are the files of other tools read when the repository has no
// AGENTS.md: GitHub Copilot's instructions, .cursorrules and the Cursor
// rules that always apply.
func (l *loader) companionFiles(ctx context.Context, rules []cursorRule) []loaded {
	var out []loaded
	if doc, ok := l.projectFile(ctx, wsPrefix+".github/copilot-instructions.md", ".github/copilot-instructions.md"); ok {
		out = append(out, doc)
	}
	if doc, ok := l.projectFile(ctx, wsPrefix+".cursorrules", ".cursorrules"); ok {
		out = append(out, doc)
	}
	for _, r := range rules {
		if r.always && strings.TrimSpace(r.body) != "" {
			out = append(out, loaded{label: r.rel, text: r.body})
		}
	}
	return out
}

// ---- @imports ----

var (
	importRef  = regexp.MustCompile("(?:^|[\\s(\\[])@([A-Za-z0-9_./~][A-Za-z0-9_./+~-]*)")
	codeSpan   = regexp.MustCompile("`[^`\n]*`")
	fenceStart = regexp.MustCompile("^\\s*(```|~~~)")
)

// importsOf lists the @path references of text outside code blocks and code
// spans. A reference needs a "/" or a "." (so @alice is a mention, not a file).
func importsOf(text string) []string {
	var out []string
	seen := map[string]bool{}
	inFence := false
	for _, line := range strings.Split(text, "\n") {
		if fenceStart.MatchString(line) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		line = codeSpan.ReplaceAllString(line, "")
		for _, m := range importRef.FindAllStringSubmatch(line, -1) {
			ref := strings.TrimRight(m[1], ".,;:)")
			if ref == "" || !strings.ContainsAny(ref, "/.") || seen[ref] {
				continue
			}
			seen[ref] = true
			out = append(out, ref)
		}
	}
	return out
}

// expandImports appends the files text refers to with @path (relative to the
// file; depth limited; each file once; only inside the allowed area; the
// total bounded by budget).
func expandImports(ctx context.Context, src source, file, text string, depth int, seen map[string]bool, budget *int) string {
	if depth >= MaxImportDepth {
		return text
	}
	var b strings.Builder
	b.WriteString(text)
	for _, ref := range importsOf(text) {
		if strings.HasPrefix(ref, "~") {
			continue // never the user's home
		}
		p := src.join(src.dir(file), ref)
		if !src.within(p) || seen[p] || *budget <= 0 {
			continue
		}
		data, size, ok := src.read(ctx, p, MaxBytes)
		if !ok {
			continue
		}
		seen[p] = true
		body := strings.ToValidUTF8(string(data), "")
		if size > int64(len(data)) {
			body = strings.TrimRight(body, "\n") + "\n[…cut: read_file " + p + " for the rest]"
		}
		body = expandImports(ctx, src, p, body, depth+1, seen, budget)
		if len(body) > *budget {
			body = strings.ToValidUTF8(body[:*budget], "") + "\n[…cut: size limit of the imports reached]"
		}
		*budget -= len(body)
		b.WriteString("\n\n[imported from " + src.rel(p) + "]\n" + strings.TrimSpace(body))
	}
	return b.String()
}

// ---- escaping ----

var closingTag = regexp.MustCompile(`(?i)<(\s*/?\s*project_instructions)`)

// escapeTag keeps the text from closing the block or opening a fake one.
func escapeTag(s string) string {
	return closingTag.ReplaceAllStringFunc(s, func(m string) string { return "<\\" + m[1:] })
}

func escapeAttr(s string) string {
	return strings.NewReplacer(`"`, "'", "<", "", ">", "", "\n", " ").Replace(s)
}

func itoa(n int) string { return strconv.Itoa(n) }
