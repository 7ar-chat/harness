//go:build unix

package local

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/GhaziBenDahmane/harness/sandbox"
)

// Background jobs: the files are those of the job scripts the tools run in a
// container (cmd.sh, pid, stdout, stderr, exit, killed under WORK_JOB_DIR).

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func readInt(p string) (int, bool) {
	b, err := os.ReadFile(p)
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return n, err == nil
}

func alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

func (in *Instance) jobDir(req sandbox.ExecRequest) (string, bool) {
	d, err := in.host(req.Env["WORK_JOB_DIR"])
	return d, err == nil && req.Env["WORK_JOB_DIR"] != ""
}

func (in *Instance) jobStart(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	dir, ok := in.jobDir(req)
	raw, err := base64.StdEncoding.DecodeString(req.Env["WORK_JOB_CMD_B64"])
	if !ok || err != nil {
		return exit(2)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return exit(1)
	}
	script := filepath.Join(dir, "cmd.sh")
	if err := os.WriteFile(script, []byte(in.mapCommand(string(raw))), 0o644); err != nil {
		return exit(1)
	}
	stdout, err1 := os.Create(filepath.Join(dir, "stdout"))
	stderr, err2 := os.Create(filepath.Join(dir, "stderr"))
	if err1 != nil || err2 != nil {
		return exit(1)
	}
	// Not CommandContext: the job outlives the call that started it.
	cmd := exec.Command(in.shell, script)
	cmd.Dir = in.ws
	cmd.Env = append(os.Environ(), "HOME="+in.ws)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		stdout.Close()
		stderr.Close()
		return exit(1)
	}
	_ = os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
	go func() {
		err := cmd.Wait()
		stdout.Close()
		stderr.Close()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		tmp := filepath.Join(dir, "exit.tmp")
		if os.WriteFile(tmp, []byte(strconv.Itoa(code)), 0o644) == nil {
			_ = os.Rename(tmp, filepath.Join(dir, "exit"))
		}
	}()
	return in.follow(ctx, dir, req)
}

// follow streams the job's output until it ends or WORK_JOB_WAIT seconds pass.
func (in *Instance) follow(ctx context.Context, dir string, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	wait := atoi(req.Env["WORK_JOB_WAIT"])
	if wait <= 0 {
		return exit(0)
	}
	offs := map[string]int64{}
	pump := func() {
		for name, emit := range map[string]func(string){"stdout": req.OnStdout, "stderr": req.OnStderr} {
			if emit == nil {
				continue
			}
			f, err := os.Open(filepath.Join(dir, name))
			if err != nil {
				continue
			}
			if _, err := f.Seek(offs[name], io.SeekStart); err == nil {
				if b, _ := io.ReadAll(f); len(b) > 0 {
					offs[name] += int64(len(b))
					emit(in.virtualText(string(b)))
				}
			}
			f.Close()
		}
	}
	end := time.Now().Add(time.Duration(wait) * time.Second)
	for time.Now().Before(end) && !exists(filepath.Join(dir, "exit")) && !exists(filepath.Join(dir, "killed")) {
		pump()
		select {
		case <-ctx.Done():
			return sandbox.ExecResult{}, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	time.Sleep(50 * time.Millisecond)
	pump()
	return exit(0)
}

func (in *Instance) jobWait(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	dir, ok := in.jobDir(req)
	if !ok || !exists(dir) {
		return exit(0)
	}
	return in.follow(ctx, dir, req)
}

func (in *Instance) jobStatus(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	dir, ok := in.jobDir(req)
	if !ok || !exists(dir) {
		in.out(req, "status missing\n")
		return exit(0)
	}
	status, code := "running", ""
	if b, err := os.ReadFile(filepath.Join(dir, "exit")); err == nil {
		status, code = "finished", strings.TrimSpace(string(b))
	}
	if exists(filepath.Join(dir, "killed")) {
		status, code = "killed", ""
	}
	if pid, _ := readInt(filepath.Join(dir, "pid")); status == "running" && !alive(pid) {
		time.Sleep(200 * time.Millisecond)
		if b, err := os.ReadFile(filepath.Join(dir, "exit")); err == nil {
			status, code = "finished", strings.TrimSpace(string(b))
		} else {
			status = "lost"
		}
	}
	var out strings.Builder
	fmt.Fprintf(&out, "status %s\ncode %s\n", status, code)
	head, tail := atoi(req.Env["WORK_HEAD"]), atoi(req.Env["WORK_TAIL"])
	small := true
	dash := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}
	for _, name := range []string{"stdout", "stderr"} {
		data, _ := os.ReadFile(filepath.Join(dir, name))
		h, t := data, []byte(nil)
		if len(data) > head+tail {
			small = false
			h, t = data[:head], data[len(data)-tail:]
		}
		hs, ts := "", ""
		if len(h) > 0 {
			hs = b64(h)
		}
		if len(t) > 0 {
			ts = b64(t)
		}
		fmt.Fprintf(&out, "%s %d %s %s\n", name, len(data), dash(hs), dash(ts))
	}
	if req.Env["WORK_JOB_CLEAN"] == "1" && status == "finished" && small {
		_ = os.RemoveAll(dir)
	}
	in.out(req, out.String())
	return exit(0)
}

func (in *Instance) jobKill(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	dir, ok := in.jobDir(req)
	if !ok || !exists(dir) {
		return exit(1)
	}
	if exists(filepath.Join(dir, "exit")) {
		return exit(0)
	}
	_ = os.WriteFile(filepath.Join(dir, "killed"), nil, 0o644)
	if pid, ok := readInt(filepath.Join(dir, "pid")); ok && pid > 1 {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		for i := 0; i < 10 && alive(pid); i++ {
			time.Sleep(100 * time.Millisecond)
		}
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	return exit(0)
}
