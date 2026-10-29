package tools

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/GhaziBenDahmane/harness/core/sandbox"
)

// SpillDir is where large tool outputs are written in full (scratch space, never saved).
const SpillDir = sandbox.ToolOutputDir + "/"

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// ErrNoSpill: there is no running sandbox to spill into (the output is cut instead).
var ErrNoSpill = errors.New("tools: no running sandbox to keep the full output")

// SpillPath is a new file for a tool call's spilled output: the call id (made
// safe) and a random suffix, so a provider reusing call ids never overwrites
// an earlier output.
func SpillPath(toolCallID string) string {
	name := unsafeName.ReplaceAllString(toolCallID, "_")
	if len(name) > 80 {
		name = name[:80]
	}
	if name == "" || name == "." || name == ".." {
		name = "output"
	}
	return SpillDir + name + "-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8] + ".txt"
}

// SandboxSpiller writes spilled outputs into the thread's running sandbox. It
// never creates one (Get, not Acquire): a large result of a tool that needs
// no sandbox (MCP, web search) in a thread without one is cut instead
// (ErrNoSpill).
type SandboxSpiller struct {
	Sandboxes sandbox.Manager
}

// SpillOutput implements OutputSpiller.
func (s SandboxSpiller) SpillOutput(ctx context.Context, rc RunContext, toolCallID, content string) (string, error) {
	if s.Sandboxes == nil || !s.Sandboxes.Enabled() || rc.Sandbox == SandboxOff {
		return "", ErrNoSpill
	}
	sb, err := s.Sandboxes.Get(ctx, rc.ThreadID)
	if errors.Is(err, sandbox.ErrNoSandbox) {
		return "", ErrNoSpill
	}
	if err != nil {
		return "", err
	}
	// The directory is created here too: the sandbox may predate the layout,
	// or the agent may have removed /workspace/.work.
	if err := sb.MkdirAll(ctx, sandbox.ToolOutputDir); err != nil {
		return "", err
	}
	path := SpillPath(toolCallID)
	if err := sb.WriteFile(ctx, path, []byte(content)); err != nil {
		return "", err
	}
	return path, nil
}
