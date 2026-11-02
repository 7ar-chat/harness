package artifacts_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/features/artifacts"
)

// uploads stores n uploads "f<i>.txt" (oldest first) in the rig's thread, before any sandbox exists.
func (r *sbRig) uploads(names ...string) {
	r.t.Helper()
	for _, n := range names {
		r.upload(n, "content of "+n)
	}
}

func TestRestoreWorkspaceWritesTheLatestVersionsIntoANewSandbox(t *testing.T) {
	// Every live artifact is written back; deleted ones are not; the notice says how many came back and why the sandbox is new.
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	r.uploads("a.txt", "b.txt")
	sb0 := r.sandbox()
	sb0.Put("/workspace/c.txt", "c")
	r.snapshot(sb0)
	sb0.Remove("/workspace/c.txt")
	r.snapshot(sb0) // c.txt is now a deleted artifact
	fresh := sandboxtest.NewSandbox("sbx-fresh")
	fresh.SetExec(sandboxtest.FindExec(fresh, nil))

	// Act
	recreated := r.svc.RestoreWorkspace(r.ctx, r.th.ID, fresh, true)
	other := sandboxtest.NewSandbox("sbx-other")
	other.SetExec(sandboxtest.FindExec(other, nil))
	restarted := r.svc.RestoreWorkspace(r.ctx, r.th.ID, other, false)

	// Assert
	if got, _ := fresh.File("/workspace/a.txt"); got != "content of a.txt" {
		t.Fatalf("a.txt restored as %q", got)
	}
	if _, ok := fresh.File("/workspace/c.txt"); ok {
		t.Fatal("a deleted artifact was restored")
	}
	if !strings.Contains(recreated, "recreated") || !strings.Contains(recreated, "2 saved file(s)") || strings.Contains(recreated, "pending") {
		t.Fatalf("recreated notice %q", recreated)
	}
	if !strings.Contains(restarted, "was restarted") || strings.Contains(restarted, "recreated") {
		t.Fatalf("restarted notice %q", restarted)
	}
	if a := r.art("/workspace/a.txt"); a.SandboxID != "sbx-other" {
		t.Fatalf("restored file not recorded as present in the sandbox: %q", a.SandboxID)
	}
}

func TestRestoreWorkspaceOfAThreadWithoutFilesSaysNothing(t *testing.T) {
	// Nothing to restore: no notice for the model.
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	sb := r.sandbox()

	// Act
	notice := r.svc.RestoreWorkspace(r.ctx, r.th.ID, sb, false)

	// Assert
	if notice != "" {
		t.Fatalf("notice %q", notice)
	}
}

func TestRestoredFilesAreNotTakenForNewOrDeletedBySnapshots(t *testing.T) {
	// After a restore the next snapshot downloads nothing and a later removal is a real deletion in that sandbox.
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	r.uploads("a.txt")
	fresh := sandboxtest.NewSandbox("sbx-fresh")
	fresh.SetExec(sandboxtest.FindExec(fresh, nil))
	r.svc.RestoreWorkspace(r.ctx, r.th.ID, fresh, false)
	before := r.art("/workspace/a.txt")

	// Act
	again := r.snapshotWith(fresh)
	fresh.Remove("/workspace/a.txt")
	removed := r.snapshotWith(fresh)

	// Assert
	if len(again) != 0 || r.art("/workspace/a.txt").Version != before.Version {
		t.Fatalf("snapshot after restore: %v", again)
	}
	if got := changePaths(removed); got["/workspace/a.txt"] != store.ChangeDeleted {
		t.Fatalf("removal after restore: %v", got)
	}
}

