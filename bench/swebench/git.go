package swebench

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// GitRunner runs one git command in dir and returns its standard output. Tests substitute a fake;
// ExecGit shells out to the git binary.
type GitRunner interface {
	Run(ctx context.Context, dir string, args ...string) (string, error)
}

// ExecGit is the default GitRunner. Failures carry git's standard error.
type ExecGit struct{}

// Run implements GitRunner. Prompts for credentials are disabled so a private or missing remote
// fails instead of hanging an unattended run.
func (ExecGit) Run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return stdout.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args[:min(len(args), 2)], " "), err, tail(stderr.String(), 600))
	}
	return stdout.String(), nil
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
