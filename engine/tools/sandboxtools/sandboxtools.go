// Package sandboxtools exposes the thread sandbox to the model:
// execute_command (with background jobs: job_output, job_kill), read_file
// (text, documents, images), view_file (rendered pages), search_files,
// write_file, edit_file and list_files.
package sandboxtools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/google/uuid"

	"github.com/GhaziBenDahmane/harness/engine/tools"
	"github.com/GhaziBenDahmane/harness/sandbox"
)

const (
	maxLiveOutput     = 256 << 10 // 256 KiB of live output events per call
	maxReadBytes      = 5 << 20   // 5 MiB read_file / edit_file cap
	defaultReadLimit  = 2000
	maxLineChars      = 2000
	maxListEntries    = 500
	defaultListDepth  = 2
	maxListDepth      = 5
	maxCommandTimeout = 30 * time.Minute
	coalesceEvery     = 100 * time.Millisecond
	// maxReadChars bounds one read_file page (below the agent's spill threshold,
	// so a page is never cut or spilled: its footer is always true).
	maxReadChars = 18000
)

// Options configures the provider.
type Options struct {
	// CommandTimeout is the default execute_command timeout (default 120 s).
	CommandTimeout time.Duration
	// MaxSandboxesPerUser is only used in the quota error message.
	MaxSandboxesPerUser int
	// OnAcquire runs after every successful sandbox acquisition (installs the skills the run needs).
	OnAcquire func(ctx context.Context, rc tools.RunContext, sb sandbox.Sandbox)
	// BeforeCommand runs before every execute_command command (foreground or
	// background job) starts, e.g. to install the skills it names
	// (/skills/<name>/). A sandbox found gone fails the call (retried on a new
	// sandbox); other errors are logged and the command runs.
	BeforeCommand func(ctx context.Context, rc tools.RunContext, sb sandbox.Sandbox, command string) error
	// OnWrite runs after write_file or edit_file wrote content to path (e.g.
	// to install the skills the content names); its errors are logged.
	OnWrite func(ctx context.Context, rc tools.RunContext, sb sandbox.Sandbox, path string, content []byte) error
	// CheckWrite runs after write_file or edit_file wrote path; a non-empty result
	// (a syntax error, say) is added to the tool result as its notice.
	CheckWrite func(ctx context.Context, sb sandbox.Sandbox, path string) string
	// AfterFailedCommand runs after a foreground command exited non-zero,
	// with its output (stdout then stderr); a non-empty result is added to the
	// tool result as its note (e.g. "Skill x was installed; run the command
	// again."). The command is never re-run.
	AfterFailedCommand func(ctx context.Context, rc tools.RunContext, sb sandbox.Sandbox, output string) string
	// KeepAlive renews the sandbox of a running background job until stop
	// (jobs.go; default sandbox.KeepAlive).
	KeepAlive func(ctx context.Context, sb sandbox.Sandbox) (stop func())
	// JobPollEvery is how often a kept-alive job's status is checked (default 60 s);
	// JobKeepAliveMax bounds the keep-alive of one job (default 6 h).
	JobPollEvery    time.Duration
	JobKeepAliveMax time.Duration
	// Jobs persists the running background jobs (nil: in memory only, their
	// keep-alives end with the process).
	Jobs JobList
	// Coding describes the tools for a coding agent (AGENT_PROFILE=coding): no
	// document features in read_file, execute_command and the like (coding.go).
	Coding bool
}

// Provider is the sandbox tools provider.
type Provider struct {
	mgr  sandbox.Manager
	opts Options
	// seen remembers, per thread, the content hash of the files the model
	// last read or wrote (edit_file refuses stale or unread files).
	seen *observations
	// jobs keeps the sandboxes of running background jobs alive (jobs.go).
	jobs *jobKeeper
}

// New returns the provider (nil when the manager is disabled).
func New(mgr sandbox.Manager, opts Options) *Provider {
	if mgr == nil || !mgr.Enabled() {
		return nil
	}
	if opts.CommandTimeout <= 0 {
		opts.CommandTimeout = 2 * time.Minute
	}
	if opts.KeepAlive == nil {
		opts.KeepAlive = sandbox.KeepAlive
	}
	if opts.JobPollEvery <= 0 {
		opts.JobPollEvery = defaultJobPollEvery
	}
	if opts.JobKeepAliveMax <= 0 {
		opts.JobKeepAliveMax = defaultJobKeepAliveMax
	}
	return &Provider{mgr: mgr, opts: opts, seen: newObservations(), jobs: &jobKeeper{list: opts.Jobs, owner: uuid.NewString()}}
}

func (p *Provider) Name() string { return "sandbox" }

// readOnlyTool marks a tool without side effects (tools.ReadOnly).
type readOnlyTool struct{ tool.InvokableTool }

