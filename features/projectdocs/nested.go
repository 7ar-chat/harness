package projectdocs

import (
	"context"
	"path"
	"strings"

	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/engine/tools"
)

// AfterPath is the hook for sandboxtools.Options.AfterPath: when a tool
// touches a path covered by notes that the system prompt does not carry, they
// are returned (to be appended to the tool's result) the first time in the
// run:
//
//   - the AGENTS.md (else CLAUDE.md) of the closest directory above the path
//     that has one, below the workspace root (the root's is in the prompt);
//   - the Cursor rules with globs that match the path (when the repository has
//     no AGENTS.md).
//
// Each is shown once per run, as "[Instructions for <dir>: …]". The system
// prompt is not touched, so it stays identical across steps.
func (d *Docs) AfterPath(ctx context.Context, rc tools.RunContext, sb sandbox.Sandbox, p string) string {
	if !strings.HasPrefix(p, wsPrefix) {
		return ""
	}
	rel := strings.TrimPrefix(path.Clean(p), wsPrefix)
	if rel == "" || strings.HasPrefix(rel, "..") {
		return ""
	}
	st := d.state(rc.RunID)
	st.mu.Lock()
	defer st.mu.Unlock()
	ld := newLoader(sb, "")
	d.ensureInit(ctx, ld, st)

	var notes []string
	if dir, name := closestNotes(ctx, ld, st, path.Dir(rel)); name != "" {
		key := dir + "/" + name
		if !st.shown[key] {
			st.shown[key] = true
			if text, ok := ld.text(ctx, ld.ws, wsPrefix+key, key, MaxNestedBytes); ok && strings.TrimSpace(text) != "" {
				notes = append(notes, "[Instructions for "+dir+" ("+key+", repository notes for files under this directory): "+escapeNote(text)+"]")
			}
		}
	}
	for _, r := range st.rules {
		if r.always || !r.matches(rel) || st.shown[r.rel] || strings.TrimSpace(r.body) == "" {
			continue
		}
		st.shown[r.rel] = true
		notes = append(notes, "[Instructions for "+strings.Join(r.globs, ", ")+" ("+r.rel+", repository rule): "+escapeNote(r.body)+"]")
	}
	return strings.Join(notes, "\n")
}

// closestNotes finds the closest directory (dir, then its parents, below the
// root) with an AGENTS.md or CLAUDE.md.
func closestNotes(ctx context.Context, ld *loader, st *runState, dir string) (string, string) {
	for ; dir != "." && dir != "/" && dir != ""; dir = path.Dir(dir) {
		name, known := st.dirNote[dir]
		if !known {
			for _, n := range NestedFiles {
				if _, _, ok := ld.ws.read(ctx, wsPrefix+dir+"/"+n, 1); ok {
					name = n
					break
				}
			}
			st.dirNote[dir] = name
		}
		if name != "" {
			return dir, name
		}
	}
	return "", ""
}

// ensureInit learns whether the repository has an AGENTS.md at its root and,
// when it has not, parses the Cursor rules (once per run).
func (d *Docs) ensureInit(ctx context.Context, ld *loader, st *runState) {
	if st.inited {
		return
	}
	st.inited = true
	root, name := ld.rootFile(ctx)
	st.root = root
	st.hasAgents = name == "AGENTS.md"
	if !st.hasAgents {
		st.rules = ld.cursorRules(ctx)
	}
}

// escapeNote keeps a note from closing its own bracket line ambiguously: a
// newline-separated "]" at the end of the note text is harmless, but the text
// must not contain a forged "[Instructions for" marker.
func escapeNote(s string) string {
	s = strings.TrimSpace(s)
	return strings.ReplaceAll(s, "[Instructions for", "[ Instructions for")
}
