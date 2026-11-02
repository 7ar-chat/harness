package sandboxtools

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/engine/tools"
)

const execDesc = `Run a bash command in this conversation's isolated Linux sandbox (cwd /workspace; python3, node, LibreOffice and
the skill scripts are installed). Returns JSON {stdout, stderr, exitCode, truncated?, outputPath?}; a non-zero exitCode is
reported, not raised. Prefer writing a script with write_file and running it over long one-liners.
Output: stdout keeps its first 4,000 and last 8,000 bytes, stderr its first 1,000 and last 3,000; when cut, truncated is true
and outputPath (and stderrPath) name the file holding the full output: page it with read_file or search_files.
Long commands: a command still running after timeoutSeconds (default 120, max 1800) is not killed; it keeps running as a
background job and the result is {jobId, status: "running", stdoutTail, stderrTail}. background: true returns that at once.
Then use job_output (wait for it, read its output) and job_kill. Use search_files rather than grep, read_file rather than cat.
/workspace/.work/jobs/ holds the commands' output files: never delete it (no rm -rf /workspace/.work).`

// RemovedJobNote is the stderr of a command that deleted its own job
// directory (%s: the directory, %d: the exit code). Round 3, A11.
const RemovedJobNote = "[the command deleted its own output files (%s/, e.g. with rm -rf /workspace/.work), so its output is lost; it exited with code %d. Do not delete /workspace/.work/jobs/: keep your scratch files in a folder of their own under /workspace/.work/]"

const jobOutputDesc = `Get the status and the latest output of a background job started by execute_command, optionally waiting up to
waitSeconds (max 300) for it to finish. Returns JSON {jobId, status: "running" | "finished" | "killed", exitCode?, stdoutTail,
stderrTail, outputPath}; the full output stays in outputPath (stdout) next to the stderr file.`

const jobKillDesc = `Stop a background job started by execute_command (its whole process group). Returns JSON {jobId, status: "killed"}.`

const (
	jobsRoot      = sandbox.Workspace + "/.work/jobs"
	stdoutHead    = 4000
	stdoutTail    = 8000
	stderrHead    = 1000
	stderrTail    = 3000
	runningTail   = 4000 // tails of a running job
	maxJobWait    = 300
	statusTimeout = 30 * time.Second
	killTimeout   = 10 * time.Second
)

var jobIDPattern = regexp.MustCompile(`^job_[a-z0-9]{6,32}$`)

type execInput struct {
	Command        string `json:"command" jsonschema_description:"The bash command (or script) to run"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty" jsonschema_description:"Seconds to wait for the command (default 120, max 1800); a command still running then becomes a background job"`
	Background     bool   `json:"background,omitempty" jsonschema_description:"Start the command as a background job and return its jobId at once (servers, long batches)"`
}

