// Package sandbox is the contract between an agent and the place its commands run: a directory on this machine or a
// container elsewhere.
//
// A Backend creates and reconnects Sandboxes (Exec, ReadFile, WriteFile, List, Stat, Healthy). A Manager owns the
// lifecycle for a conversation: one sandbox per thread, created on first use, a quota per user, health checks, and the
// replacement of a dead one. Implement Backend to add a new kind of sandbox and prove it with the conformance suite in
// package sandboxtest; adapters/sandbox has the local and OpenSandbox implementations.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

var (
	// ErrNoSandbox means the thread has no live sandbox (Get never creates one).
	ErrNoSandbox = errors.New("no sandbox for this thread")
	// ErrNotFound means the file or directory does not exist.
	ErrNotFound = errors.New("no such file or directory")
	// ErrQuota means the user reached MAX_SANDBOXES_PER_USER.
	ErrQuota = errors.New("sandbox quota reached")
	// ErrUnavailable is a transient failure (unhealthy, timeout, provider error): retry later.
	// The sandbox is kept; only a definitive "not found" forgets it.
	ErrUnavailable = errors.New("the sandbox is temporarily unavailable")
	// ErrDisabled means OPENSANDBOX_URL is not configured.
	ErrDisabled = errors.New("sandbox is disabled")
	// ErrGone means an operation found the sandbox itself gone at the provider
	// (removed, expired): its handle must be invalidated (see IsGone).
	ErrGone = errors.New("the sandbox no longer exists")
)

// notFoundCode is the OpenSandbox error code of a sandbox that does not exist
// (e.g. "DOCKER::SANDBOX_NOT_FOUND").
const notFoundCode = "SANDBOX_NOT_FOUND"

// IsGone reports whether an operation failed because the sandbox itself no
// longer exists: ErrGone (a 404 the backend attributed to the sandbox) or the
// provider's SANDBOX_NOT_FOUND code, whatever wrapped it. A missing file
// (ErrNotFound) is not a gone sandbox.
func IsGone(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, ErrGone) || strings.Contains(err.Error(), notFoundCode)
}

// Retry runs op on the sandbox returned by acquire. When op fails because the
// sandbox is gone (IsGone), the thread's handle is invalidated and op runs once
// more on a freshly acquired sandbox (which gets the workspace restore and the
// recreation notice). acquire's errors are returned as is.
func Retry(ctx context.Context, mgr Manager, threadID string, acquire func(context.Context) (Sandbox, error), op func(Sandbox) error) error {
	sb, err := acquire(ctx)
	if err != nil {
		return err
	}
	err = op(sb)
	if !IsGone(err) || ctx.Err() != nil {
		return err
	}
	slog.Warn("sandbox gone during an operation; retrying on a new sandbox", "thread_id", threadID, "sandbox_id", sb.ID(), "error", err)
	mgr.Invalidate(ctx, threadID, sb.ID())
	if sb, err = acquire(ctx); err != nil {
		return err
	}
	return op(sb)
}

// Workspace is the working directory inside every sandbox.
const Workspace = "/workspace"

// The /workspace layout the prompts, tools and skills advertise.
const (
	// OutputsDir holds the final deliverables.
	OutputsDir = Workspace + "/outputs"
	// ScratchDir is scratch space, never saved.
	ScratchDir = Workspace + "/.work"
	// ToolOutputDir receives spilled tool outputs.
	ToolOutputDir = ScratchDir + "/tool-output"
	// RendersDir receives view_file renders.
	RendersDir = ScratchDir + "/renders"
)

// Layout lists the directories of a fresh /workspace, parents first.
var Layout = []string{Workspace, OutputsDir, ScratchDir, ToolOutputDir, RendersDir}

// PrepareWorkspace creates the Layout in sb (idempotent). The image ships none
// of it, and the agent's first `cat > /workspace/.work/x.py` would fail.
func PrepareWorkspace(ctx context.Context, sb Sandbox) error {
	for _, d := range Layout {
		if err := sb.MkdirAll(ctx, d); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
	}
	return nil
}

// FileInfo describes a sandbox file.
type FileInfo struct {
	Path       string
	Type       string // "file" | "directory"
	Size       int64
	ModifiedAt time.Time
}

// ExecRequest runs a shell command.
type ExecRequest struct {
	Command  string
	Cwd      string
	Timeout  time.Duration
	Env      map[string]string
	OnStdout func(text string)
	OnStderr func(text string)
}

// ExecResult is the outcome of a command. A non-zero exit is not an error.
type ExecResult struct {
	ExitCode int
}

// Sandbox is a live sandbox usable by tools and the files API.
type Sandbox interface {
	ID() string
	Exec(ctx context.Context, req ExecRequest) (ExecResult, error)
	// ReadFile returns at most maxBytes of the file and its full size.
	ReadFile(ctx context.Context, path string, maxBytes int64) (data []byte, size int64, err error)
	// WriteFile creates parent directories and writes the file (mode 0644).
	WriteFile(ctx context.Context, path string, data []byte) error
	MkdirAll(ctx context.Context, path string) error
	List(ctx context.Context, path string, depth int) ([]FileInfo, error)
	Stat(ctx context.Context, path string) (FileInfo, error)
}

// Instance is a backend sandbox with lifecycle operations.
type Instance interface {
	Sandbox
	Healthy(ctx context.Context) bool
	// Gone reports a definitive end of life (lifecycle state Terminated/Failed, or 404).
	Gone(ctx context.Context) bool
	Renew(ctx context.Context, d time.Duration) error
}

// CreateOptions are per-sandbox creation options.
type CreateOptions struct {
	Metadata map[string]string
}

// Backend talks to the sandbox provider.
type Backend interface {
	Create(ctx context.Context, opts CreateOptions) (Instance, error)
	// Connect returns ErrNoSandbox only when the sandbox definitively no longer
	// exists (404, Terminated/Failed); any other error is transient.
	Connect(ctx context.Context, id string) (Instance, error)
	Kill(ctx context.Context, id string) error
}

// Manager hands out the sandbox of a thread.
type Manager interface {
	Enabled() bool
	// Acquire returns the thread's sandbox, creating it when needed.
	Acquire(ctx context.Context, threadID, login string) (Sandbox, error)
	// Get returns the thread's sandbox or ErrNoSandbox; it never creates one.
	Get(ctx context.Context, threadID string) (Sandbox, error)
	// Kill destroys a sandbox (best effort).
	Kill(ctx context.Context, sandboxID string) error
	// Invalidate drops the handle of a sandbox an operation found gone; once the
	// provider confirms it no longer exists (the definitive 404 rule of
	// Backend.Connect), the thread forgets it and the next Acquire creates a
	// new one, flagged as recreated.
	Invalidate(ctx context.Context, threadID, sandboxID string)
}