func (readOnlyTool) ReadOnly() bool { return true }

var _ tools.ReadOnly = readOnlyTool{}

// Tools implements tools.Provider. The sandbox is acquired lazily on first tool call.
func (p *Provider) Tools(_ context.Context, rc tools.RunContext) ([]tool.BaseTool, func(), error) {
	b := &binding{p: p, rc: rc}
	var out []tool.BaseTool
	add := func(readOnly bool) func(t tool.InvokableTool, err error) error {
		return func(t tool.InvokableTool, err error) error {
			if err != nil {
				return err
			}
			if readOnly {
				t = readOnlyTool{t}
			}
			out = append(out, t)
			return nil
		}
	}
	rw, ro := add(false), add(true)
	d := workDescriptions
	readNotice := func(o string, n string) string { return n + "\n" + o }
	readTool := func() (tool.InvokableTool, error) {
		return utils.InferTool("read_file", d.read, withRecreateNotice(b.read, readNotice))
	}
	if p.opts.Coding {
		d = codingDescriptions
		readTool = func() (tool.InvokableTool, error) {
			return utils.InferTool("read_file", d.read, withRecreateNotice(b.readCoding, readNotice))
		}
	}
	if err := errors.Join(
		rw(utils.InferTool("execute_command", d.exec, withRecreateNotice(b.execute, func(o *execOutput, n string) *execOutput { o.Notice = n; return o }))),
		ro(utils.InferTool("job_output", jobOutputDesc, b.jobOutput)),
		rw(utils.InferTool("job_kill", jobKillDesc, b.jobKill)),
		ro(readTool()),
		ro(utils.InferTool("view_file", viewDesc, withRecreateNotice(b.view, func(o *viewOutput, n string) *viewOutput { o.Notice = n; return o }))),
		ro(utils.InferTool("search_files", d.search, withRecreateNotice(b.search, func(o *searchOutput, n string) *searchOutput { o.Notice = n; return o }))),
		rw(utils.InferTool("write_file", d.write, withRecreateNotice(b.write, func(o *writeOutput, n string) *writeOutput { o.Notice = n; return o }))),
		rw(utils.InferTool("edit_file", d.edit, withRecreateNotice(b.edit, func(o *editOutput, n string) *editOutput { o.Notice = n; return o }))),
		ro(utils.InferTool("list_files", d.list, withRecreateNotice(b.list, func(o *listOutput, n string) *listOutput { o.Notice = n; return o }))),
	); err != nil {
		return nil, nil, err
	}
	return out, func() {}, nil
}

const writeDesc = `Create or overwrite a file in the sandbox with the full content given (parent directories are created).
Returns JSON {path, bytesWritten}. Save deliverables under /workspace/outputs/ (or where the user asked); /workspace/.work/ is scratch space that is not kept.
For a large file, write it in several parts (several files, or write then edit_file) rather than one huge call. Do not use it to change a file the user uploaded: write a new version next to it.`

const listDesc = `List files and directories in the sandbox. path defaults to /workspace, depth to 2 (at most 5).
Returns JSON {path, entries: [{path, type, size}], truncated?} with at most 500 entries. Use search_files to find text inside files.`

type binding struct {
	p  *Provider
	rc tools.RunContext
	// swept are the sandboxes this run already swept (sweepLeftovers).
	mu    sync.Mutex
	swept map[string]bool
}

// Leftover bounds of sweepLeftovers (final review of round 3, F7).
const (
	// stagingMaxAge: a staged upload lives for the seconds of its write.
	stagingMaxAge = 10 * time.Minute
	// endedJobMaxAge and maxEndedJobs bound the shadow records of ended jobs
	// (a job whose directory was deleted reports from its record until then).
	endedJobMaxAge = 24 * time.Hour
	maxEndedJobs   = 200
)

// sweepScript removes, best effort, the staged uploads older than
// HARNESS_STAGING_MINUTES (writes that failed without cleaning up) and the
// shadow records (/tmp/work-jobs/<id>) of ended jobs older than
// HARNESS_JOBS_MINUTES or past the HARNESS_JOBS_KEEP newest; a running job's record stays.
const sweepScript = `# work:sweep
S="$HARNESS_STAGING_DIR"; J="${HARNESS_JOBS_SHADOW_ROOT:-` + jobShadowRoot + `}"
[ -d "$S" ] && find "$S" -mindepth 1 -maxdepth 1 -type f -mmin +"$HARNESS_STAGING_MINUTES" -delete 2>/dev/null
[ -d "$J" ] || exit 0
ended() { [ -f "$1/exit" ] || [ -f "$1/killed" ] || ! { [ -s "$1/pid" ] && kill -0 "$(cat "$1/pid" 2>/dev/null)" 2>/dev/null; }; }
find "$J" -mindepth 1 -maxdepth 1 -type d -mmin +"$HARNESS_JOBS_MINUTES" 2>/dev/null | while IFS= read -r d; do
  ended "$d" && rm -rf -- "$d"
done
ls -1dt -- "$J"/*/ 2>/dev/null | tail -n +"$((HARNESS_JOBS_KEEP + 1))" | while IFS= read -r d; do
  ended "$d" && rm -rf -- "$d"
done
exit 0
`

