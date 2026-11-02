package sandboxtools_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/7ar-chat/harness/coding/sandboxtools"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/engine/tools"
)

func TestACommandThatDeletesItsOwnJobFilesStillReportsItsExitCode(t *testing.T) {
	// Arrange: the command removes /workspace/.work, with its own job directory, then exits 3.
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.SetJobs(func(_ context.Context, _ string, out func(string, string)) int {
			out("stdout", "unpacked\n")
			sb.RemoveTree("/workspace/.work")
			return 3
		})
	})

	// Act
	out, err := f.call(context.Background(), "execute_command", `{"command":"rm -rf /workspace/.work","timeoutSeconds":5}`)

	// Assert: the exit code survives and the model is told why the output is gone.
	r := decodeExec(t, out)
	if err != nil || r.ExitCode == nil || *r.ExitCode != 3 || !strings.Contains(r.Stderr, "deleted its own output files") || r.Stdout != "" {
		t.Fatalf("err %v out %s", err, out)
	}
}

func TestJobOutputOfAJobThatDeletedItsFilesReportsItFinishedWithTheNote(t *testing.T) {
	// Arrange
	release := make(chan struct{})
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.SetJobs(func(_ context.Context, _ string, _ func(string, string)) int {
			<-release
			sb.RemoveTree("/workspace/.work")
			return 4
		})
	})
	out, _ := f.call(context.Background(), "execute_command", `{"command":"x","background":true}`)
	id := decodeExec(t, out).JobID

	// Act
	running, _ := f.call(context.Background(), "job_output", `{"jobId":"`+id+`"}`)
	close(release)
	f.sb.RemoveTree("/workspace/.work")
	done, err := f.call(context.Background(), "job_output", `{"jobId":"`+id+`","waitSeconds":5}`)

	// Assert
	m := decodeMap(t, done)
	if err != nil || decodeMap(t, running)["status"] != "running" || m["status"] != "finished" || m["exitCode"] != float64(4) ||
		!strings.Contains(m["stderrTail"].(string), "deleted its own output files") {
		t.Fatalf("err %v running %s done %s", err, running, done)
	}
}

func TestJobToolsExplainAMissingSandboxAndBadJobIds(t *testing.T) {
	cases := map[string]struct {
		get  error
		tool string
		args string
		want string
	}{
		"no sandbox, output": {sandbox.ErrNoSandbox, "job_output", `{"jobId":"job_0123456789ab"}`, "is gone: the sandbox stopped"},
		"no sandbox, kill":   {sandbox.ErrNoSandbox, "job_kill", `{"jobId":"job_0123456789ab"}`, "is gone: the sandbox stopped"},
		"unavailable":        {errors.New("redis down"), "job_output", `{"jobId":"job_0123456789ab"}`, "temporarily unavailable"},
		"bad id, output":     {nil, "job_output", `{"jobId":"../x"}`, `unknown job "../x"`},
		"bad id, kill":       {nil, "job_kill", `{"jobId":"job_UPPER"}`, `unknown job "job_UPPER"`},
		"blank id":           {nil, "job_kill", `{"jobId":"  "}`, "unknown job"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			// Arrange
			p := sandboxtools.New(stubManager{get: func(context.Context, string) (sandbox.Sandbox, error) { return nil, c.get }}, sandboxtools.Options{})
			ts := toolsOf(t, p, tools.RunContext{ThreadID: "t", Login: "alice"})

			// Act
			_, err := ts[c.tool].InvokableRun(context.Background(), c.args)

			// Assert
			var ue *tools.UserError
			if err == nil || !strings.Contains(err.Error(), c.want) || !errors.As(err, &ue) {
				t.Fatalf("err %v", err)
			}
		})
	}
}

