package artifacts

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/GhaziBenDahmane/harness/engine/agent"
	"github.com/GhaziBenDahmane/harness/engine/agui"
	"github.com/GhaziBenDahmane/harness/store"
)

// fileLinkRe matches the target of a workspace-file link (absolute path).
var fileLinkRe = regexp.MustCompile(`workspace-file://(/[^)\s"'<>\]]+)`)

// linkedPaths returns the paths of the workspace-file links in text, in order, without duplicates.
func linkedPaths(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range fileLinkRe.FindAllStringSubmatch(text, -1) {
		p := m[1]
		if u, err := url.PathUnescape(p); err == nil {
			p = u
		}
		p = path.Clean(p)
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// danglingLinksNote is the corrective note for links without a saved file.
func danglingLinksNote(paths []string) string {
	return fmt.Sprintf("These linked files don't exist as saved files: %s; save them under /workspace/outputs/ or fix the links, then give your final answer again.", strings.Join(paths, ", "))
}

var _ agent.AnswerChecker = (*Service)(nil)

// CheckAnswer implements agent.AnswerChecker: when the answer links workspace
// files, the workspace is snapshotted (the end-of-run snapshot) and the links
// whose path has no non-deleted artifact (e.g. /workspace/.work/ files, which
// are never saved) get a corrective note; saved files the answer names
// without linking them (by absolute path, or by the name of an output of this
// run) get one too (harness v3 4.6).
func (s *Service) CheckAnswer(ctx context.Context, run *store.Run, answer string, emit func(agui.Event) error) string {
	if s == nil || run.Kind == store.KindCompact {
		return ""
	}
	links := linkedPaths(answer)
	if len(links) > 0 {
		s.SnapshotEnd(ctx, run, emit)
	}
	all, err := s.Store.ListArtifacts(ctx, run.ThreadID)
	if err != nil {
		slog.Warn("artifacts: listing for the link check failed", "run_id", run.ID, "error", err)
		return ""
	}
	saved := map[string]bool{}
	for _, a := range all {
		if !a.Deleted {
			saved[a.Path] = true
		}
	}
	var dangling []string
	linked := map[string]bool{}
	for _, p := range links {
		linked[p] = true
		if !saved[p] {
			dangling = append(dangling, p)
		}
	}
	unlinked := unlinkedMentions(answer, all, linked, run.ID)
	var notes []string
	if len(dangling) > 0 {
		slog.Info("artifacts: the answer links unsaved files; asking for a correction", "run_id", run.ID, "paths", len(dangling))
		notes = append(notes, danglingLinksNote(dangling))
	}
	if len(unlinked) > 0 {
		notes = append(notes, unlinkedNote(unlinked))
	}
	return strings.Join(notes, "\n")
}

// mdLinkRe matches markdown links (their text and target are not plain mentions).
var mdLinkRe = regexp.MustCompile(`\[[^\]]*\]\([^)]*\)`)

// absPathRe matches an absolute /workspace path in plain text.
var absPathRe = regexp.MustCompile(`/workspace/[^\s)\]"'<>` + "`" + `*,;]+`)

// unlinkedMentions are the saved files the answer names outside links: by
// absolute path, or by the base name of an output this run produced.
func unlinkedMentions(answer string, all []*store.Artifact, linked map[string]bool, runID string) []string {
	plain := mdLinkRe.ReplaceAllString(answer, " ")
	found := map[string]bool{}
	saved := map[string]bool{}
	for _, a := range all {
		if !a.Deleted {
			saved[a.Path] = true
		}
	}
	for _, m := range absPathRe.FindAllString(plain, -1) {
		p := path.Clean(strings.TrimRight(m, ".:!?"))
		if saved[p] && !linked[p] {
			found[p] = true
		}
	}
	for _, a := range all {
		if a.Deleted || a.RunID != runID || a.Kind != store.ArtifactOutput || linked[a.Path] || !strings.HasPrefix(a.Path, "/workspace/outputs/") {
			continue
		}
		name := path.Base(a.Path)
		re := regexp.MustCompile(`(^|[^\w./-])` + regexp.QuoteMeta(name) + `($|[^\w/-])`)
		if re.MatchString(plain) {
			found[a.Path] = true
		}
	}
	out := make([]string, 0, len(found))
	for p := range found {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// unlinkedNote is the corrective note for saved files named without a link.
func unlinkedNote(paths []string) string {
	return fmt.Sprintf("You mention files without linking them: %s; link each one as [name](workspace-file:///absolute/path) so the user can open it, then give your final answer again.", strings.Join(paths, ", "))
}
