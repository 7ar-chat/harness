package gitsafe

import (
	"context"
	"fmt"
	"strings"
)

// runNS holds one ref per run, pointing at the snapshot taken when the run started
// (snapshots are deduplicated, so several runs may share one).
const runNS = "refs/harness/runs/"

func runRef(thread, run string) string { return runNS + safeThread(thread) + "/" + safeThread(run) }

// RecordRun remembers s as the snapshot taken before the run started.
func RecordRun(ctx context.Context, dir, thread, run string, s Snap) error {
	top, err := Toplevel(ctx, dir)
	if err != nil {
		return err
	}
	_, err = git(ctx, top, nil, "update-ref", "-m", "harness run", runRef(thread, run), s.Commit)
	return err
}

// ForRun returns the snapshot taken before the run started; an error when none was recorded.
func ForRun(ctx context.Context, dir, thread, run string) (Snap, error) {
	top, err := Toplevel(ctx, dir)
	if err != nil {
		return Snap{}, err
	}
	sha, err := git(ctx, top, nil, "rev-parse", "--verify", "-q", runRef(thread, run)+"^{commit}")
	if err != nil || sha == "" {
		return Snap{}, fmt.Errorf("gitsafe: no snapshot recorded for run %q", run)
	}
	// The numbered snapshot ref is returned: Restore derives the backup namespace from it.
	for _, s := range list(ctx, top, snapshotNS, safeThread(thread)) {
		if s.Commit == sha {
			return s, nil
		}
	}
	return Snap{}, fmt.Errorf("gitsafe: the snapshot of run %q is gone", run)
}

// Changed lists the paths whose content differs between two refs.
func Changed(ctx context.Context, dir, from, to string) ([]string, error) {
	top, err := Toplevel(ctx, dir)
	if err != nil {
		return nil, err
	}
	out, err := git(ctx, top, nil, "diff", "--name-only", from, to)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range strings.Split(out, "\n") {
		if f = strings.TrimSpace(f); f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}