// sweepLeftovers runs sweepScript once per run and sandbox, at the run's
// first acquisition of it (bounded; its failure is ignored).
func (b *binding) sweepLeftovers(ctx context.Context, sb sandbox.Sandbox) {
	b.mu.Lock()
	if b.swept[sb.ID()] {
		b.mu.Unlock()
		return
	}
	if b.swept == nil {
		b.swept = map[string]bool{}
	}
	b.swept[sb.ID()] = true
	b.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := sb.Exec(cctx, sandbox.ExecRequest{Command: sweepScript, Cwd: "/", Timeout: 5 * time.Second, Env: map[string]string{
		"HARNESS_STAGING_DIR":     sandbox.StagingDir,
		"HARNESS_STAGING_MINUTES": strconv.Itoa(int(stagingMaxAge.Minutes())),
		"HARNESS_JOBS_MINUTES":    strconv.Itoa(int(endedJobMaxAge.Minutes())),
		"HARNESS_JOBS_KEEP":       strconv.Itoa(maxEndedJobs),
	}})
	if err != nil && ctx.Err() == nil {
		slog.Debug("sandboxtools: sweeping leftovers failed", "thread_id", b.rc.ThreadID, "error", err)
	}
}

// RecreatedNotice tells the model its previous sandbox was replaced.
const RecreatedNotice = "Notice: the sandbox stopped or stayed unresponsive and has been recreated; /workspace was reset and previous files are gone."

type noticeKey struct{}

type noticeHolder struct{ notice string }

// sandbox acquires the thread's sandbox; a recreation is recorded for withRecreateNotice.
func (b *binding) sandbox(ctx context.Context) (sandbox.Sandbox, error) {
	sb, notice, err := b.acquire(ctx)
	if h, ok := ctx.Value(noticeKey{}).(*noticeHolder); ok && notice != "" {
		h.notice = notice
	}
	return sb, err
}

// withRecreateNotice adds RecreatedNotice to the result (or error) of a tool
// call during which the sandbox had to be recreated.
func withRecreateNotice[T, D any](fn func(context.Context, T) (D, error), attach func(D, string) D) func(context.Context, T) (D, error) {
	return func(ctx context.Context, in T) (D, error) {
		h := &noticeHolder{}
		out, err := fn(context.WithValue(ctx, noticeKey{}, h), in)
		if h.notice != "" {
			if err != nil {
				return out, withNotice(err, h.notice)
			}
			out = attach(out, h.notice)
		}
		return out, err
	}
}

// withNotice prefixes a tool error with the recreation notice.
func withNotice(err error, notice string) error {
	if err == nil || notice == "" {
		return err
	}
	var ue *tools.UserError
	if errors.As(err, &ue) {
		return tools.Errorf("%s %s", notice, ue.Msg)
	}
	return fmt.Errorf("%s %w", notice, err)
}

// withSandbox runs op on the thread's sandbox; a sandbox found gone is
// replaced and op retried once (sandbox.Retry).
func (b *binding) withSandbox(ctx context.Context, op func(sandbox.Sandbox) error) error {
	return sandbox.Retry(ctx, b.p.mgr, b.rc.ThreadID, b.sandbox, op)
}

func (b *binding) acquire(ctx context.Context) (sandbox.Sandbox, string, error) {
	sb, err := b.p.mgr.Acquire(ctx, b.rc.ThreadID, b.rc.Login)
	if errors.Is(err, sandbox.ErrQuota) {
		return nil, "", tools.Errorf("cannot start a sandbox: the user already has %d active sandboxes in other conversations; ask them to delete an old conversation", b.p.opts.MaxSandboxesPerUser)
	}
	if errors.Is(err, sandbox.ErrUnavailable) {
		return nil, "", tools.Errorf("the sandbox is temporarily unavailable (busy or restarting); wait a moment and retry the tool call")
	}
	if err != nil {
		return nil, "", fmt.Errorf("sandbox unavailable: %w", err)
	}
	if p := b.p.opts.OnAcquire; p != nil {
		p(ctx, b.rc, sb)
	}
	b.sweepLeftovers(ctx, sb)
	if n := sandbox.NoticeOf(sb); n != "" {
		return sb, n, nil // workspace restored from artifacts
	}
	if sandbox.WasRecreated(sb) {
		return sb, RecreatedNotice, nil
	}
	return sb, "", nil
}

