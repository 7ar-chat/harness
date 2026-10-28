package sandboxtools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/GhaziBenDahmane/harness/engine/tools"
	"github.com/GhaziBenDahmane/harness/sandbox"
)

const searchDesc = `Search file contents with a regular expression (ripgrep) and get the matching lines with their line numbers.
path is a directory or a file (default /workspace); glob filters file names (e.g. "*.csv", "reports/**/*.md"); ignoreCase
makes the search case-insensitive. Hidden directories such as /workspace/.work/ are searched (saved tool outputs live there);
.git is not. Returns JSON {matches: [{path, line, text}], truncated} with at most maxResults matches (default 100, max 500).
Use it instead of grep in execute_command, then read_file with offset to see the context of a match.`

const (
	defaultSearchResults = 100
	maxSearchResults     = 500
	maxMatchText         = 300
	searchWait           = 60 * time.Second
)

type searchInput struct {
	Pattern    string `json:"pattern" jsonschema_description:"Regular expression to search for (ripgrep/Rust syntax; escape special characters for a literal search)"`
	Path       string `json:"path,omitempty" jsonschema_description:"Directory or file to search (default /workspace)"`
	Glob       string `json:"glob,omitempty" jsonschema_description:"Only search files matching this glob, e.g. \"*.py\" or \"data/**/*.csv\""`
	IgnoreCase bool   `json:"ignoreCase,omitempty" jsonschema_description:"Case-insensitive search (default false)"`
	MaxResults int    `json:"maxResults,omitempty" jsonschema_description:"Maximum number of matches to return (default 100, max 500)"`
}

type searchMatch struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

type searchOutput struct {
	Matches   []searchMatch `json:"matches"`
	Truncated bool          `json:"truncated"`
	Notice    string        `json:"notice,omitempty"`
}

// searchScript runs ripgrep (--json) for HARNESS_PATTERN under HARNESS_PATH; without
// rg (or with HARNESS_SEARCH_ENGINE=python) a Python fallback prints the same
// "match" events. Exit 0: matches, 1: none, 2: error (stderr).
const searchScript = `# work:search
set -u
if [ "${HARNESS_SEARCH_ENGINE:-}" != python ] && command -v rg >/dev/null 2>&1; then
  set -- --json --hidden --glob '!.git' --max-columns 2000 --max-filesize 50M
  [ "${HARNESS_ICASE:-}" = 1 ] && set -- "$@" --ignore-case
  [ -n "${HARNESS_GLOB:-}" ] && set -- "$@" --glob "$HARNESS_GLOB"
  rg "$@" -e "$HARNESS_PATTERN" -- "$HARNESS_PATH" | head -c 8000000
  exit "${PIPESTATUS[0]}"
fi
python3 - <<'HARNESS_EOF'
import fnmatch, json, os, re, sys
pat, root, glob = os.environ["HARNESS_PATTERN"], os.environ["HARNESS_PATH"], os.environ.get("HARNESS_GLOB", "")
try:
    rx = re.compile(pat, re.IGNORECASE if os.environ.get("HARNESS_ICASE") == "1" else 0)
except re.error as e:
    sys.stderr.write("regex parse error: %s\n" % e); sys.exit(2)
limit, found = int(os.environ.get("HARNESS_MAX", "500")) + 1, 0
def files():
    if os.path.isfile(root):
        yield root; return
    for d, dirs, names in os.walk(root):
        dirs[:] = [x for x in dirs if x != ".git"]
        for n in sorted(names):
            p = os.path.join(d, n)
            rel = os.path.relpath(p, root)
            if glob and not (fnmatch.fnmatch(n, glob) or fnmatch.fnmatch(rel, glob)):
                continue
            yield p
if not os.path.exists(root):
    sys.stderr.write("%s: No such file or directory\n" % root); sys.exit(2)
for p in files():
    try:
        with open(p, "rb") as f:
            data = f.read(50 << 20)
    except OSError:
        continue
    if b"\0" in data[:8192]:
        continue
    for i, line in enumerate(data.decode("utf-8", "replace").split("\n"), 1):
        if rx.search(line):
            print(json.dumps({"type": "match", "data": {"path": {"text": p}, "line_number": i, "lines": {"text": line[:2000] + "\n"}}}))
            found += 1
            if found >= limit:
                sys.exit(0)
sys.exit(0 if found else 1)
HARNESS_EOF
`

