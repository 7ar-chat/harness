package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/engine/tools"
)

// Harness v3 §2: large tool outputs are spilled to a file.
const (
	defaultSpillChars = 20000
	spillHeadChars    = 8000
	spillTailChars    = 4000
)

// spillChars is the spill threshold, never above the tool-output cap
// (AGENT_MAX_TOOL_OUTPUT_CHARS): a result between the two would be cut in the
// middle without its full text being kept (A-04).
func (a *Agent) spillChars() int {
	limit := defaultSpillChars
	if a.SpillChars > 0 {
		limit = a.SpillChars
	}
	if a.MaxToolOutputChars > 0 && a.MaxToolOutputChars < limit {
		limit = a.MaxToolOutputChars
	}
	return limit
}

// spill returns the model's view of a tool output: unchanged when short;
// otherwise the full output goes to a file of /workspace/.work/tool-output/
// and the model gets its head and tail, the path and a paging hint. A
// JSON-object result stays valid JSON: its large strings are cut in place and
// it gains "spilled": true and "outputPath" ("spillPath" when it already has an
// outputPath of its own). Without a spiller (or a running sandbox) the output
// is cut with a note that the full text was not kept.
func (st *runState) spill(ctx context.Context, tc ai.ToolCall, content string) string {
	limit := st.a.spillChars()
	n := utf8.RuneCountInString(content)
	if n <= limit {
		return content
	}
	path, err := "", tools.ErrNoSpill
	if st.a.Harness.Spill != nil {
		rc := tools.RunContext{ThreadID: st.in.Run.ThreadID, RunID: st.in.Run.ID, Login: st.in.Run.UserLogin, Sandbox: st.in.Run.Sandbox}
		path, err = st.a.Harness.Spill.SpillOutput(ctx, rc, tc.ID, content)
		if err != nil && !errors.Is(err, tools.ErrNoSpill) {
			slog.Warn("agent: spilling a large tool output failed; cutting it", "run_id", st.in.Run.ID, "tool", tc.Function.Name, "error", err)
		}
	}
	if err != nil {
		return cutWithNote(content, limit)
	}
	if isJSONObject(content) {
		if out, ok := spillJSON(content, limit, path); ok {
			return out
		}
	}
	head, tail := spillHeadChars, spillTailChars
	if head+tail > limit*3/4 { // small limits (tests): keep the same 2:1 shape
		head, tail = limit/2, limit/4
	}
	r := []rune(content)
	return string(r[:head]) + spillMarker(n-head-tail, n, path) + string(r[n-tail:])
}

func spillMarker(omitted, total int, path string) string {
	return fmt.Sprintf("\n\n[… %d of %d characters not shown. The full output is in %s: page it with read_file (offset/limit) or search it with search_files instead of re-running the tool. …]\n\n",
		omitted, total, path)
}

// spillJSON is the model's view of a spilled JSON-object result (valid JSON).
func spillJSON(content string, limit int, path string) (string, bool) {
	root, err := parseJSON(strings.TrimSpace(content))
	if err != nil {
		return "", false
	}
	key := "outputPath"
	for _, k := range root.keys {
		if k == "outputPath" {
			key = "spillPath"
		}
	}
	extra := map[string]*jsonNode{key: {kind: 's', str: path, runes: utf8.RuneCountInString(path)}, "spilled": {kind: 'v', raw: "true"}}
	marker := func(omitted, total int) string {
		return fmt.Sprintf("\n[… %d of %d characters not shown; the full result is in %s: page it with read_file or search it with search_files …]\n", omitted, total, path)
	}
	if out, ok := shrinkJSON(content, limit, marker, extra); ok {
		return out, true
	}
	// Not made of large strings (long arrays…): a valid JSON preview instead.
	preview, _ := marshalNoEscape(map[string]any{"spilled": true, key: path, "preview": content})
	return shrinkJSON(preview, limit, marker, nil)
}

// cutWithNote cuts an output that could not be spilled to head and tail; a
// JSON object keeps its shape ("truncated": true).
func cutWithNote(content string, limit int) string {
	note := func(omitted, total int) string {
		return fmt.Sprintf("\n[… %d of %d characters not shown; the full output could not be kept (no running sandbox) …]\n", omitted, total)
	}
	if isJSONObject(content) {
		if out, ok := shrinkJSON(content, limit, note, map[string]*jsonNode{"truncated": {kind: 'v', raw: "true"}}); ok {
			return out
		}
	}
	r := []rune(content)
	n := len(r)
	m := note(0, n)
	keep := max(0, limit-utf8.RuneCountInString(m)-8)
	head := keep * 2 / 3
	tail := keep - head
	return string(r[:head]) + note(n-head-tail, n) + string(r[n-tail:])
}
