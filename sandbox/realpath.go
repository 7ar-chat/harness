package sandbox

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"
)

// RealPathMarker is the first line of the realpath script (sandboxtest fakes it).
const RealPathMarker = "# work:realpath"

// realPathScript resolves WORK_PATH_0 … WORK_PATH_{WORK_N-1} with `realpath -m`
// (every symlink of the existing components resolved; missing components
// allowed) and prints the results one per line, in order, then the line "ok"
// when realpath succeeded (sh is dash in the sandbox: no pipefail, the pipe
// hides realpath's exit status). OpenSandbox delivers output line by line and
// the adapter appends a newline to every message (withNewline, ADISTU-6764),
// so the output must not depend on where its newlines fall: the NUL
// terminators of `realpath -z` become the line ends, and a newline inside a
// resolved path becomes a NUL RealPaths refuses.
const realPathScript = RealPathMarker + `
set -- ; i=0
while [ "$i" -lt "$WORK_N" ]; do eval "set -- \"\$@\" \"\${WORK_PATH_$i}\""; i=$((i+1)); done
{ realpath -m -z -- "$@" && printf 'ok\0'; } | tr '\0\n' '\n\0'
`

// realPathOK is the script's last line when realpath succeeded.
const realPathOK = "ok"

// ErrRealPath means the sandbox could not resolve a path (fail closed).
var ErrRealPath = errors.New("the sandbox could not resolve the path")

// RealPaths resolves the symlinks of absolute sandbox paths in sb (review round
// 1: files/content and the approval work-area check decide on the real path).
func RealPaths(ctx context.Context, sb Sandbox, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	env := map[string]string{"WORK_N": strconv.Itoa(len(paths))}
	for i, p := range paths {
		if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\x00\n\r") {
			return nil, fmt.Errorf("%w: %q is not an absolute path", ErrRealPath, p)
		}
		env["WORK_PATH_"+strconv.Itoa(i)] = p
	}
	var out strings.Builder
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	res, err := sb.Exec(cctx, ExecRequest{Command: realPathScript, Cwd: "/", Timeout: 10 * time.Second, Env: env,
		OnStdout: func(t string) {
			if out.Len() < 1<<20 {
				out.WriteString(t)
			}
		}})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("%w (exit %d)", ErrRealPath, res.ExitCode)
	}
	// One result per non-empty line, then "ok": the transport's extra newlines
	// only add empty lines; a failing realpath prints no "ok"; a resolved path
	// holding a newline holds a NUL.
	var got []string
	for _, line := range strings.Split(out.String(), "\n") {
		if line != "" {
			got = append(got, line)
		}
	}
	if len(got) != len(paths)+1 || got[len(paths)] != realPathOK {
		return nil, fmt.Errorf("%w: %d lines for %d paths", ErrRealPath, len(got), len(paths))
	}
	got = got[:len(paths)]
	for i, p := range got {
		if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\x00\r") {
			return nil, fmt.Errorf("%w: %q", ErrRealPath, p)
		}
		got[i] = path.Clean(p)
	}
	return got, nil
}

// RealPath is RealPaths for one path.
func RealPath(ctx context.Context, sb Sandbox, p string) (string, error) {
	out, err := RealPaths(ctx, sb, []string{p})
	if err != nil {
		return "", err
	}
	return out[0], nil
}