type execOutput struct {
	Stdout     string `json:"stdout,omitempty"`
	Stderr     string `json:"stderr,omitempty"`
	ExitCode   *int   `json:"exitCode,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	OutputPath string `json:"outputPath,omitempty"`
	StderrPath string `json:"stderrPath,omitempty"`
	// Running command (background job).
	JobID      string `json:"jobId,omitempty"`
	Status     string `json:"status,omitempty"`
	StdoutTail string `json:"stdoutTail,omitempty"`
	StderrTail string `json:"stderrTail,omitempty"`
	Notice     string `json:"notice,omitempty"`
	// Note is the AfterFailedCommand hook's line (e.g. a skill installed after the command failed without it).
	Note string `json:"note,omitempty"`
}

// jobShadow is the job's second record outside /workspace (pid, exit,
// killed): a command that deletes /workspace/.work (its own job directory)
// still gets its exit status, and the result says what happened (round 3, A11).
const jobShadow = `S="${HARNESS_JOBS_SHADOW_ROOT:-` + jobShadowRoot + `}/$(basename "$D")"
`

// jobShadowRoot holds the jobs' second records (reset with the sandbox, like
// the job; HARNESS_JOBS_SHADOW_ROOT in the sandbox's environment moves it, for tests).
const jobShadowRoot = "/tmp/work-jobs"

// jobStartScript starts HARNESS_JOB_CMD_B64 (base64 bash) detached in its own
// session under HARNESS_JOB_DIR (cmd.sh, pid, stdout, stderr, exit; pid and exit
// also in the shadow directory), then follows its output for up to
// HARNESS_JOB_WAIT seconds (live output of the call).
const jobStartScript = `# work:job-start
set -u
D="$HARNESS_JOB_DIR"
` + jobShadow + `mkdir -p "$D" "$S"
printf '%s' "$HARNESS_JOB_CMD_B64" | base64 -d > "$D/cmd.sh"
: > "$D/stdout"; : > "$D/stderr"
cd /workspace
setsid bash -c 'echo $$ > "$1/pid"; echo $$ > "$2/pid"; bash "$1/cmd.sh" > "$1/stdout" 2> "$1/stderr" < /dev/null; c=$?; echo $c > "$2/exit"; if [ -d "$1" ]; then echo $c > "$1/exit.tmp"; mv "$1/exit.tmp" "$1/exit"; fi' work-job "$D" "$S" > /dev/null 2>&1 < /dev/null &
` + jobFollow

// jobFollow streams the job's files until it exits or HARNESS_JOB_WAIT seconds pass.
const jobFollow = `W="${HARNESS_JOB_WAIT:-0}"
if [ "$W" -gt 0 ]; then
  tail -n +1 -F -s 0.2 "$D/stdout" 2>/dev/null & T1=$!
  tail -n +1 -F -s 0.2 "$D/stderr" 1>&2 2>/dev/null & T2=$!
  end=$((SECONDS + W))
  while [ ! -f "$D/exit" ] && [ ! -f "$D/killed" ] && [ ! -f "$S/exit" ] && [ ! -f "$S/killed" ] && [ "$SECONDS" -lt "$end" ]; do sleep 0.1; done
  sleep 0.4
  kill "$T1" "$T2" 2>/dev/null
  wait "$T1" "$T2" 2>/dev/null
fi
exit 0
`

const jobWaitScript = `# work:job-wait
set -u
D="$HARNESS_JOB_DIR"
` + jobShadow + `[ -d "$D" ] || [ -d "$S" ] || exit 0
` + jobFollow

// jobStatusScript prints "status S", "code N" and, per stream, "name size
// head-b64 tail-b64" ("-" for empty); HARNESS_JOB_CLEAN=1 removes the directory
// of a finished job whose output was returned whole. A job whose directory was
// deleted reports from its shadow: "removed" once it ended (with its exit
// code), "running" or "killed" before; "missing" when neither exists (the
// sandbox was recreated).
const jobStatusScript = `# work:job-status
set -u
D="$HARNESS_JOB_DIR"
` + jobShadow + `if [ ! -d "$D" ]; then
  if [ -f "$S/killed" ]; then echo "status killed"; echo "code "
  elif [ -f "$S/exit" ]; then echo "status removed"; echo "code $(cat "$S/exit")"
  elif [ -s "$S/pid" ] && kill -0 "$(cat "$S/pid")" 2>/dev/null; then echo "status running"; echo "code "
  elif [ -d "$S" ]; then echo "status removed"; echo "code "
  else echo "status missing"; exit 0; fi
  echo "stdout 0 - -"; echo "stderr 0 - -"
  exit 0
fi
st=running; code=""
if [ -f "$D/exit" ]; then st=finished; code=$(cat "$D/exit"); fi
if [ -f "$D/killed" ]; then st=killed; code=""; fi
if [ "$st" = running ] && [ -s "$D/pid" ] && ! kill -0 "$(cat "$D/pid")" 2>/dev/null; then
  sleep 0.2
  if [ -f "$D/exit" ]; then st=finished; code=$(cat "$D/exit"); else st=lost; fi
fi
echo "status $st"
echo "code $code"
H="$HARNESS_HEAD"; T="$HARNESS_TAIL"; small=1
for s in stdout stderr; do
  f="$D/$s"; n=$(stat -c %s "$f" 2>/dev/null || echo 0)
  if [ "$n" -le $((H + T)) ]; then
    h=$(base64 -w0 < "$f" 2>/dev/null); t=""
  else
    small=0; h=$(head -c "$H" "$f" | base64 -w0); t=$(tail -c "$T" "$f" | base64 -w0)
  fi
  echo "$s $n ${h:--} ${t:--}"
done
if [ "${HARNESS_JOB_CLEAN:-}" = 1 ] && [ "$st" = finished ] && [ "$small" = 1 ]; then rm -rf "$D" "$S"; fi
`

const jobKillScript = `# work:job-kill
set -u
D="$HARNESS_JOB_DIR"
` + jobShadow + `[ -d "$D" ] || [ -d "$S" ] || exit 1
[ -f "$D/exit" ] || [ -f "$S/exit" ] && exit 0
[ -d "$D" ] && touch "$D/killed"
[ -d "$S" ] && touch "$S/killed"
P=$(cat "$D/pid" 2>/dev/null || cat "$S/pid" 2>/dev/null || true)
if [ -n "$P" ]; then
  kill -TERM -- "-$P" 2>/dev/null || kill -TERM "$P" 2>/dev/null
  command -v pkill >/dev/null 2>&1 && pkill -TERM -s "$P" 2>/dev/null
  sleep 1
  kill -KILL -- "-$P" 2>/dev/null
  command -v pkill >/dev/null 2>&1 && pkill -KILL -s "$P" 2>/dev/null
fi
exit 0
`

// jobStatus is a parsed job-status report.
type jobStatus struct {
	status         string
	code           *int
	stdoutSize     int64
	stderrSize     int64
	stdoutH, stdoT string
	stderrH, stdeT string
}

func decode64(s string) string {
	if s == "-" || s == "" {
		return ""
	}
	b, _ := base64.StdEncoding.DecodeString(s)
	return string(b)
}

func parseStatus(text string) jobStatus {
	st := jobStatus{status: "missing"}
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "status":
			if len(f) > 1 {
				st.status = f[1]
			}
		case "code":
			if len(f) > 1 {
				if n, err := strconv.Atoi(f[1]); err == nil {
					st.code = &n
				}
			}
		case "stdout", "stderr":
			if len(f) < 4 {
				continue
			}
			n, _ := strconv.ParseInt(f[1], 10, 64)
			if f[0] == "stdout" {
				st.stdoutSize, st.stdoutH, st.stdoT = n, decode64(f[2]), decode64(f[3])
			} else {
				st.stderrSize, st.stderrH, st.stdeT = n, decode64(f[2]), decode64(f[3])
			}
		}
	}
	return st
}

// cut keeps the first head and last tail bytes of a stream of size bytes
// (h is its head, t its tail when it was longer than head+tail), on rune
// boundaries, with a marker naming the full output file.
func cut(h, t string, size int64, head, tail int, file string) (string, bool) {
	whole := t == "" && int64(len(h)) >= size
	if whole && len(h) <= head+tail {
		return h, false
	}
	if whole {
		t = h
	}
	if len(t) > tail {
		t = t[len(t)-tail:]
	}
	h = dropPartialRune(truncateUTF8(h, head))
	for len(t) > 0 && !utf8.RuneStart(t[0]) {
		t = t[1:]
	}
	omitted := size - int64(len(h)) - int64(len(t))
	return fmt.Sprintf("%s\n[... %d bytes omitted; the full output is in %s: page it with read_file or search_files ...]\n%s", h, omitted, file, t), true
}

// dropPartialRune removes an incomplete UTF-8 sequence at the end of s (a byte-cut head).
func dropPartialRune(s string) string {
	for i := 0; i < utf8.UTFMax && len(s) > 0; i++ {
		if r, size := utf8.DecodeLastRuneInString(s); r != utf8.RuneError || size > 1 {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}

func tailOf(h, t string, n int) string {
	s := h
	if t != "" {
		s = t
	}
	if len(s) > n {
		s = s[len(s)-n:]
		for len(s) > 0 && !utf8.RuneStart(s[0]) {
			s = s[1:]
		}
	}
	return s
}

func newJobID() string {
	return "job_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
}

func jobDir(id string) string { return jobsRoot + "/" + id }

func (b *binding) execute(ctx context.Context, in execInput) (*execOutput, error) {
	if strings.TrimSpace(in.Command) == "" {
		return nil, tools.Errorf("command is required")
	}
	timeout := b.p.opts.CommandTimeout
	if in.TimeoutSeconds > 0 {
		timeout = time.Duration(in.TimeoutSeconds) * time.Second
	}
	timeout = min(timeout, maxCommandTimeout)
	wait := int(timeout.Seconds())
	if in.Background {
		wait = 0
	}
	var out *execOutput
	err := b.withSandbox(ctx, func(sb sandbox.Sandbox) error {
		if err := b.beforeCommand(ctx, sb, in.Command); err != nil {
			return err
		}
		var err error
		if out, err = b.start(ctx, sb, in.Command, wait); err != nil {
			return err
		}
		if hook := b.p.opts.AfterFailedCommand; hook != nil && out.ExitCode != nil && *out.ExitCode != 0 {
			out.Note = hook(ctx, b.rc, sb, out.Stdout+"\n"+out.Stderr)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// beforeCommand runs the BeforeCommand hook: only a sandbox found gone (or a
// cancelled call) stops the command.
func (b *binding) beforeCommand(ctx context.Context, sb sandbox.Sandbox, command string) error {
	hook := b.p.opts.BeforeCommand
	if hook == nil {
		return nil
	}
	err := hook(ctx, b.rc, sb, command)
	if err == nil {
		return nil
	}
	if sandbox.IsGone(err) || ctx.Err() != nil {
		return err
	}
	slog.Warn("sandboxtools: preparing the command failed", "thread_id", b.rc.ThreadID, "error", err)
	return nil
}

// start runs a command as a job and follows it for wait seconds: a finished
// job gives its output, a running one its id and output tails.
func (b *binding) start(ctx context.Context, sb sandbox.Sandbox, command string, wait int) (*execOutput, error) {
	id := newJobID()
	dir := jobDir(id)
	live := newLiveOutput(ctx)
	env := map[string]string{"HARNESS_JOB_DIR": dir, "HARNESS_JOB_CMD_B64": base64.StdEncoding.EncodeToString([]byte(command)), "HARNESS_JOB_WAIT": strconv.Itoa(wait)}
	err := b.follow(ctx, sb, jobStartScript, env, wait, live)
	if err != nil {
		if ctx.Err() != nil && wait > 0 {
			// The run was stopped while it waited for its command: a foreground
			// command dies with it (only background jobs outlive their run).
			b.kill(ctx, sb, dir)
		}
		return nil, err
	}
	st, err := b.status(ctx, sb, dir, true)
	if err != nil {
		return nil, err
	}
	out := b.result(id, dir, st)
	if out.Status == "running" {
		// The job outlives this call: it keeps its sandbox alive until it ends (jobs.go).
		b.p.jobs.keep(ctx, b.p.opts, sb, b.rc.ThreadID, id, time.Time{}, func(ctx context.Context) (bool, error) {
			st, err := b.status(ctx, sb, dir, false)
			return st.status == "running", err
		})
	}
	return out, nil
}

// kill stops a job's process group (cancelled foreground command), with a
// context that survives the run's cancellation and a short bound.
func (b *binding) kill(ctx context.Context, sb sandbox.Sandbox, dir string) {
	kctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), killTimeout)
	defer cancel()
	if _, err := sb.Exec(kctx, sandbox.ExecRequest{Command: jobKillScript, Cwd: sandbox.Workspace, Timeout: killTimeout,
		Env: map[string]string{"HARNESS_JOB_DIR": dir}}); err != nil {
		slog.Warn("sandboxtools: killing a cancelled command failed", "thread_id", b.rc.ThreadID, "job_dir", dir, "error", err)
	}
}

// follow runs a start/wait script with the live output streamed.
func (b *binding) follow(ctx context.Context, sb sandbox.Sandbox, script string, env map[string]string, wait int, live *liveOutput) error {
	limit := time.Duration(wait)*time.Second + 30*time.Second
	cctx, cancel := context.WithTimeout(ctx, limit+5*time.Second)
	defer cancel()
	_, err := sb.Exec(cctx, sandbox.ExecRequest{Command: script, Cwd: sandbox.Workspace, Timeout: limit, Env: env,
		OnStdout: func(t string) { live.write("stdout", t) },
		OnStderr: func(t string) { live.write("stderr", t) },
	})
	live.close()
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case err == nil, cctx.Err() != nil:
		return nil // still running when the follow ended: reported by the status
	case sandbox.IsGone(err):
		return err
	}
	return fmt.Errorf("command failed to run: %w", err)
}

func (b *binding) status(ctx context.Context, sb sandbox.Sandbox, dir string, clean bool) (jobStatus, error) {
	var stdout strings.Builder
	env := map[string]string{"HARNESS_JOB_DIR": dir, "HARNESS_HEAD": strconv.Itoa(stdoutHead), "HARNESS_TAIL": strconv.Itoa(stdoutTail)}
	if clean {
		env["HARNESS_JOB_CLEAN"] = "1"
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), statusTimeout)
	defer cancel()
	if _, err := sb.Exec(cctx, sandbox.ExecRequest{Command: jobStatusScript, Cwd: sandbox.Workspace, Timeout: statusTimeout, Env: env,
		OnStdout: func(t string) { stdout.WriteString(t) }}); err != nil {
		if sandbox.IsGone(err) {
			return jobStatus{}, err
		}
		return jobStatus{}, fmt.Errorf("reading the job status failed: %w", err)
	}
	return parseStatus(stdout.String()), nil
}

// result is the execute_command result of a job.
func (b *binding) result(id, dir string, st jobStatus) *execOutput {
	switch st.status {
	case "running":
		return &execOutput{JobID: id, Status: "running", OutputPath: dir + "/stdout",
			StdoutTail: tailOf(st.stdoutH, st.stdoT, runningTail), StderrTail: tailOf(st.stderrH, st.stdeT, runningTail/2)}
	case "missing", "lost":
		code := -1
		return &execOutput{ExitCode: &code, Stderr: "[the command's process ended without an exit status (the sandbox may have restarted)]"}
	case "removed":
		code := -1
		if st.code != nil {
			code = *st.code
		}
		return &execOutput{ExitCode: &code, Stderr: fmt.Sprintf(RemovedJobNote, dir, code)}
	}
	out := &execOutput{ExitCode: st.code}
	if out.ExitCode == nil {
		code := -1
		out.ExitCode = &code
	}
	var cutOut, cutErr bool
	out.Stdout, cutOut = cut(st.stdoutH, st.stdoT, st.stdoutSize, stdoutHead, stdoutTail, dir+"/stdout")
	out.Stderr, cutErr = cut(st.stderrH, st.stdeT, st.stderrSize, stderrHead, stderrTail, dir+"/stderr")
	if cutOut || cutErr {
		out.Truncated, out.OutputPath = true, dir+"/stdout"
		if cutErr {
			out.StderrPath = dir + "/stderr"
		}
	}
	return out
}

// ---- job_output / job_kill ----

type jobOutputInput struct {
	JobID       string `json:"jobId" jsonschema_description:"The jobId returned by execute_command"`
	WaitSeconds int    `json:"waitSeconds,omitempty" jsonschema_description:"Wait up to this many seconds for the job to finish (default 0, max 300)"`
}

type jobOutput struct {
	JobID      string `json:"jobId"`
	Status     string `json:"status"`
	ExitCode   *int   `json:"exitCode,omitempty"`
	StdoutTail string `json:"stdoutTail"`
	StderrTail string `json:"stderrTail"`
	OutputPath string `json:"outputPath"`
}

func (b *binding) jobSandbox(ctx context.Context, id string) (sandbox.Sandbox, error) {
	if !jobIDPattern.MatchString(id) {
		return nil, tools.Errorf("unknown job %q; use the jobId returned by execute_command", id)
	}
	sb, err := b.p.mgr.Get(ctx, b.rc.ThreadID)
	if errors.Is(err, sandbox.ErrNoSandbox) {
		return nil, tools.Errorf("job %s is gone: the sandbox stopped since it started", id)
	}
	if err != nil {
		return nil, tools.Errorf("the sandbox is temporarily unavailable; retry the tool call")
	}
	return sb, nil
}

func (b *binding) jobOutput(ctx context.Context, in jobOutputInput) (*jobOutput, error) {
	id := strings.TrimSpace(in.JobID)
	sb, err := b.jobSandbox(ctx, id)
	if err != nil {
		return nil, err
	}
	dir := jobDir(id)
	if wait := min(max(in.WaitSeconds, 0), maxJobWait); wait > 0 {
		live := newLiveOutput(ctx)
		err := b.follow(ctx, sb, jobWaitScript, map[string]string{"HARNESS_JOB_DIR": dir, "HARNESS_JOB_WAIT": strconv.Itoa(wait)}, wait, live)
		if err != nil {
			return nil, err
		}
	}
	st, err := b.status(ctx, sb, dir, false)
	if err != nil {
		return nil, err
	}
	if st.status != "running" {
		b.p.jobs.release(b.rc.ThreadID, id)
	}
	switch st.status {
	case "missing":
		return nil, tools.Errorf("unknown job %q (the sandbox may have been recreated since it started)", id)
	case "lost":
		st.status = "finished"
		code := -1
		st.code = &code
	case "removed":
		st.status = "finished"
		if st.code == nil {
			code := -1
			st.code = &code
		}
		st.stderrH = fmt.Sprintf(RemovedJobNote, dir, *st.code)
	}
	return &jobOutput{JobID: id, Status: st.status, ExitCode: st.code, OutputPath: dir + "/stdout",
		StdoutTail: tailOf(st.stdoutH, st.stdoT, stdoutTail), StderrTail: tailOf(st.stderrH, st.stdeT, stderrTail)}, nil
}

type jobKillInput struct {
	JobID string `json:"jobId" jsonschema_description:"The jobId returned by execute_command"`
}

type jobKilled struct {
	JobID  string `json:"jobId"`
	Status string `json:"status"`
}

func (b *binding) jobKill(ctx context.Context, in jobKillInput) (*jobKilled, error) {
	id := strings.TrimSpace(in.JobID)
	sb, err := b.jobSandbox(ctx, id)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	res, err := sb.Exec(cctx, sandbox.ExecRequest{Command: jobKillScript, Cwd: sandbox.Workspace, Timeout: statusTimeout,
		Env: map[string]string{"HARNESS_JOB_DIR": jobDir(id)}})
	if err != nil {
		return nil, fmt.Errorf("stopping the job failed: %w", err)
	}
	if res.ExitCode != 0 {
		return nil, tools.Errorf("unknown job %q", id)
	}
	b.p.jobs.release(b.rc.ThreadID, id)
	st, err := b.status(ctx, sb, jobDir(id), false)
	if err == nil && st.status == "finished" {
		return &jobKilled{JobID: id, Status: "finished"}, nil
	}
	return &jobKilled{JobID: id, Status: "killed"}, nil
}

// ---- live output ----

// liveOutput coalesces live output into CUSTOM work.tool_output events.
type liveOutput struct {
	mu         sync.Mutex
	emitter    tools.Emitter
	toolCallID string
	pending    map[string]*strings.Builder
	order      []string
	sent       int
	capped     bool
	timer      *time.Timer
	closed     bool
}

func newLiveOutput(ctx context.Context) *liveOutput {
	id, em := tools.CallFromContext(ctx)
	if em == nil {
		return nil
	}
	return &liveOutput{emitter: em, toolCallID: id, pending: map[string]*strings.Builder{}}
}

func (l *liveOutput) write(stream, text string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.capped {
		return
	}
	b := l.pending[stream]
	if b == nil {
		b = &strings.Builder{}
		l.pending[stream] = b
		l.order = append(l.order, stream)
	}
	b.WriteString(text)
	if l.timer == nil {
		l.timer = time.AfterFunc(coalesceEvery, l.flush)
	}
}

func (l *liveOutput) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.flushLocked()
}

func (l *liveOutput) flushLocked() {
	l.timer = nil
	for _, stream := range l.order {
		b := l.pending[stream]
		if b == nil || b.Len() == 0 || l.capped {
			continue
		}
		text := b.String()
		b.Reset()
		room := maxLiveOutput - l.sent
		if len(text) >= room {
			text = truncateUTF8(text, room) + "\n[live output truncated; the full output is in the tool result]\n"
			l.capped = true
		}
		l.sent += len(text)
		l.emitter.ToolOutput(l.toolCallID, stream, text)
	}
}

func (l *liveOutput) close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.timer != nil {
		l.timer.Stop()
	}
	if !l.closed {
		l.flushLocked()
		l.closed = true
	}
}

func truncateUTF8(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
