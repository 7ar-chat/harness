package sandboxtest

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/GhaziBenDahmane/harness/core/sandbox"
)

// realpathStub is the `realpath` the fake's realpath script runs: it prints
// the fake's resolutions of its paths like `realpath -m`, NUL-terminated
// with -z and newline-terminated without.
const realpathStub = `#!/bin/sh
d=$(dirname "$0"); t=nl
for a; do [ "$a" = -- ] && break; [ "$a" = -z ] && t=z; done
cat "$d/$t"
`

// realpath runs sandbox.RealPaths' script itself with sh, its realpath a stub
// printing the fake's resolutions of HARNESS_PATH_0 … (symlinks followed by
// realLocked, like realpath -m): the script's own handling of the output and
// of the exit status is the real one, and Exec delivers the output like
// OpenSandbox does.
func (s *Sandbox) realpath(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	n := atoiEnv(req.Env, "HARNESS_N")
	var z, nl strings.Builder
	s.mu.Lock()
	s.realpaths++
	for i := 0; i < n; i++ {
		real := s.realLocked(req.Env["HARNESS_PATH_"+strconv.Itoa(i)])
		z.WriteString(real + "\x00")
		nl.WriteString(real + "\n")
	}
	s.mu.Unlock()
	dir, err := os.MkdirTemp("", "sbx-realpath-*")
	if err != nil {
		return sandbox.ExecResult{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	for name, data := range map[string]string{"z": z.String(), "nl": nl.String(), "realpath": realpathStub} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o755); err != nil {
			return sandbox.ExecResult{}, err
		}
	}
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", req.Command)
	cmd.Dir = "/"
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return sandbox.ExecResult{}, ctx.Err()
	}
	code := 0
	var exitErr *exec.ExitError
	switch {
	case errors.As(runErr, &exitErr):
		code = exitErr.ExitCode()
	case runErr != nil:
		return sandbox.ExecResult{}, runErr
	}
	if stdout.Len() > 0 && req.OnStdout != nil {
		req.OnStdout(stdout.String())
	}
	if stderr.Len() > 0 && req.OnStderr != nil {
		req.OnStderr(stderr.String())
	}
	return sandbox.ExecResult{ExitCode: code}, nil
}