func TestRestoreWorkspaceKeepsFilesOverTheLimitPendingAndRestoresThemFirstNextTime(t *testing.T) {
	// Above RestoreMaxFiles the newest files are written, the others are listed as pending, and a later restore writes the pending ones first.
	// Arrange
	r := newSBRig(t, artifacts.Options{RestoreMaxFiles: 2})
	r.uploads("old.txt", "mid.txt", "new.txt")
	first := sandboxtest.NewSandbox("sbx-1")
	first.SetExec(sandboxtest.FindExec(first, nil))
	second := sandboxtest.NewSandbox("sbx-2")
	second.SetExec(sandboxtest.FindExec(second, nil))

	// Act
	notice := r.svc.RestoreWorkspace(r.ctx, r.th.ID, first, false)
	next := r.svc.RestoreWorkspace(r.ctx, r.th.ID, second, false)

	// Assert
	if !strings.Contains(notice, "2 saved file(s)") || !strings.Contains(notice, "Still pending") || !strings.Contains(notice, "/workspace/old.txt") ||
		!strings.Contains(notice, "limit 2 files") {
		t.Fatalf("notice %q", notice)
	}
	if _, ok := first.File("/workspace/old.txt"); ok {
		t.Fatal("the oldest file was written despite the limit")
	}
	if _, ok := second.File("/workspace/old.txt"); !ok {
		t.Fatal("the pending file was not restored first the next time")
	}
	if !strings.Contains(next, "Still pending") || strings.Contains(next, "/workspace/old.txt") {
		t.Fatalf("next notice %q", next)
	}
}

func TestRestoreWorkspaceHonoursTheByteLimit(t *testing.T) {
	// Files whose cumulative size exceeds RestoreMaxBytes stay pending.
	// Arrange
	r := newSBRig(t, artifacts.Options{RestoreMaxBytes: 40})
	r.uploads("one.txt", "two.txt") // 16 bytes each... "content of one.txt" is 17
	r.upload("three.txt", strings.Repeat("z", 30))
	fresh := sandboxtest.NewSandbox("sbx-fresh")
	fresh.SetExec(sandboxtest.FindExec(fresh, nil))

	// Act
	notice := r.svc.RestoreWorkspace(r.ctx, r.th.ID, fresh, false)

	// Assert
	if !strings.Contains(notice, "1 saved file(s)") || !strings.Contains(notice, "Still pending") {
		t.Fatalf("notice %q", notice)
	}
	if _, ok := fresh.File("/workspace/three.txt"); !ok {
		t.Fatal("the newest file must come first")
	}
}

func TestRestoreWorkspaceListsAtMostTwentyPendingFiles(t *testing.T) {
	// The notice names 20 pending files and counts the rest.
	// Arrange
	r := newSBRig(t, artifacts.Options{RestoreMaxFiles: 1})
	for i := range 23 {
		r.upload(fmt.Sprintf("f%02d.txt", i), "x")
	}
	fresh := sandboxtest.NewSandbox("sbx-fresh")
	fresh.SetExec(sandboxtest.FindExec(fresh, nil))

	// Act
	notice := r.svc.RestoreWorkspace(r.ctx, r.th.ID, fresh, false)

	// Assert
	if !strings.Contains(notice, "1 saved file(s)") || !strings.Contains(notice, "and 2 more.") || strings.Count(notice, "/workspace/f") != 20 {
		t.Fatalf("notice %q", notice)
	}
}

func TestRestoreWorkspaceKeepsAFilePendingWhenItsContentOrSandboxWriteFails(t *testing.T) {
	// A missing blob or a refused write leaves that file pending without stopping the others.
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	r.uploads("blob-lost.txt", "write-fails.txt", "fine.txt")
	_ = r.mem.DeleteBlob(r.ctx, sum([]byte("content of blob-lost.txt")))
	fresh := sandboxtest.NewSandbox("sbx-fresh")
	fresh.SetExec(sandboxtest.FindExec(fresh, nil))
	fresh.SetWriteErr(func(p string) error {
		if strings.HasSuffix(p, "write-fails.txt") {
			return errors.New("read-only file system")
		}
		return nil
	})

	// Act
	notice := r.svc.RestoreWorkspace(r.ctx, r.th.ID, fresh, false)

	// Assert
	if !strings.Contains(notice, "1 saved file(s)") || !strings.Contains(notice, "/workspace/blob-lost.txt") || !strings.Contains(notice, "/workspace/write-fails.txt") {
		t.Fatalf("notice %q", notice)
	}
	if _, ok := fresh.File("/workspace/fine.txt"); !ok {
		t.Fatal("the healthy file was not restored")
	}
	if r.art("/workspace/blob-lost.txt").RestoreSkippedAt == nil {
		t.Fatal("the failed file is not marked as skipped")
	}
}

