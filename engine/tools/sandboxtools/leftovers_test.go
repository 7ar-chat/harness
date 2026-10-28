package sandboxtools_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/sandbox"
	"github.com/GhaziBenDahmane/harness/sandbox/sandboxtest"
)

// Final review of round 3 (F7): the files the sandbox tools leave behind
// (staged uploads of failed writes, the shadow records of ended jobs) are
// cleaned up, best effort.

func TestAWriteWhoseMoveIntoPlaceFailsLeavesNoStagedFile(t *testing.T) {
	// write_file stages its content under /workspace/.work/.staging/ before moving it into place; when the move fails (its exec breaks off), the staged copy is removed and the call fails.
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.FailScript(sandbox.PlaceMarker, errors.New("connection reset by peer"))
	})

	// Act
	_, err := f.call(context.Background(), "write_file", `{"path":"/workspace/outputs/report.md","content":"# Report"}`)

	// Assert
	if left := f.sb.Paths(sandbox.StagingDir + "/"); err == nil || len(left) != 0 {
		t.Fatalf("err %v, staged files left %v", err, left)
	}
}

func TestTheFirstToolCallOfARunSweepsOldStagedFilesAndEndedJobRecords(t *testing.T) {
	// At a run's first sandbox acquisition, staged uploads older than 10 minutes and the /tmp/work-jobs records of jobs that ended more than a day ago are removed; a fresh staged upload (another call's write in flight), a recent record and a running job's record stay (F7).
	// Arrange
	base, shadow := t.TempDir(), t.TempDir()
	staging := filepath.Join(base, "host-1", "workspace", ".work", ".staging")
	old := time.Now().Add(-2 * time.Hour)
	put := func(p, content string, mtime time.Time) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(p, mtime, mtime)
		_ = os.Chtimes(filepath.Dir(p), mtime, mtime)
	}
	put(filepath.Join(staging, "stale"), "lost upload", old)
	put(filepath.Join(staging, "fresh"), "in flight", time.Now())
	twoDays := time.Now().Add(-48 * time.Hour)
	put(filepath.Join(shadow, "job_ended", "exit"), "0", twoDays)
	put(filepath.Join(shadow, "job_killed", "killed"), "", twoDays)
	put(filepath.Join(shadow, "job_recent", "exit"), "0", time.Now())
	put(filepath.Join(shadow, "job_running", "pid"), strconv.Itoa(os.Getpid()), twoDays) // a live process
	f, _ := newHostFixtureAt(t, base, map[string]string{"HARNESS_JOBS_SHADOW_ROOT": shadow})

	// Act
	_, err := f.call(context.Background(), "list_files", `{"path":"/workspace"}`)

	// Assert
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	if err != nil || exists(filepath.Join(staging, "stale")) || !exists(filepath.Join(staging, "fresh")) ||
		exists(filepath.Join(shadow, "job_ended")) || exists(filepath.Join(shadow, "job_killed")) ||
		!exists(filepath.Join(shadow, "job_recent")) || !exists(filepath.Join(shadow, "job_running")) {
		entries, _ := os.ReadDir(shadow)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("err %v stale %v fresh %v shadow %v", err, exists(filepath.Join(staging, "stale")), exists(filepath.Join(staging, "fresh")), names)
	}
}