func TestJobKillOfAnUnknownJobIsAnError(t *testing.T) {
	// Arrange
	f := newFixture(t, nil)
	_, _ = f.call(context.Background(), "execute_command", `{"command":"true"}`)

	// Act
	_, err := f.call(context.Background(), "job_kill", `{"jobId":"job_0123456789ab"}`)

	// Assert
	if err == nil || !strings.Contains(err.Error(), `unknown job "job_0123456789ab"`) {
		t.Fatalf("err %v", err)
	}
}

func TestJobKillOfAJobThatAlreadyFinishedSaysSo(t *testing.T) {
	// Arrange
	release := make(chan struct{})
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.SetJobs(blockingJob(release)) })
	out, _ := f.call(context.Background(), "execute_command", `{"command":"x","background":true}`)
	id := decodeExec(t, out).JobID
	close(release)
	_, _ = f.call(context.Background(), "job_output", `{"jobId":"`+id+`","waitSeconds":5}`)

	// Act
	killed, err := f.call(context.Background(), "job_kill", `{"jobId":"`+id+`"}`)

	// Assert
	if err != nil || killed != `{"jobId":"`+id+`","status":"finished"}` {
		t.Fatalf("err %v out %s", err, killed)
	}
}

func TestScriptFailuresAreReportedPerStep(t *testing.T) {
	cases := map[string]struct {
		marker string
		tool   string
		args   string
		want   string
	}{
		"start":  {"# work:job-start", "execute_command", `{"command":"x"}`, "command failed to run: boom"},
		"status": {"# work:job-status", "execute_command", `{"command":"x"}`, "reading the job status failed: boom"},
		"kill":   {"# work:job-kill", "job_kill", `{"jobId":"JOB"}`, "stopping the job failed: boom"},
		"wait":   {"# work:job-wait", "job_output", `{"jobId":"JOB","waitSeconds":1}`, "command failed to run: boom"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			// Arrange
			release := make(chan struct{})
			f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.SetJobs(blockingJob(release)) })
			t.Cleanup(func() { close(release) })
			started, _ := f.call(context.Background(), "execute_command", `{"command":"x","background":true}`)
			id := decodeExec(t, started).JobID
			f.sb.FailScript(c.marker, errors.New("boom"))

			// Act
			var err error
			if c.tool == "execute_command" {
				_, err = f.call(context.Background(), c.tool, c.args)
			} else {
				_, err = f.call(context.Background(), c.tool, strings.ReplaceAll(c.args, "JOB", id))
			}

			// Assert
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err %v, want %q", err, c.want)
			}
		})
	}
}

func TestACommandThatFailsRunsTheAfterFailedCommandHook(t *testing.T) {
	// Arrange
	var seen []string
	f := newFixtureWith(t, func(sb *sandboxtest.Sandbox) {
		sb.SetExec(func(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
			req.OnStdout("partial\n")
			req.OnStderr("ModuleNotFoundError: xlsx\n")
			if strings.Contains(req.Command, "ok") {
				return sandbox.ExecResult{}, nil
			}
			return sandbox.ExecResult{ExitCode: 1}, nil
		})
	}, func(o *sandboxtools.Options, _ *sandbox.ManagerOptions) {
		o.AfterFailedCommand = func(_ context.Context, _ tools.RunContext, _ sandbox.Sandbox, output string) string {
			seen = append(seen, output)
			return "Skill xlsx was installed; run the command again."
		}
	})

	// Act
	failed, err1 := f.call(context.Background(), "execute_command", `{"command":"python3 a.py"}`)
	passed, err2 := f.call(context.Background(), "execute_command", `{"command":"ok"}`)

	// Assert: the hook sees stdout then stderr of the failed command only, and its line becomes the note.
	if err1 != nil || err2 != nil || decodeMap(t, failed)["note"] != "Skill xlsx was installed; run the command again." || decodeMap(t, passed)["note"] != nil {
		t.Fatalf("failed %s passed %s errs %v %v", failed, passed, err1, err2)
	}
	if len(seen) != 1 || seen[0] != "partial\n\nModuleNotFoundError: xlsx\n" {
		t.Fatalf("hook outputs %q", seen)
	}
}