func TestRestoreWorkspaceWritesNothingWithoutTheThreadLock(t *testing.T) {
	// Without the thread lock, nothing is written (a snapshot or version restore could be racing): every file stays pending and the notice says the files were busy.
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	r.uploads("a.txt")
	r.withStore(&faultStore{Store: r.mem, lockErr: errors.New("lock timeout")}, nil, artifacts.Options{})
	fresh := sandboxtest.NewSandbox("sbx-fresh")
	fresh.SetExec(sandboxtest.FindExec(fresh, nil))

	// Act
	notice := r.svc.RestoreWorkspace(r.ctx, r.th.ID, fresh, false)

	// Assert
	if _, ok := fresh.File("/workspace/a.txt"); ok {
		t.Fatal("a file was written without the lock")
	}
	if !strings.Contains(notice, "0 saved file(s)") || !strings.Contains(notice, "the conversation's files were busy") || !strings.Contains(notice, "/workspace/a.txt") {
		t.Fatalf("notice %q", notice)
	}
}

func TestRestoreWorkspaceFailsQuietlyWhenTheArtifactsCannotBeListed(t *testing.T) {
	// A metadata failure means no restore and no notice (the model is not told about files that were never looked at).
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	r.uploads("a.txt")
	r.withStore(&faultStore{Store: r.mem, listErr: errors.New("db down")}, nil, artifacts.Options{})
	fresh := sandboxtest.NewSandbox("sbx-fresh")

	// Act
	notice := r.svc.RestoreWorkspace(r.ctx, r.th.ID, fresh, false)

	// Assert
	if notice != "" || len(fresh.Paths("/workspace")) != 0 {
		t.Fatalf("notice %q files %v", notice, fresh.Paths("/workspace"))
	}
}

func TestRestoreWorkspaceSurvivesBookkeepingFailures(t *testing.T) {
	// If the store cannot record what was restored or skipped, the files are still written and the notice still returned.
	// Arrange
	r := newSBRig(t, artifacts.Options{RestoreMaxFiles: 1})
	r.uploads("a.txt", "b.txt")
	r.withStore(&faultStore{Store: r.mem, materializeErr: errors.New("db down"), skipErr: errors.New("db down")}, nil, artifacts.Options{RestoreMaxFiles: 1})
	fresh := sandboxtest.NewSandbox("sbx-fresh")
	fresh.SetExec(sandboxtest.FindExec(fresh, nil))

	// Act
	notice := r.svc.RestoreWorkspace(r.ctx, r.th.ID, fresh, false)

	// Assert
	if !strings.Contains(notice, "1 saved file(s)") || !strings.Contains(notice, "Still pending") {
		t.Fatalf("notice %q", notice)
	}
}

func TestRestoreWorkspaceRecordsFilesTheListingDoesNotShow(t *testing.T) {
	// If the post-restore listing fails or does not show a file at its size, the file is still marked present in the sandbox.
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	r.uploads("a.txt")
	fresh := sandboxtest.NewSandbox("sbx-fresh")
	fresh.SetExec(func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{}, errors.New("exec unavailable")
	})

	// Act
	r.svc.RestoreWorkspace(r.ctx, r.th.ID, fresh, false)

	// Assert
	if a := r.art("/workspace/a.txt"); a.SandboxID != "sbx-fresh" {
		t.Fatalf("sandbox id %q", a.SandboxID)
	}
}
