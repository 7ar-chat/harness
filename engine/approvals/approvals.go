// Package approvals implements Work's interactive mode: risky tool calls of an
// interactive run pause before they execute until the user approves, denies or
// answers them (agent BeforeToolHook; decisions are applied by the store when
// the resume run is created and by the agent when it starts).
package approvals

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/core/sandbox"
	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/tools"
)

// riskyNames are the tools that always need approval (Work: Bash, Write, Edit,
// MultiEdit; normalized: lower case, "-" and "_" removed).
var riskyNames = map[string]bool{
	"executecommand": true, "writefile": true, "editfile": true,
	"bash": true, "write": true, "edit": true, "multiedit": true,
}

// riskyPrefixes make any tool risky whose (normalized) name starts with them.
var riskyPrefixes = []string{"delete", "remove", "move", "rename"}

var (
	extraMu    sync.RWMutex
	extraRisky = map[string]bool{}
)

// RegisterRisky makes the named tools need approval in interactive runs, like
// the built-in risky ones (a host's own tools that act on the world). Call it
// at start-up, before runs.
func RegisterRisky(names ...string) {
	extraMu.Lock()
	defer extraMu.Unlock()
	for _, n := range names {
		extraRisky[normalize(n)] = true
	}
}

func registeredRisky(n string) bool {
	extraMu.RLock()
	defer extraMu.RUnlock()
	return extraRisky[n]
}

func normalize(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	return strings.NewReplacer("-", "", "_", "").Replace(n)
}

// IsRisky reports tools that need the user's approval in interactive mode:
// execute_command, write_file, edit_file and any tool whose name starts with
// delete/remove/move/rename — for MCP tools ({server}__{tool}) the tool part
// counts (Work's is_risky_tool). Every part after a "__" is a candidate, so a
// tool named "delete__all" (srv__delete__all) is risky too.
func IsRisky(toolName string) bool {
	candidates := []string{toolName}
	for rest := toolName; ; {
		i := strings.Index(rest, "__")
		if i < 0 {
			break
		}
		rest = rest[i+2:]
		candidates = append(candidates, rest)
	}
	for _, c := range candidates {
		n := normalize(c)
		if n == "" {
			continue
		}
		if riskyNames[n] || registeredRisky(n) {
			return true
		}
		for _, p := range riskyPrefixes {
			if strings.HasPrefix(n, p) {
				return true
			}
		}
	}
	return false
}

// Gated reports whether a call of an interactive run must wait for approval.
func Gated(mode, toolName string) bool {
	return mode == store.InteractionInteractive && IsRisky(toolName)
}