func TestBeforeCommandFailuresOnlyStopTheCommandWhenTheSandboxIsGone(t *testing.T) {
	// Arrange
	var calls atomic.Int32
	f := newFixtureWith(t, nil, func(o *sandboxtools.Options, _ *sandbox.ManagerOptions) {
		o.BeforeCommand = func(_ context.Context, _ tools.RunContext, _ sandbox.Sandbox, command string) error {
			switch {
			case strings.Contains(command, "flaky"):
				return errors.New("skill install failed")
			case strings.Contains(command, "gone") && calls.Add(1) == 1:
				return sandbox.ErrGone
			}
			return nil
		}
	})

	// Act
	flaky, err1 := f.call(context.Background(), "execute_command", `{"command":"flaky"}`)
	gone, err2 := f.call(context.Background(), "execute_command", `{"command":"gone"}`)

	// Assert: a hook failure is only logged; a gone sandbox makes the call retry once (and run the hook again).
	if err1 != nil || *decodeExec(t, flaky).ExitCode != 0 {
		t.Fatalf("flaky %s err %v", flaky, err1)
	}
	if err2 != nil || *decodeExec(t, gone).ExitCode != 0 || calls.Load() != 2 {
		t.Fatalf("gone %s err %v hook calls %d", gone, err2, calls.Load())
	}
	ran := 0
	for _, e := range f.sb.Execs() {
		if e.Command == "gone" {
			ran++
		}
	}
	if ran != 1 {
		t.Fatalf("command ran %d times", ran)
	}
}

func TestACancelledBeforeCommandHookStopsTheCommand(t *testing.T) {
	// Arrange
	ctx, cancel := context.WithCancel(context.Background())
	f := newFixtureWith(t, nil, func(o *sandboxtools.Options, _ *sandbox.ManagerOptions) {
		o.BeforeCommand = func(context.Context, tools.RunContext, sandbox.Sandbox, string) error {
			cancel()
			return errors.New("interrupted")
		}
	})

	// Act
	_, err := f.call(ctx, "execute_command", `{"command":"never"}`)

	// Assert
	if err == nil || len(f.sb.Execs()) != 0 {
		t.Fatalf("err %v execs %d", err, len(f.sb.Execs()))
	}
}

func TestExecuteCommandCutsALongStderrAndNamesBothFiles(t *testing.T) {
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.SetExec(func(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
			req.OnStdout("short\n")
			req.OnStderr(strings.Repeat("e", 1000) + strings.Repeat("m", 5000) + strings.Repeat("z", 3000))
			return sandbox.ExecResult{ExitCode: 2}, nil
		})
	})

	// Act
	out, err := f.call(context.Background(), "execute_command", `{"command":"x"}`)

	// Assert
	r := decodeExec(t, out)
	if err != nil || !r.Truncated || r.Stdout != "short\n" || !strings.HasPrefix(r.Stderr, strings.Repeat("e", 1000)) ||
		!strings.HasSuffix(r.Stderr, strings.Repeat("z", 3000)) || !strings.Contains(r.Stderr, "[... 5000 bytes omitted;") ||
		r.OutputPath != r.StderrPath[:len(r.StderrPath)-len("stderr")]+"stdout" || !strings.HasSuffix(r.StderrPath, "/stderr") {
		t.Fatalf("err %v out %.300s paths %q %q", err, out, r.OutputPath, r.StderrPath)
	}
}