func (b *binding) search(ctx context.Context, in searchInput) (*searchOutput, error) {
	if strings.TrimSpace(in.Pattern) == "" {
		return nil, tools.Errorf("pattern is required")
	}
	if strings.TrimSpace(in.Path) == "" {
		in.Path = sandbox.Workspace
	}
	p, err := resolve(in.Path)
	if err != nil {
		return nil, err
	}
	limit := in.MaxResults
	if limit <= 0 {
		limit = defaultSearchResults
	}
	limit = min(limit, maxSearchResults)
	icase := ""
	if in.IgnoreCase {
		icase = "1"
	}
	out := &searchOutput{Matches: []searchMatch{}}
	err = b.withSandbox(ctx, func(sb sandbox.Sandbox) error {
		out.Matches, out.Truncated = out.Matches[:0], false
		var stderr strings.Builder
		var pending strings.Builder
		feed := func(text string) {
			pending.WriteString(text)
			buf := pending.String()
			last := strings.LastIndexByte(buf, '\n')
			if last < 0 {
				return
			}
			pending.Reset()
			pending.WriteString(buf[last+1:])
			for _, line := range strings.Split(buf[:last], "\n") {
				if m, ok := parseMatch(line); ok {
					if len(out.Matches) >= limit {
						out.Truncated = true
						continue
					}
					out.Matches = append(out.Matches, m)
				}
			}
		}
		cctx, cancel := context.WithTimeout(ctx, searchWait+5*time.Second)
		defer cancel()
		res, err := sb.Exec(cctx, sandbox.ExecRequest{Command: searchScript, Cwd: sandbox.Workspace, Timeout: searchWait,
			Env:      map[string]string{"HARNESS_PATTERN": in.Pattern, "HARNESS_PATH": p, "HARNESS_GLOB": in.Glob, "HARNESS_ICASE": icase, "HARNESS_MAX": fmt.Sprint(limit)},
			OnStdout: feed,
			OnStderr: func(t string) {
				if stderr.Len() < 2000 {
					stderr.WriteString(t)
				}
			}})
		feed("\n")
		if err != nil {
			if ctx.Err() != nil || sandbox.IsGone(err) {
				return err
			}
			if cctx.Err() != nil {
				out.Truncated = true // timed out: keep what was found
				return nil
			}
			return fmt.Errorf("search failed: %w", err)
		}
		if res.ExitCode >= 2 && len(out.Matches) == 0 {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = fmt.Sprintf("exit %d", res.ExitCode)
			}
			return tools.Errorf("search failed: %s", msg)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// parseMatch decodes one ripgrep --json "match" event.
func parseMatch(line string) (searchMatch, bool) {
	var ev struct {
		Type string `json:"type"`
		Data struct {
			Path struct {
				Text string `json:"text"`
			} `json:"path"`
			LineNumber int `json:"line_number"`
			Lines      struct {
				Text string `json:"text"`
			} `json:"lines"`
		} `json:"data"`
	}
	if !strings.HasPrefix(line, "{") || json.Unmarshal([]byte(line), &ev) != nil || ev.Type != "match" {
		return searchMatch{}, false
	}
	text := strings.TrimRight(ev.Data.Lines.Text, "\r\n")
	if len(text) > maxMatchText {
		text = truncateUTF8(text, maxMatchText) + "…"
	}
	return searchMatch{Path: ev.Data.Path.Text, Line: ev.Data.LineNumber, Text: text}, true
}