// Preview is the canonical JSON of tool arguments (sorted keys, compact, no
// HTML escaping, numbers kept as written: Work's build_tool_input_preview).
// Invalid JSON is returned trimmed as is.
func Preview(arguments string) string {
	args := strings.TrimSpace(arguments)
	if args == "" {
		args = "{}"
	}
	dec := json.NewDecoder(strings.NewReader(args))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return args
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return args
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// PreviewMatches compares an approved preview with the current call's preview
// (Work's _preview_matches: empty expected matches, "..." is a truncated prefix).
func PreviewMatches(expected, current string) bool {
	e := strings.TrimSpace(expected)
	c := strings.TrimSpace(current)
	switch {
	case e == "", e == c:
		return true
	case strings.HasSuffix(e, "..."):
		return strings.HasPrefix(c, strings.TrimSuffix(e, "..."))
	}
	return false
}

// MatchesCall reports whether an approval still describes a tool call (same
// tool, same arguments): an approve decision only runs a matching call.
func MatchesCall(a *store.Approval, tc schema.ToolCall) bool {
	return a.ToolCallID == tc.ID && normalize(a.ToolName) == normalize(tc.Function.Name) &&
		PreviewMatches(a.ToolInputPreview, Preview(tc.Function.Arguments))
}

// MismatchResult is the tool result of an approved call whose arguments changed.
const MismatchResult = "Error: the approved action no longer matches this tool call (its tool or arguments changed), so it was not run. Ask the user again if it is still needed."

// PauseFailedResult is the tool result when the approval could not be recorded (fail closed).
const PauseFailedResult = "Error: the approval request could not be recorded, so the action was not run. Try again later."

// Store is the persistence used by the gate.
type Store interface {
	CreateApproval(ctx context.Context, a *store.Approval) error
	// RunInteractionMode is the run's current mode: the user may switch it
	// while the run is active ("" is automatic).
	RunInteractionMode(ctx context.Context, runID string) (string, error)
}

// DefaultRiskyToolPatterns are APPROVAL_RISKY_TOOL_PATTERNS' default: the
// mutating verbs that make an MCP tool need approval (matched per name word).
const DefaultRiskyToolPatterns = "create|send|update|delete|post|publish|write"

var defaultRiskyTool = regexp.MustCompile("(?i)^(?:" + DefaultRiskyToolPatterns + ")")

// Gate is the agent's BeforeToolHook for interactive runs (harness v3 §7).
type Gate struct {
	Store Store
	// RiskyToolPattern matches the words of MCP tool names that need approval
	// (APPROVAL_RISKY_TOOL_PATTERNS; nil: DefaultRiskyToolPatterns).
	RiskyToolPattern *regexp.Regexp
	// Resolver resolves symlinks before the work-area check (nil: lexical only).
	Resolver PathResolver
}

// NeedsApproval implements agent.BeforeToolHook. In interactive mode it gates
// execute_command, writes/edits and deletes/moves of paths outside the work
// areas (/workspace/outputs/, /workspace/.work/: never asked), and MCP tools
// whose name has a mutating word (or starts with delete/remove/move/rename);
// a tool kind approved for the run (scope "run") is not asked again. The mode
// is the run's current one (switched mid-run by the user), not the one it
// started with; NeedsApproval has no side effects.
func (g *Gate) NeedsApproval(ctx context.Context, run *store.Run, toolName, arguments string) bool {
	if g.mode(ctx, run) != store.InteractionInteractive || slices.Contains(run.ApprovedTools, Kind(toolName)) {
		return false
	}
	return g.risky(ctx, run, toolName, arguments)
}

// Risky implements agent.RiskJudge: NeedsApproval's rules as if the run were
// interactive (a tool kind approved for the run is still not risky). It reads
// no mode, so calls after a pending approval of the step keep waiting behind it
// after a switch to automatic.
func (g *Gate) Risky(ctx context.Context, run *store.Run, toolName, arguments string) bool {
	if slices.Contains(run.ApprovedTools, Kind(toolName)) {
		return false
	}
	return g.risky(ctx, run, toolName, arguments)
}

// mode is the run's live interaction mode (the mode it started with when there
// is no store). A failed read fails closed: the run may have been switched to
// interactive, so the call is asked (if the store is down, recording the
// approval fails too and the call is refused, never run). Once the run is
// stopped (ctx done) the start mode is kept instead, so the pause, which runs on
// a detached context, records no approval after Stop.
func (g *Gate) mode(ctx context.Context, run *store.Run) string {
	if g.Store == nil || run.ID == "" {
		return run.InteractionMode
	}
	mode, err := g.Store.RunInteractionMode(ctx, run.ID)
	switch {
	case err == nil:
		return mode
	case ctx.Err() != nil:
		slog.Warn("approvals: the run stopped while reading its interaction mode; using the mode it started with",
			"run_id", run.ID, "mode", run.InteractionMode, "error", err)
		return run.InteractionMode
	default:
		slog.Warn("approvals: reading the run's interaction mode failed; asking for approval",
			"run_id", run.ID, "start_mode", run.InteractionMode, "error", err)
		return store.InteractionInteractive
	}
}

// Kind is the tool kind an approval with scope "run" covers (the normalized tool name).
func Kind(toolName string) string { return normalize(toolName) }

// WorkAreas are the sandbox directories written without approval.
var WorkAreas = []string{"/workspace/outputs/", "/workspace/.work/"}

func (g *Gate) risky(ctx context.Context, run *store.Run, toolName, arguments string) bool {
	if i := strings.Index(toolName, "__"); i >= 0 { // MCP: {server}__{tool}
		pattern := g.RiskyToolPattern
		if pattern == nil {
			pattern = defaultRiskyTool
		}
		for _, part := range strings.Split(toolName[i+2:], "__") {
			for _, w := range words(part) {
				if pattern.MatchString(w) {
					return true
				}
			}
		}
		return IsRisky(toolName)
	}
	n := normalize(toolName)
	if registeredRisky(n) {
		return true
	}
	switch n {
	case "executecommand", "bash":
		return true
	case "writefile", "editfile", "write", "edit", "multiedit":
		return !g.inWorkAreas(ctx, run, arguments)
	}
	for _, p := range riskyPrefixes {
		if strings.HasPrefix(n, p) {
			return !g.inWorkAreas(ctx, run, arguments)
		}
	}
	return false
}

// words splits a tool name into lower-case words (snake, kebab and camelCase).
func words(name string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	prevLower := false
	for _, r := range name {
		switch {
		case r == '_' || r == '-' || r == '.' || r == ' ':
			flush()
			prevLower = false
			continue
		case unicode.IsUpper(r) && prevLower:
			flush()
		}
		cur = append(cur, r)
		prevLower = unicode.IsLower(r) || unicode.IsDigit(r)
	}
	flush()
	return out
}

// pathFields are the arguments naming the files a write, delete or move touches.
var pathFields = []string{"path", "paths", "source", "destination", "target", "from", "to", "file", "filePath", "file_path"}

// argumentPaths returns the paths of a call's arguments (ok false: ask). The
// tools decode arguments case-insensitively and the last of repeated keys wins
// (eino InferTool, sonic), so a key repeated in any case, or a path field
// spelled in another case, is refused rather than guessed (review round 1 P2 #1).
func argumentPaths(arguments string) (paths []string, ok bool) {
	dec := json.NewDecoder(strings.NewReader(arguments))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, false
	}
	var keys []string
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, _ := t.(string)
		for _, k := range keys {
			if strings.EqualFold(k, key) {
				return nil, false // repeated key (any case)
			}
		}
		keys = append(keys, key)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, false
		}
		field := ""
		for _, f := range pathFields {
			if strings.EqualFold(f, key) {
				field = f
			}
		}
		if field == "" {
			continue
		}
		if field != key {
			return nil, false // a path field in another case
		}
		var v any
		if json.Unmarshal(raw, &v) != nil {
			return nil, false
		}
		switch v := v.(type) {
		case string:
			paths = append(paths, v)
		case []any:
			for _, x := range v {
				s, isString := x.(string)
				if !isString {
					return nil, false
				}
				paths = append(paths, s)
			}
		case nil:
		default:
			return nil, false
		}
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false // trailing data
	}
	return paths, true
}

