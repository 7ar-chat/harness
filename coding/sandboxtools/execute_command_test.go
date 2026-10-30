package sandboxtools_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/tool"

	"github.com/GhaziBenDahmane/harness/coding/sandboxtools"
	"github.com/GhaziBenDahmane/harness/core/sandbox"
	"github.com/GhaziBenDahmane/harness/core/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/engine/tools"
)

type execResult struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   *int   `json:"exitCode"`
	Truncated  bool   `json:"truncated"`
	OutputPath string `json:"outputPath"`
	StderrPath string `json:"stderrPath"`
	JobID      string `json:"jobId"`
	Status     string `json:"status"`
	StdoutTail string `json:"stdoutTail"`
	StderrTail string `json:"stderrTail"`
}

func decodeExec(t *testing.T, out string) execResult {
	t.Helper()
	var r execResult
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("result %q: %v", out, err)
	}
	return r
}

func TestExecuteCommandReturnsOutputAndExitCode(t *testing.T) {
	// execute_command runs in /workspace and reports stdout, stderr and a non-zero exit code without failing.
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.SetExec(func(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
			req.OnStdout("out\n")
			req.OnStderr("warn\n")
			return sandbox.ExecResult{ExitCode: 3}, nil
		})
	})

	// Act
	out, err := f.call(context.Background(), "execute_command", `{"command":"make"}`)

	// Assert
	r := decodeExec(t, out)
	if err != nil || r.Stdout != "out\n" || r.Stderr != "warn\n" || r.ExitCode == nil || *r.ExitCode != 3 || r.Truncated || r.JobID != "" {
		t.Fatalf("err %v out %s", err, out)
	}
	if e := f.sb.Execs()[0]; e.Cwd != "/workspace" || e.Command != "make" {
		t.Fatalf("exec %+v", e)
	}
}

func TestExecuteCommandStreamsCoalescedLiveOutput(t *testing.T) {
	// Live output is emitted through the run emitter, coalesced, tagged with the tool call id.
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.SetExec(func(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
			for i := 0; i < 50; i++ {
				req.OnStdout("x\n")
			}
			return sandbox.ExecResult{}, nil
		})
	})
	rec := &recorder{}
	ctx := tools.WithCall(context.Background(), "call-9", rec)

	// Act
	_, err := f.call(ctx, "execute_command", `{"command":"seq"}`)

	// Assert
	joined := strings.Join(rec.chunks, "")
	if err != nil || len(rec.chunks) == 0 || len(rec.chunks) >= 50 || strings.Count(joined, "x\n") != 50 || rec.ids[0] != "call-9" {
		t.Fatalf("err %v chunks %d", err, len(rec.chunks))
	}
}

func TestExecuteCommandKeepsHeadAndTailOfLargeOutput(t *testing.T) {
	// Large output keeps its first 4,000 and last 8,000 bytes with a marker, is flagged truncated and names the file holding all of it; live output stops at 256 KiB.
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.SetExec(func(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
			req.OnStdout("FIRST\n")
			line := strings.Repeat("y", 1023) + "\n"
			for i := 0; i < 1500; i++ {
				req.OnStdout(line)
			}
			req.OnStdout("LAST\n")
			return sandbox.ExecResult{}, nil
		})
	})
	rec := &recorder{}

	// Act
	out, _ := f.call(tools.WithCall(context.Background(), "c", rec), "execute_command", `{"command":"yes"}`)

	// Assert
	r := decodeExec(t, out)
	live := len(strings.Join(rec.chunks, ""))
	if !r.Truncated || !strings.HasPrefix(r.Stdout, "FIRST\n") || !strings.HasSuffix(r.Stdout, "LAST\n") || len(r.Stdout) > 12300 ||
		!strings.Contains(r.Stdout, "bytes omitted; the full output is in "+r.OutputPath) || !strings.HasPrefix(r.OutputPath, "/workspace/.work/jobs/job_") || live > 256<<10+200 {
		t.Fatalf("truncated %v stdout %d path %q live %d", r.Truncated, len(r.Stdout), r.OutputPath, live)
	}
	full, ok := f.sb.File(r.OutputPath)
	if !ok || len(full) != 6+1500*1024+5 {
		t.Fatalf("full output file %d bytes (%v)", len(full), ok)
	}
}

func TestExecuteCommandCapCutsOnRuneBoundary(t *testing.T) {
	// The head/tail cut never splits a multi-byte character.
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.SetExec(func(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
			req.OnStdout(strings.Repeat("a", 3999) + "é" + strings.Repeat("b", 20000) + "é" + strings.Repeat("c", 7999))
			return sandbox.ExecResult{}, nil
		})
	})

	// Act
	out, _ := f.call(context.Background(), "execute_command", `{"command":"x"}`)

	// Assert
	r := decodeExec(t, out)
	if !r.Truncated || strings.ContainsRune(r.Stdout, utf8.RuneError) || !utf8.ValidString(r.Stdout) {
		t.Fatalf("truncated %v invalid utf8 in %q", r.Truncated, r.Stdout[3990:4010])
	}
}

