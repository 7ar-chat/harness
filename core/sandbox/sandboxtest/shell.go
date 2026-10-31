package sandboxtest

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/7ar-chat/harness/core/sandbox"
)

// ShellExec runs commands with bash on a temporary copy of the in-memory
// /workspace (files and directories), then writes every change back, so real
// scripts (git, tar) work on the sandbox. "/workspace" is mapped in the
// command, the working directory and the output; git ignores the user's and
// the system configuration. Compose it with the snapshot listing emulation
// as FindExec(sb, ShellExec(sb)).
func ShellExec(s *Sandbox) ExecFunc {
	return func(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
		if err := s.deadErr(); err != nil {
			return sandbox.ExecResult{}, err
		}
		root, err := os.MkdirTemp("", "sbx-shell-*")
		if err != nil {
			return sandbox.ExecResult{}, err
		}
		defer func() { _ = os.RemoveAll(root) }()
		if err := s.materialize(root); err != nil {
			return sandbox.ExecResult{}, err
		}
		if req.Timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, req.Timeout)
			defer cancel()
		}
		cmd := exec.CommandContext(ctx, "bash", "-c", mapWorkspace(req.Command, root))
		cmd.Dir = root + sandbox.Workspace
		if strings.HasPrefix(req.Cwd, sandbox.Workspace) {
			cmd.Dir = root + req.Cwd
		}
		cmd.Env = append(os.Environ(), "HOME="+root, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		for k, v := range req.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		runErr := cmd.Run()
		code := 0
		var exitErr *exec.ExitError
		switch {
		case errors.As(runErr, &exitErr):
			code = exitErr.ExitCode()
		case runErr != nil:
			return sandbox.ExecResult{}, runErr
		}
		if err := s.absorb(root); err != nil {
			return sandbox.ExecResult{}, err
		}
		if out := strings.ReplaceAll(stdout.String(), root, ""); out != "" && req.OnStdout != nil {
			req.OnStdout(out)
		}
		if out := strings.ReplaceAll(stderr.String(), root, ""); out != "" && req.OnStderr != nil {
			req.OnStderr(out)
		}
		return sandbox.ExecResult{ExitCode: code}, nil
	}
}

// mapWorkspace prefixes with root every "/workspace" of s that starts a
// sandbox path: not inside a longer path or name, so a host path such as the
// CI checkout /home/jenkins/agent/workspace/... (a checker script run by its
// absolute path) and /workspacefoo are left alone.
func mapWorkspace(s, root string) string {
	var b strings.Builder
	last := 0
	for i := 0; ; {
		j := strings.Index(s[i:], sandbox.Workspace)
		if j < 0 {
			break
		}
		j += i
		end := j + len(sandbox.Workspace)
		if (j == 0 || !isPathByte(s[j-1])) && (end == len(s) || s[end] == '/' || !isPathByte(s[end])) {
			b.WriteString(s[last:j])
			b.WriteString(root)
			last = j
		}
		i = end
	}
	b.WriteString(s[last:])
	return b.String()
}

// isPathByte reports whether c can be part of a path segment (or is a "/").
func isPathByte(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c >= 0x80:
		return true
	}
	return strings.IndexByte("/_-.~+@%", c) >= 0
}

func inWorkspace(p string) bool {
	if strings.HasPrefix(p, jobsDir) {
		return false // the fake jobs' files live in memory only (protocol.go)
	}
	return p == sandbox.Workspace || strings.HasPrefix(p, sandbox.Workspace+"/")
}

const jobsDir = sandbox.Workspace + "/.work/jobs/"

// materialize writes the in-memory /workspace under root.
func (s *Sandbox) materialize(root string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(root+sandbox.Workspace, 0o755); err != nil {
		return err
	}
	for d := range s.dirs {
		if inWorkspace(d) {
			if err := os.MkdirAll(root+d, 0o755); err != nil {
				return err
			}
		}
	}
	for p, data := range s.files {
		if !inWorkspace(p) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(root+p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(root+p, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// absorb replaces the in-memory /workspace with the tree under root.
func (s *Sandbox) absorb(root string) error {
	files := map[string][]byte{}
	dirs := map[string]bool{}
	base := root + sandbox.Workspace
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := sandbox.Workspace + strings.TrimPrefix(p, base)
		switch {
		case d.IsDir():
			dirs[rel] = true
		case d.Type().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			files[rel] = data
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for p := range s.files {
		if inWorkspace(p) {
			delete(s.files, p)
		}
	}
	for d := range s.dirs {
		if inWorkspace(d) {
			delete(s.dirs, d)
		}
	}
	for p, data := range files {
		s.files[p] = data
	}
	for d := range dirs {
		s.dirs[d] = true
	}
	s.dirs["/"] = true
	return nil
}