// inWorkAreas reports arguments whose every path (at least one) is inside a
// work area; relative paths are under /workspace like the sandbox tools, and
// symlinks are resolved in the thread's sandbox when a Resolver is set.
func (g *Gate) inWorkAreas(ctx context.Context, run *store.Run, arguments string) bool {
	paths, ok := argumentPaths(arguments)
	if !ok || len(paths) == 0 {
		return false
	}
	for i, p := range paths {
		lexical, ok := lexicalPath(p)
		if !ok || !underWorkArea(lexical) {
			return false
		}
		paths[i] = lexical
	}
	if g.Resolver == nil {
		return true
	}
	// Resolved for every decision, never cached: a command or write of the run
	// may have turned a work-area directory into a symbolic link since (final
	// review of round 3, F6; one realpath exec, ~70 ms with docker exec).
	rctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	real, err := g.Resolver.RealPaths(rctx, run, paths)
	if err != nil || len(real) != len(paths) {
		return false // fail closed: ask
	}
	for _, p := range real {
		if !underWorkArea(path.Clean(p)) {
			return false
		}
	}
	return true
}

// resolveTimeout bounds the sandbox realpath of a gate decision (the run's
// live sandbox only: the gate never starts one).
const resolveTimeout = 10 * time.Second

// PathResolver resolves the symlinks of absolute sandbox paths of a run's thread.
type PathResolver interface {
	RealPaths(ctx context.Context, run *store.Run, paths []string) ([]string, error)
}

// SandboxResolver resolves paths with realpath in the thread's live sandbox.
// Without one (round 3, N5) the paths are their own real paths: the gate never
// starts a sandbox, and write_file/edit_file refuse to write through a
// symlink that a workspace restored into the new sandbox may hold (N6).
type SandboxResolver struct{ Sandboxes sandbox.Manager }

// RealPaths implements PathResolver.
func (r SandboxResolver) RealPaths(ctx context.Context, run *store.Run, paths []string) ([]string, error) {
	sb, err := r.Sandboxes.Get(ctx, run.ThreadID)
	if errors.Is(err, sandbox.ErrNoSandbox) {
		return paths, nil
	}
	if err != nil {
		return nil, err
	}
	return sandbox.RealPaths(ctx, sb, paths)
}

func lexicalPath(p string) (string, bool) {
	p = strings.TrimSpace(p)
	if p == "" || strings.ContainsRune(p, 0) {
		return "", false
	}
	if !strings.HasPrefix(p, "/") {
		p = "/workspace/" + p
	}
	return path.Clean(p), true
}

func underWorkArea(p string) bool {
	for _, area := range WorkAreas {
		if strings.HasPrefix(p, area) {
			return true
		}
	}
	return false
}

// Pause records the pending approval of a call and returns the interrupt that ends the run.
func (g *Gate) Pause(ctx context.Context, run *store.Run, tc schema.ToolCall) (*tools.Interrupt, error) {
	a := &store.Approval{Kind: store.ApprovalKindRiskyTool, ThreadID: run.ThreadID, RunID: run.ID, ToolCallID: tc.ID,
		ToolName: tc.Function.Name, ToolInputPreview: Preview(tc.Function.Arguments)}
	if err := g.Store.CreateApproval(ctx, a); err != nil {
		return nil, err
	}
	return Interrupt(a), nil
}

// Interrupt is the AG-UI interrupt of a pending approval.
func Interrupt(a *store.Approval) *tools.Interrupt {
	return &tools.Interrupt{Reason: store.ReasonToolApproval, Question: a.ToolName, AllowFreeText: true,
		ID: a.ID, ToolCallID: a.ToolCallID, Approval: a}
}