// resolve cleans a path; relative paths are under /workspace.
func resolve(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", tools.Errorf("path is required")
	}
	if strings.ContainsRune(p, 0) {
		return "", tools.Errorf("invalid path")
	}
	if !strings.HasPrefix(p, "/") {
		p = sandbox.Workspace + "/" + p
	}
	return path.Clean(p), nil
}

func mapFileErr(err error, p string) error {
	if errors.Is(err, sandbox.ErrNotFound) {
		return tools.Errorf("%s: no such file or directory", p)
	}
	return err
}

// ---- write_file ----

type writeInput struct {
	Path    string `json:"path" jsonschema_description:"File path, absolute or relative to /workspace"`
	Content string `json:"content" jsonschema_description:"The complete new content of the file"`
}

type writeOutput struct {
	Path         string `json:"path"`
	BytesWritten int    `json:"bytesWritten"`
	Notice       string `json:"notice,omitempty"`
}

func (b *binding) write(ctx context.Context, in writeInput) (*writeOutput, error) {
	p, err := resolve(in.Path)
	if err != nil {
		return nil, err
	}
	var notice string
	if err := b.withSandbox(ctx, func(sb sandbox.Sandbox) error {
		if err := writeNoFollow(ctx, sb, p, []byte(in.Content)); err != nil {
			return err
		}
		b.afterWrite(ctx, sb, p, []byte(in.Content))
		notice = b.checkWrite(ctx, sb, p)
		return nil
	}); err != nil {
		return nil, err
	}
	b.p.seen.record(b.rc.ThreadID, p, []byte(in.Content))
	return &writeOutput{Path: p, BytesWritten: len(in.Content), Notice: notice}, nil
}

// checkWrite runs the CheckWrite hook ("" without one).
func (b *binding) checkWrite(ctx context.Context, sb sandbox.Sandbox, p string) string {
	if hook := b.p.opts.CheckWrite; hook != nil {
		return hook(ctx, sb, p)
	}
	return ""
}

// afterWrite runs the OnWrite hook (best effort).
func (b *binding) afterWrite(ctx context.Context, sb sandbox.Sandbox, p string, content []byte) {
	hook := b.p.opts.OnWrite
	if hook == nil {
		return
	}
	if err := hook(ctx, b.rc, sb, p, content); err != nil && ctx.Err() == nil {
		slog.Warn("sandboxtools: after-write hook failed", "thread_id", b.rc.ThreadID, "path", p, "error", err)
	}
}

// writeNoFollow writes a file of write_file/edit_file without following a
// symbolic link at the target or in its directory (round 3, N6).
func writeNoFollow(ctx context.Context, sb sandbox.Sandbox, p string, data []byte) error {
	err := sandbox.WriteFileNoFollow(ctx, sb, p, data)
	var se *sandbox.SymlinkError
	if errors.As(err, &se) {
		return tools.Errorf("%s is a symbolic link or inside one (its directory is really %s); not written: write to the real path instead", p, se.RealDir)
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", p, err)
	}
	return nil
}

// ---- list_files ----

type listInput struct {
	Path  string `json:"path,omitempty" jsonschema_description:"Directory to list (default /workspace)"`
	Depth int    `json:"depth,omitempty" jsonschema_description:"Recursion depth (default 2, max 5)"`
}

type listEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

type listOutput struct {
	Path      string      `json:"path"`
	Entries   []listEntry `json:"entries"`
	Truncated bool        `json:"truncated,omitempty"`
	Notice    string      `json:"notice,omitempty"`
}

func (b *binding) list(ctx context.Context, in listInput) (*listOutput, error) {
	if strings.TrimSpace(in.Path) == "" {
		in.Path = sandbox.Workspace
	}
	p, err := resolve(in.Path)
	if err != nil {
		return nil, err
	}
	depth := in.Depth
	if depth <= 0 {
		depth = defaultListDepth
	}
	if depth > maxListDepth {
		depth = maxListDepth
	}
	var infos []sandbox.FileInfo
	if err := b.withSandbox(ctx, func(sb sandbox.Sandbox) error {
		var err error
		infos, err = sb.List(ctx, p, depth)
		return err
	}); err != nil {
		return nil, mapFileErr(err, p)
	}
	out := &listOutput{Path: p, Entries: []listEntry{}}
	for _, fi := range infos {
		if len(out.Entries) >= maxListEntries {
			out.Truncated = true
			break
		}
		out.Entries = append(out.Entries, listEntry{Path: fi.Path, Type: fi.Type, Size: fi.Size})
	}
	return out, nil
}
