package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// PlaceMarker is the first line of the place script (sandboxtest fakes it).
const PlaceMarker = "# work:place"

// UnstageMarker is the first line of the unstage script (sandboxtest fakes it).
const UnstageMarker = "# work:unstage"

// unstageScript removes WORK_STAGED (a write that failed before or while
// moving it into place; final review of round 3, F7).
const unstageScript = UnstageMarker + `
rm -f -- "$WORK_STAGED"
`

// StagingDir receives the uploads WriteFileNoFollow moves into place.
const StagingDir = ScratchDir + "/.staging"

// placeScript moves WORK_STAGED onto WORK_TARGET in one exec, unless the target
// is a symbolic link or its directory resolves elsewhere (a symlinked
// component; /workspace itself may be a mount point under another name). It
// prints the real directory and exits 4 when it refuses, 3 on other failures.
const placeScript = PlaceMarker + `
set -u
T="$WORK_TARGET"; S="$WORK_STAGED"; D=$(dirname -- "$T")
W=$(realpath -m -- /workspace) || W=/workspace
case "$D" in /workspace|/workspace/*) WANT="$W${D#/workspace}";; *) WANT="$D";; esac
R=$(realpath -m -- "$D") || { rm -f -- "$S"; exit 3; }
if [ "$R" != "$WANT" ] || [ -L "$T" ]; then rm -f -- "$S"; printf '%s' "$R"; exit 4; fi
mkdir -p -- "$D" || { rm -f -- "$S"; exit 3; }
mv -f -T -- "$S" "$T" || { rm -f -- "$S"; exit 3; }
`

// ErrSymlinkTarget means a write was refused because its target is, or is
// inside, a symbolic link (a *SymlinkError names the real directory).
var ErrSymlinkTarget = errors.New("the target is a symbolic link or inside one")

// SymlinkError is the refusal of WriteFileNoFollow.
type SymlinkError struct{ RealDir string }

func (e *SymlinkError) Error() string {
	return ErrSymlinkTarget.Error() + " (real directory: " + e.RealDir + ")"
}

func (e *SymlinkError) Unwrap() error { return ErrSymlinkTarget }

// WriteFileNoFollow writes data to p without following a symbolic link at p
// or in its directory (round 3, N6: the approval gate's realpath and the write
// are two operations; this one re-checks in the exec that writes). The content
// is uploaded under StagingDir, then moved into place by one script that
// checks the target first. A refusal is a *SymlinkError (ErrSymlinkTarget).
func WriteFileNoFollow(ctx context.Context, sb Sandbox, p string, data []byte) error {
	staged := StagingDir + "/" + uuid.NewString()
	placed := false
	defer func() {
		if !placed {
			unstage(ctx, sb, staged)
		}
	}()
	if err := sb.WriteFile(ctx, staged, data); err != nil {
		return err
	}
	var out strings.Builder
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res, err := sb.Exec(cctx, ExecRequest{Command: placeScript, Cwd: "/", Timeout: 20 * time.Second,
		Env: map[string]string{"WORK_TARGET": p, "WORK_STAGED": staged},
		OnStdout: func(t string) {
			if out.Len() < 4096 {
				out.WriteString(t)
			}
		}})
	switch {
	case err != nil:
		return err
	case res.ExitCode == 3 || res.ExitCode == 4:
		placed = true // the script removed the staged file itself
		if res.ExitCode == 4 {
			return &SymlinkError{RealDir: strings.TrimSpace(out.String())}
		}
		return fmt.Errorf("moving the written file into place failed (exit %d)", res.ExitCode)
	case res.ExitCode != 0:
		return fmt.Errorf("moving the written file into place failed (exit %d)", res.ExitCode)
	}
	placed = true
	return nil
}

// unstage removes a staged upload left by a failed write, best effort (a
// sandbox that cannot run it now gets the sweep at a later acquisition).
func unstage(ctx context.Context, sb Sandbox, staged string) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_, _ = sb.Exec(cctx, ExecRequest{Command: unstageScript, Cwd: "/", Timeout: 5 * time.Second, Env: map[string]string{"WORK_STAGED": staged}})
}