// blockingJob is a job that writes "started" then runs until released or killed.
func blockingJob(release <-chan struct{}) sandboxtest.JobFunc {
	return func(ctx context.Context, command string, out func(stream, text string)) int {
		out("stdout", "started\n")
		select {
		case <-release:
			out("stdout", "done\n")
			return 0
		case <-ctx.Done():
			return 143
		}
	}
}

func TestExecuteCommandStillRunningAtTimeoutBecomesABackgroundJob(t *testing.T) {
	// A command still running at timeoutSeconds is not killed: the result is a running job with its output tail, and job_output later reports it finished.
	// Arrange
	release := make(chan struct{})
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.SetJobs(blockingJob(release)) })

	// Act
	start := time.Now()
	out, err := f.call(context.Background(), "execute_command", `{"command":"soffice --convert-to pdf big.docx","timeoutSeconds":1}`)
	r := decodeExec(t, out)
	close(release)
	later, lerr := f.call(context.Background(), "job_output", `{"jobId":"`+r.JobID+`","waitSeconds":5}`)

	// Assert
	if err != nil || r.Status != "running" || !strings.HasPrefix(r.JobID, "job_") || r.StdoutTail != "started\n" || r.ExitCode != nil || time.Since(start) > 4*time.Second {
		t.Fatalf("err %v out %s", err, out)
	}
	var j map[string]any
	_ = json.Unmarshal([]byte(later), &j)
	if lerr != nil || j["status"] != "finished" || j["exitCode"] != float64(0) || j["stdoutTail"] != "started\ndone\n" || j["outputPath"] != "/workspace/.work/jobs/"+r.JobID+"/stdout" {
		t.Fatalf("job_output err %v out %s", lerr, later)
	}
}

func TestExecuteCommandInBackgroundReturnsAtOnceAndJobKillStopsIt(t *testing.T) {
	// background:true returns the job id immediately; job_kill stops the job and job_output then reports it killed.
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.SetJobs(blockingJob(make(chan struct{}))) })

	// Act
	out, err := f.call(context.Background(), "execute_command", `{"command":"python3 server.py","background":true}`)
	r := decodeExec(t, out)
	killed, kerr := f.call(context.Background(), "job_kill", `{"jobId":"`+r.JobID+`"}`)
	after, _ := f.call(context.Background(), "job_output", `{"jobId":"`+r.JobID+`"}`)

	// Assert
	if err != nil || r.Status != "running" || r.JobID == "" {
		t.Fatalf("err %v out %s", err, out)
	}
	if kerr != nil || killed != `{"jobId":"`+r.JobID+`","status":"killed"}` || !strings.Contains(after, `"status":"killed"`) {
		t.Fatalf("kill err %v %s; after %s", kerr, killed, after)
	}
}

func TestJobOutputRejectsUnknownJobs(t *testing.T) {
	// job_output of an id that is not a job of this sandbox is a tool error.
	// Arrange
	f := newFixture(t, nil)
	if _, err := f.call(context.Background(), "execute_command", `{"command":"true"}`); err != nil {
		t.Fatal(err)
	}

	// Act
	_, bad := f.call(context.Background(), "job_output", `{"jobId":"../../etc"}`)
	_, missing := f.call(context.Background(), "job_output", `{"jobId":"job_0123456789ab"}`)

	// Assert
	if bad == nil || missing == nil || !strings.Contains(missing.Error(), "unknown job") {
		t.Fatalf("bad %v missing %v", bad, missing)
	}
}

func TestExecuteCommandRequiresCommand(t *testing.T) {
	// An empty command is a tool error and no sandbox is created.
	// Arrange
	f := newFixture(t, nil)

	// Act
	_, err := f.call(context.Background(), "execute_command", `{"command":" "}`)

	// Assert
	if err == nil || len(f.backend.Created()) != 0 {
		t.Fatalf("err %v created %d", err, len(f.backend.Created()))
	}
}

func TestBackgroundJobsRunForRealInAShell(t *testing.T) {
	// With real bash: a command outliving its timeout keeps running detached, job_output waits for its exit code and output, job_kill ends a job's whole process group.
	// Arrange
	requireGNUTools(t)
	f, _ := newHostFixture(t, nil)

	// Act
	out, err := f.call(context.Background(), "execute_command", `{"command":"echo begin; sleep 2; echo end; exit 4","timeoutSeconds":1}`)
	r := decodeExec(t, out)
	waited, werr := f.call(context.Background(), "job_output", `{"jobId":"`+r.JobID+`","waitSeconds":10}`)
	long, _ := f.call(context.Background(), "execute_command", `{"command":"sleep 60 & sleep 60; echo never","background":true}`)
	lj := decodeExec(t, long)
	killed, kerr := f.call(context.Background(), "job_kill", `{"jobId":"`+lj.JobID+`"}`)
	status, _ := f.call(context.Background(), "job_output", `{"jobId":"`+lj.JobID+`","waitSeconds":2}`)
	quick, qerr := f.call(context.Background(), "execute_command", `{"command":"printf 'a\\nb\\n'; echo oops >&2; exit 2"}`)

	// Assert
	if err != nil || r.Status != "running" || !strings.Contains(r.StdoutTail, "begin") {
		t.Fatalf("err %v out %s", err, out)
	}
	var w map[string]any
	_ = json.Unmarshal([]byte(waited), &w)
	if werr != nil || w["status"] != "finished" || w["exitCode"] != float64(4) || w["stdoutTail"] != "begin\nend\n" {
		t.Fatalf("job_output err %v out %s", werr, waited)
	}
	if kerr != nil || !strings.Contains(killed, `"status":"killed"`) || !strings.Contains(status, `"status":"killed"`) || strings.Contains(status, "never") {
		t.Fatalf("kill err %v %s; status %s", kerr, killed, status)
	}
	q := decodeExec(t, quick)
	if qerr != nil || q.Stdout != "a\nb\n" || q.Stderr != "oops\n" || q.ExitCode == nil || *q.ExitCode != 2 || q.JobID != "" {
		t.Fatalf("quick err %v out %s", qerr, quick)
	}
}