func TestARunningJobShowsOnlyTheEndOfItsOutputOnRuneBoundaries(t *testing.T) {
	// Arrange: more output than the running tail, in 3-byte characters so a byte cut lands inside one.
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.SetJobs(func(ctx context.Context, _ string, out func(string, string)) int {
			out("stdout", strings.Repeat("x", 7)+strings.Repeat("€", 5000)+"END")
			out("stderr", strings.Repeat("!", 20000))
			<-ctx.Done()
			return 143
		})
	})

	// Act
	out, err := f.call(context.Background(), "execute_command", `{"command":"x","timeoutSeconds":1}`)

	// Assert
	r := decodeExec(t, out)
	if err != nil || r.Status != "running" || !utf8.ValidString(r.StdoutTail) || !strings.HasSuffix(r.StdoutTail, "€END") || len(r.StdoutTail) > 4000 ||
		strings.HasPrefix(r.StdoutTail, "x") || len(r.StderrTail) != 2000 {
		t.Fatalf("err %v stdoutTail %d bytes valid %v stderrTail %d", err, len(r.StdoutTail), utf8.ValidString(r.StdoutTail), len(r.StderrTail))
	}
}

func TestJobOutputKeepsTheEndOfALargeOutput(t *testing.T) {
	// Arrange
	release := make(chan struct{})
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.SetJobs(func(_ context.Context, _ string, out func(string, string)) int {
			out("stdout", strings.Repeat("a", 5000)+strings.Repeat("b", 20000)+"THE END")
			<-release
			return 0
		})
	})
	started, _ := f.call(context.Background(), "execute_command", `{"command":"x","background":true}`)
	id := decodeExec(t, started).JobID
	close(release)

	// Act
	out, err := f.call(context.Background(), "job_output", `{"jobId":"`+id+`","waitSeconds":5}`)

	// Assert
	m := decodeMap(t, out)
	tail := m["stdoutTail"].(string)
	if err != nil || m["status"] != "finished" || len(tail) != 8000 || !strings.HasSuffix(tail, "THE END") {
		t.Fatalf("err %v status %v tail %d bytes", err, m["status"], len(tail))
	}
}

func TestLiveOutputIsCappedAndStaysValidUTF8(t *testing.T) {
	// Arrange: far more live output than the per-call cap, in multi-byte characters.
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.SetExec(func(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
			for i := 0; i < 30; i++ {
				req.OnStdout(strings.Repeat("é", 5000) + "\n")
			}
			req.OnStderr("late\n")
			return sandbox.ExecResult{}, nil
		})
	})
	rec := &recorder{}

	// Act
	out, err := f.call(tools.WithCall(context.Background(), "c1", rec), "execute_command", `{"command":"x"}`)

	// Assert
	total := 0
	for _, c := range rec.chunks {
		total += len(c)
		if !utf8.ValidString(c) {
			t.Fatalf("invalid UTF-8 in a live chunk")
		}
	}
	joined := strings.Join(rec.chunks, "")
	if err != nil || total > 256<<10+200 || !strings.Contains(joined, "[live output truncated;") || strings.Contains(joined, "late") {
		t.Fatalf("err %v live bytes %d", err, total)
	}
	if r := decodeExec(t, out); !r.Truncated || !strings.Contains(r.Stdout, "bytes omitted") {
		t.Fatalf("the final result must still cut the output on its own: %.200s", out)
	}
}

func TestAFailedKillOfACancelledCommandDoesNotMaskTheCancellation(t *testing.T) {
	// Arrange
	started := make(chan struct{})
	var killed atomic.Bool
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.SetJobs(cancellableJob(started, &killed))
		sb.FailScript("# work:job-kill", errors.New("sandbox unreachable"))
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()

	// Act
	_, err := f.call(ctx, "execute_command", `{"command":"x","timeoutSeconds":30}`)

	// Assert
	if !errors.Is(err, context.Canceled) || killed.Load() {
		t.Fatalf("err %v killed %v", err, killed.Load())
	}
}

func TestJobOutputWaitStopsWhenTheCallIsCancelled(t *testing.T) {
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.SetJobs(blockingJob(make(chan struct{}))) })
	started, _ := f.call(context.Background(), "execute_command", `{"command":"x","background":true}`)
	id := decodeExec(t, started).JobID
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Act
	_, err := f.call(ctx, "job_output", `{"jobId":"`+id+`","waitSeconds":300}`)

	// Assert
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
}
