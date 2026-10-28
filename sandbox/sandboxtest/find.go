package sandboxtest

import (
	"context"
	"fmt"
	"hash/crc32"
	"sort"
	"strings"

	"github.com/GhaziBenDahmane/harness/sandbox"
)

// FindExec answers the artifacts snapshot listing (a "find /workspace …
// -printf" command) from the in-memory files; every other command goes to next
// (nil: exit 0). The reported mtime is a checksum of the content, so rewriting
// a file with other bytes looks modified.
func FindExec(s *Sandbox, next ExecFunc) ExecFunc {
	return func(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
		if !strings.HasPrefix(req.Command, "find /workspace") || !strings.Contains(req.Command, "-printf") {
			if next == nil {
				return sandbox.ExecResult{}, nil
			}
			return next(ctx, req)
		}
		s.mu.Lock()
		var lines []string
		for p, data := range s.files {
			if !strings.HasPrefix(p, "/workspace/") || excluded(p) {
				continue
			}
			lines = append(lines, fmt.Sprintf("%d %d.0000000000 %s\n", len(data), crc32.ChecksumIEEE(data), p))
		}
		s.mu.Unlock()
		sort.Strings(lines)
		if req.OnStdout != nil {
			for _, l := range lines {
				req.OnStdout(l)
			}
		}
		return sandbox.ExecResult{}, nil
	}
}

func excluded(p string) bool {
	if strings.HasPrefix(p, "/workspace/.work/") {
		return true
	}
	for _, part := range strings.Split(p, "/") {
		switch part {
		case "node_modules", ".git", "__pycache__", ".venv":
			return true
		}
	}
	return false
}

// RemoveTree deletes a directory and everything below it, like `rm -rf dir`
// run by a command: the files of the jobs it holds are gone too.
func (s *Sandbox) RemoveTree(dir string) {
	dir = strings.TrimSuffix(dir, "/")
	s.mu.Lock()
	defer s.mu.Unlock()
	for p := range s.files {
		if p == dir || strings.HasPrefix(p, dir+"/") {
			delete(s.files, p)
		}
	}
	for d := range s.dirs {
		if d == dir || strings.HasPrefix(d, dir+"/") {
			delete(s.dirs, d)
		}
	}
	for jd, j := range s.jobs {
		if jd == dir || strings.HasPrefix(jd, dir+"/") {
			j.mu.Lock()
			j.removed = true
			j.mu.Unlock()
		}
	}
}

// Remove deletes a file (test setup).
func (s *Sandbox) Remove(p string) {
	s.mu.Lock()
	delete(s.files, p)
	s.mu.Unlock()
}