func TestLargeOutputIsPagedFromItsFileForReal(t *testing.T) {
	// With real bash: a 300 KB output is cut in the result and its outputPath can be paged with read_file.
	// Arrange
	requireGNUTools(t)
	f, _ := newHostFixture(t, nil)

	// Act
	out, err := f.call(context.Background(), "execute_command", `{"command":"seq 1 60000"}`)
	r := decodeExec(t, out)
	page, perr := f.call(context.Background(), "read_file", `{"path":"`+r.OutputPath+`","offset":30000,"limit":2}`)

	// Assert
	if err != nil || !r.Truncated || !strings.HasPrefix(r.Stdout, "1\n2\n") || !strings.HasSuffix(r.Stdout, "59999\n60000\n") {
		t.Fatalf("err %v out %.300s", err, out)
	}
	if perr != nil || page != " 30000\t30000\n 30001\t30001\n[Showing lines 30000-30001 of 60000. Use offset=30002 to read more.]\n" {
		t.Fatalf("page err %v %q", perr, page)
	}
}

type recreatedManager struct{ sb sandbox.Sandbox }

func (m recreatedManager) Enabled() bool { return true }
func (m recreatedManager) Acquire(context.Context, string, string) (sandbox.Sandbox, error) {
	return sandbox.MarkRecreated(m.sb), nil
}
func (m recreatedManager) Get(context.Context, string) (sandbox.Sandbox, error) { return m.sb, nil }
func (m recreatedManager) Kill(context.Context, string) error                   { return nil }
func (m recreatedManager) Invalidate(context.Context, string, string)           {}

func TestToolsTellTheModelWhenTheSandboxWasRecreated(t *testing.T) {
	// When the sandbox had to be recreated, the tool result says /workspace was reset.
	// Arrange
	fresh := sandboxtest.NewSandbox("sbx-new")
	_ = fresh.MkdirAll(context.Background(), "/workspace")
	p := sandboxtools.New(recreatedManager{sb: fresh}, sandboxtools.Options{})
	ts, _, _ := p.Tools(context.Background(), tools.RunContext{ThreadID: "t", Login: "alice"})
	byName := map[string]tool.InvokableTool{}
	for _, x := range ts {
		info, _ := x.Info(context.Background())
		byName[info.Name] = x.(tool.InvokableTool)
	}

	// Act
	execOut, _ := byName["execute_command"].InvokableRun(context.Background(), `{"command":"ls"}`)
	listOut, _ := byName["list_files"].InvokableRun(context.Background(), `{}`)
	_, readErr := byName["read_file"].InvokableRun(context.Background(), `{"path":"old.txt"}`)

	// Assert
	for _, out := range []string{execOut, listOut, readErr.Error()} {
		if !strings.Contains(out, "recreated") || !strings.Contains(out, "/workspace") {
			t.Fatalf("no notice in %q", out)
		}
	}
}

func TestFirstCommandCanWriteToTheAdvertisedScratchAndOutputFolders(t *testing.T) {
	// With real bash: on a brand-new sandbox the very first command can write to /workspace/outputs/ and
	// /workspace/.work/ (the baseline's `cat > /workspace/.work/x.py` failed), and the spill and render folders exist.
	// Arrange
	requireGNUTools(t)
	f, _ := newHostFixture(t, nil)
	// Relative paths: the command runs in /workspace (the host sandbox maps only the working directory).
	cmd := `cat > .work/probe.py <<'PY'
print('ok')
PY
printf 'done\n' > outputs/probe.txt && ls -d .work/tool-output .work/renders >/dev/null && cat outputs/probe.txt`
	args, _ := json.Marshal(map[string]string{"command": cmd})

	// Act
	out, err := f.call(context.Background(), "execute_command", string(args))

	// Assert
	r := decodeExec(t, out)
	if err != nil || r.ExitCode == nil || *r.ExitCode != 0 || r.Stdout != "done\n" {
		t.Fatalf("err %v out %s", err, out)
	}
}
