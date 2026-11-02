package workspace_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/features/artifacts"
	"github.com/7ar-chat/harness/features/artifacts/workspace"
)

// archiveScript answers the archive scripts: HEAD is head, the tarball is size bytes.
func archiveScript(head, size string) func(sandbox.ExecRequest) (string, string, int, error) {
	return func(req sandbox.ExecRequest) (string, string, int, error) {
		switch {
		case strings.Contains(req.Command, "rev-parse --verify -q HEAD ||"):
			return head, "", 0, nil
		case strings.Contains(req.Command, "tar -czf"):
			return size, "", 0, nil
		}
		return "", "", 0, nil
	}
}

func (s *scripted) issue() *store.WorkspaceGitIssue {
	s.t.Helper()
	i, err := s.st.WorkspaceGitIssue(s.ctx, s.th.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		s.t.Fatal(err)
	}
	return i
}

func (s *scripted) archived() *store.WorkspaceGitArchive {
	s.t.Helper()
	a, err := s.st.WorkspaceGit(s.ctx, s.th.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		s.t.Fatal(err)
	}
	return a
}

func TestArchiveStoresTheTarballAndQueuesItForCollection(t *testing.T) {
	// The tarball of .git is stored under a fresh key with HEAD and size, queued for garbage collection before it is written, and its scratch copy is removed.
	// Arrange
	s := newScripted(t, workspace.Options{}, archiveScript(sha40, "7"))
	s.sb.Put("/workspace/.work/git-archive.tar.gz", "tarball")

	// Act
	s.ws.Archive(s.ctx, s.th.ID, s.sb)

	// Assert
	a := s.archived()
	if a == nil || a.Head != sha40 || a.SizeBytes != 7 || !strings.HasPrefix(a.BlobKey, "workspace-git/"+s.th.ID+"/") || !strings.HasSuffix(a.BlobKey, ".tar.gz") {
		t.Fatalf("archive %+v", a)
	}
	if data, err := s.arts.Blobs.Get(s.ctx, a.BlobKey); err != nil || string(data) != "tarball" {
		t.Fatalf("blob %q %v", data, err)
	}
	if last := s.cmds[len(s.cmds)-1]; last != "rm -f .work/git-archive.tar.gz" {
		t.Fatalf("scratch archive not removed after the upload: last command %q", last)
	}
	if s.issue() != nil {
		t.Fatalf("issue %+v", s.issue())
	}
}

func TestArchiveDoesNothingWithoutAHistoryOrWhenDisabled(t *testing.T) {
	// No repository or no commit yet, a disabled service, missing store/artifacts/sandbox: nothing is archived and no issue recorded.
	// Arrange
	noRepo := newScripted(t, workspace.Options{}, archiveScript(markNoRepo, "7"))
	off := newScripted(t, workspace.Options{Disabled: true}, archiveScript(sha40, "7"))
	noStore := newScripted(t, workspace.Options{}, archiveScript(sha40, "7"))
	noStore.ws.Store = nil
	noArts := newScripted(t, workspace.Options{}, archiveScript(sha40, "7"))
	noArts.ws.Artifacts = nil

	// Act
	noRepo.ws.Archive(noRepo.ctx, noRepo.th.ID, noRepo.sb)
	off.ws.Archive(off.ctx, off.th.ID, off.sb)
	noStore.ws.Archive(noStore.ctx, noStore.th.ID, noStore.sb)
	noArts.ws.Archive(noArts.ctx, noArts.th.ID, noArts.sb)
	noRepo.ws.Archive(noRepo.ctx, noRepo.th.ID, nil)

	// Assert
	for name, s := range map[string]*scripted{"no repository": noRepo, "disabled": off, "no artifacts": noArts} {
		if s.archived() != nil || s.issue() != nil {
			t.Errorf("%s: archived %+v issue %+v", name, s.archived(), s.issue())
		}
	}
	if noStore.count("tar -czf") != 0 {
		t.Error("archived without a store")
	}
}

func TestArchiveRecordsWhyTheHistoryIsNotSaved(t *testing.T) {
	// A too-large tarball keeps the previous archive and records the size; a script failure records "failed"; a later success clears the flag.
	// Arrange
	s := newScripted(t, workspace.Options{ArchiveMaxBytes: 100}, archiveScript(sha40, "101"))
	s.sb.Put("/workspace/.work/git-archive.tar.gz", "x")

	// Act
	s.ws.Archive(s.ctx, s.th.ID, s.sb)
	tooLarge := s.issue()
	s.mu.Lock()
	s.fn = func(sandbox.ExecRequest) (string, string, int, error) { return "", "git exploded", 1, nil }
	s.mu.Unlock()
	s.ws.Archive(s.ctx, s.th.ID, s.sb)
	failed := s.issue()
	s.mu.Lock()
	s.fn = archiveScript(sha40, "7")
	s.mu.Unlock()
	s.ws.Archive(s.ctx, s.th.ID, s.sb)

	// Assert
	if tooLarge == nil || tooLarge.Reason != store.GitIssueTooLarge || tooLarge.SizeBytes != 101 || tooLarge.Head != sha40 {
		t.Fatalf("too large issue %+v", tooLarge)
	}
	if failed == nil || failed.Reason != store.GitIssueFailed {
		t.Fatalf("failed issue %+v", failed)
	}
	if s.issue() != nil || s.archived() == nil || s.archived().Head != sha40 {
		t.Fatalf("after success: issue %+v archive %+v", s.issue(), s.archived())
	}
}

func TestArchiveCapIsTheSmallerOfTheOptionAndTheBlobBackend(t *testing.T) {
	// The blob backend's cap bounds the archive even when ArchiveMaxBytes is larger.
	// Arrange
	s := newScripted(t, workspace.Options{ArchiveMaxBytes: 1 << 30}, archiveScript(sha40, "60"))
	s.ws.Artifacts = artifacts.New(s.st, cappedBlobs{PGBlobs: artifacts.PGBlobs{S: s.st}, max: 50}, nil, artifacts.Options{})

	// Act
	s.ws.Archive(s.ctx, s.th.ID, s.sb)

	// Assert
	if i := s.issue(); i == nil || i.Reason != store.GitIssueTooLarge || s.archived() != nil {
		t.Fatalf("issue %+v archive %+v", i, s.archived())
	}
}

type cappedBlobs struct {
	artifacts.PGBlobs
	max int64
}

func (c cappedBlobs) MaxBytes() int64 { return c.max }

func TestArchiveFailuresLeaveNoStoredStateBehind(t *testing.T) {
	// Each failing step is recorded as an unsaved history and leaves no archive record; a record that cannot be written removes the blob.
	boom := errors.New("storage down")
	cases := []struct {
		name  string
		setup func(s *scripted)
	}{
		{"unparsable size", func(s *scripted) { s.fn = archiveScript(sha40, "lots") }},
		{"head lookup fails", func(s *scripted) {
			s.fn = func(sandbox.ExecRequest) (string, string, int, error) { return "", "fatal", 1, nil }
		}},
		{"archive read fails", func(s *scripted) { s.sb.Remove("/workspace/.work/git-archive.tar.gz") }},
		{"previous archive unreadable", func(s *scripted) { s.fs.getErr = boom }},
		{"blob write fails", func(s *scripted) {
			s.ws.Artifacts = artifacts.New(s.st, failingPut{PGBlobs: artifacts.PGBlobs{S: s.st}, err: boom}, nil, artifacts.Options{})
		}},
		{"record write fails", func(s *scripted) { s.fs.putErr = boom }},
		{"tarball grew past the cap", func(s *scripted) {
			s.fn = archiveScript(sha40, "3")
			s.sb.Put("/workspace/.work/git-archive.tar.gz", "way more than three bytes")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			s := newScripted(t, workspace.Options{ArchiveMaxBytes: 20}, archiveScript(sha40, "7"))
			s.sb.Put("/workspace/.work/git-archive.tar.gz", "tarball")
			rec := &recordingBlobs{PGBlobs: artifacts.PGBlobs{S: s.st}}
			s.ws.Artifacts = artifacts.New(s.st, rec, nil, artifacts.Options{})
			c.setup(s)

			// Act
			s.ws.Archive(s.ctx, s.th.ID, s.sb)

			// Assert
			if i := s.issue(); i == nil || i.Reason != store.GitIssueFailed {
				t.Fatalf("issue %+v", i)
			}
			if s.archived() != nil && c.name != "previous archive unreadable" {
				t.Fatalf("archive record %+v", s.archived())
			}
			if c.name == "record write fails" {
				if len(rec.keys()) == 0 {
					t.Fatal("the archive blob was never written, so the orphan check proves nothing")
				}
				for _, k := range rec.keys() {
					if s.st.Blob(k) {
						t.Fatalf("the orphan blob %q was kept", k)
					}
				}
			}
		})
	}
}

// recordingBlobs remembers the keys written so a test can check none was left behind.
type recordingBlobs struct {
	artifacts.PGBlobs
	mu  sync.Mutex
	put []string
}

func (r *recordingBlobs) Put(ctx context.Context, key string, data []byte) error {
	r.mu.Lock()
	r.put = append(r.put, key)
	r.mu.Unlock()
	return r.PGBlobs.Put(ctx, key, data)
}

func (r *recordingBlobs) keys() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.put...)
}

type failingPut struct {
	artifacts.PGBlobs
	err error
}

func (f failingPut) Put(context.Context, string, []byte) error { return f.err }

func TestArchiveSurvivesBookkeepingFailures(t *testing.T) {
	// If the issue flag cannot be written or cleared, the archive still happens (best effort).
	boom := errors.New("db down")
	// Arrange
	s := newScripted(t, workspace.Options{}, archiveScript(sha40, "7"))
	s.sb.Put("/workspace/.work/git-archive.tar.gz", "tarball")
	s.fs.clearErr, s.fs.setIssueErr = boom, boom

	// Act
	s.ws.Archive(s.ctx, s.th.ID, s.sb)
	first := s.archived()
	s.ws.Archive(s.ctx, s.th.ID, s.sb) // same HEAD: only the flag is cleared (and fails)
	s.mu.Lock()
	s.fn = func(sandbox.ExecRequest) (string, string, int, error) { return "", "fatal", 1, nil }
	s.mu.Unlock()
	s.ws.Archive(s.ctx, s.th.ID, s.sb) // fails; the issue cannot be recorded

	// Assert
	if first == nil || first.Head != sha40 || s.archived().BlobKey != first.BlobKey {
		t.Fatalf("archive %+v", first)
	}
}

func TestArchiveAsyncCoalescesRequestsAndArchivesTheLatestSandbox(t *testing.T) {
	// While an archive runs, further requests collapse into one more archive of the newest sandbox; WaitArchives reports completion or its context ending.
	// Arrange
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var heads []string
	s := newScripted(t, workspace.Options{}, nil)
	s.fn = func(req sandbox.ExecRequest) (string, string, int, error) {
		if strings.Contains(req.Command, "rev-parse --verify -q HEAD ||") {
			once.Do(func() { close(started); <-release })
			mu.Lock()
			heads = append(heads, req.Command)
			mu.Unlock()
			return markNoRepo, "", 0, nil
		}
		return "", "", 0, nil
	}

	// Act
	s.ws.ArchiveAsync(s.th.ID, s.sb)
	<-started
	for range 5 {
		s.ws.ArchiveAsync(s.th.ID, s.sb) // coalesced
	}
	cancelled, cancel := context.WithCancel(s.ctx)
	cancel()
	waitedWhileRunning := s.ws.WaitArchives(cancelled)
	close(release)
	done := s.ws.WaitArchives(s.ctx)

	// Assert
	mu.Lock()
	n := len(heads)
	mu.Unlock()
	if waitedWhileRunning || !done {
		t.Fatalf("WaitArchives while running %v, after %v", waitedWhileRunning, done)
	}
	if n != 2 {
		t.Fatalf("archives run: %d, want the running one plus one coalesced", n)
	}
}

func TestArchiveAsyncIgnoresUnarchivableRequests(t *testing.T) {
	// Disabled, store-less, artifact-less services and a missing sandbox start nothing; a nil service has nothing to wait for.
	// Arrange
	off := newScripted(t, workspace.Options{Disabled: true}, always(""))
	noStore := newScripted(t, workspace.Options{}, always(""))
	noStore.ws.Store = nil
	noArts := newScripted(t, workspace.Options{}, always(""))
	noArts.ws.Artifacts = nil
	noSB := newScripted(t, workspace.Options{}, always(""))
	var nilSvc *workspace.Service

	// Act
	off.ws.ArchiveAsync(off.th.ID, off.sb)
	noStore.ws.ArchiveAsync(noStore.th.ID, noStore.sb)
	noArts.ws.ArchiveAsync(noArts.th.ID, noArts.sb)
	noSB.ws.ArchiveAsync(noSB.th.ID, nil)
	for _, s := range []*scripted{off, noStore, noArts, noSB} {
		s.ws.WaitArchives(s.ctx)
	}

	// Assert
	for _, s := range []*scripted{off, noStore, noArts, noSB} {
		if len(s.cmds) != 0 {
			t.Fatalf("an archive ran: %q", s.cmds)
		}
	}
	if !nilSvc.WaitArchives(context.Background()) {
		t.Fatal("a nil service has nothing to wait for")
	}
}

// restoreRig is a scripted service with an archive stored for the thread.
func restoreRig(t *testing.T, result string) *scripted {
	t.Helper()
	s := newScripted(t, workspace.Options{}, func(req sandbox.ExecRequest) (string, string, int, error) {
		if strings.Contains(req.Command, "git-restore.tar.gz") {
			return result, "", 0, nil
		}
		return "", "", 0, nil
	})
	if err := s.st.PutBlob(s.ctx, "workspace-git/k.tar.gz", []byte("tarball")); err != nil {
		t.Fatal(err)
	}
	if err := s.st.PutWorkspaceGit(s.ctx, store.WorkspaceGitArchive{ThreadID: s.th.ID, BlobKey: "workspace-git/k.tar.gz", Head: sha40}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRestoreHistoryWritesTheArchiveIntoANewSandbox(t *testing.T) {
	// The stored tarball is written to the scratch path and extracted by the script; every outcome is best effort and never panics.
	for _, result := range []string{"restored", "exists", "broken", ""} {
		t.Run("script says "+result, func(t *testing.T) {
			// Arrange
			s := restoreRig(t, result)

			// Act
			s.ws.RestoreHistory(s.ctx, s.th.ID, s.sb)

			// Assert
			if data, ok := s.sb.File("/workspace/.work/git-restore.tar.gz"); !ok || data != "tarball" {
				t.Fatalf("tarball written as %q %v", data, ok)
			}
			if s.count("tar -xzf") != 1 {
				t.Fatalf("extract script ran %d times", s.count("tar -xzf"))
			}
		})
	}
}

func TestRestoreHistoryDoesNothingWithoutAUsableArchive(t *testing.T) {
	// No archive, a lost blob, a failing store, a refused write, a disabled service: nothing is extracted.
	boom := errors.New("down")
	cases := map[string]func(s *scripted){
		"no archive": func(s *scripted) {
			_ = s.st.PutWorkspaceGit(s.ctx, store.WorkspaceGitArchive{})
			s.fs.getErr = store.ErrNotFound
		},
		"store failure": func(s *scripted) { s.fs.getErr = boom },
		"lost blob":     func(s *scripted) { _ = s.st.DeleteBlob(s.ctx, "workspace-git/k.tar.gz") },
		"write refused": func(s *scripted) { s.sb.SetWriteErr(func(string) error { return boom }) },
		"disabled":      func(s *scripted) { s.ws.Opts.Disabled = true },
		"no artifacts":  func(s *scripted) { s.ws.Artifacts = nil },
		"no store":      func(s *scripted) { s.ws.Store = nil },
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			// Arrange
			s := restoreRig(t, "restored")
			set(s)

			// Act
			s.ws.RestoreHistory(s.ctx, s.th.ID, s.sb)

			// Assert
			if s.count("tar -xzf") != 0 {
				t.Fatal("the archive was extracted")
			}
		})
	}
}

func TestRestoreHistoryStillRestoresWithoutTheGitLock(t *testing.T) {
	// A lock that cannot be taken is logged; the restore continues (the sandbox is brand new, nobody else uses it).
	// Arrange
	s := restoreRig(t, "restored")
	s.ws.Locks = failLocker{err: store.ErrLockTimeout}

	// Act
	s.ws.RestoreHistory(s.ctx, s.th.ID, s.sb)

	// Assert
	if s.count("tar -xzf") != 1 {
		t.Fatal("the history was not restored")
	}
}

func TestRestoreHistoryFailingScriptIsNotFatal(t *testing.T) {
	// A script that errors leaves the sandbox as it is.
	// Arrange
	s := restoreRig(t, "")
	s.fn = func(sandbox.ExecRequest) (string, string, int, error) { return "", "tar: error", 2, nil }

	// Act
	s.ws.RestoreHistory(s.ctx, s.th.ID, s.sb)

	// Assert
	if s.count("tar -xzf") != 1 {
		t.Fatal("the extract script did not run")
	}
}

func TestOnCreateRestoresTheArchivedHistory(t *testing.T) {
	// The sandbox creation hook is the history restore.
	// Arrange
	s := restoreRig(t, "restored")

	// Act
	s.ws.OnCreate(s.ctx, s.th.ID, s.sb)

	// Assert
	if s.count("tar -xzf") != 1 {
		t.Fatal("OnCreate did not restore the history")
	}
}

func TestStatusAvailabilityFollowsTheSandboxAndSavedState(t *testing.T) {
	// ready with a sandbox; stopped/missing without one depending on saved files or history; starting while unavailable; missing with sandboxes disabled; provider errors are returned.
	boom := errors.New("provider down")
	cases := []struct {
		name  string
		setup func(s *scripted)
		want  string
		err   error
	}{
		{"ready", func(s *scripted) {}, workspace.AvailabilityReady, nil},
		{"stopped with files", func(s *scripted) { s.mgr.sb = nil; saveFile(s) }, workspace.AvailabilityStopped, nil},
		{"stopped with history", func(s *scripted) {
			s.mgr.sb = nil
			_ = s.st.PutWorkspaceGit(s.ctx, store.WorkspaceGitArchive{ThreadID: s.th.ID, BlobKey: "k", Head: sha40})
		}, workspace.AvailabilityStopped, nil},
		{"missing", func(s *scripted) { s.mgr.sb = nil }, workspace.AvailabilityMissing, nil},
		{"starting", func(s *scripted) { s.mgr.getErr = sandbox.ErrUnavailable }, workspace.AvailabilityStarting, nil},
		{"sandboxes disabled", func(s *scripted) { s.mgr.disabled = true; saveFile(s) }, workspace.AvailabilityMissing, nil},
		{"provider failure", func(s *scripted) { s.mgr.getErr = boom }, "", boom},
		{"artifact store failure", func(s *scripted) {
			s.ws.Artifacts = artifacts.New(&brokenArtifacts{Store: s.st}, nil, nil, artifacts.Options{})
		}, "", errors.New("db down")},
		{"archive store failure", func(s *scripted) { s.fs.getErr = boom }, "", boom},
		{"issue store failure", func(s *scripted) { s.fs.issueErr = boom }, "", boom},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			s := newScripted(t, workspace.Options{}, always(""))
			c.setup(s)

			// Act
			st, err := s.ws.Status(s.ctx, s.th)

			// Assert
			if c.err != nil {
				if err == nil {
					t.Fatalf("status %+v, want an error", st)
				}
				return
			}
			if err != nil || st.Availability != c.want || st.Status != c.want || st.Ready != (c.want == workspace.AvailabilityReady) {
				t.Fatalf("status %+v %v", st, err)
			}
		})
	}
}

func saveFile(s *scripted) {
	_, err := s.arts.Upload(s.ctx, artifacts.UploadInput{ThreadID: s.th.ID, Login: "alice", Filename: "a.txt", Data: []byte("a")})
	if err != nil {
		s.t.Fatal(err)
	}
}

func TestStatusIgnoresDeletedFilesAndDisabledHistory(t *testing.T) {
	// A deleted artifact is nothing to restore; with checkpoints disabled the history and issue are not consulted.
	// Arrange
	s := newScripted(t, workspace.Options{}, always(""))
	s.mgr.sb = nil
	s.sb.Put("/workspace/a.txt", "a")
	if _, err := s.arts.Snapshot(s.ctx, s.sb, artifacts.RunRef{ThreadID: s.th.ID, Login: "alice"}); err != nil {
		t.Fatal(err)
	}
	s.sb.Remove("/workspace/a.txt")
	if _, err := s.arts.Snapshot(s.ctx, s.sb, artifacts.RunRef{ThreadID: s.th.ID, Login: "alice"}); err != nil {
		t.Fatal(err)
	}
	_ = s.st.PutWorkspaceGit(s.ctx, store.WorkspaceGitArchive{ThreadID: s.th.ID, BlobKey: "k", Head: sha40})
	s.ws.Opts.Disabled = true

	// Act
	st, err := s.ws.Status(s.ctx, s.th)

	// Assert
	if err != nil || st.Availability != workspace.AvailabilityMissing || st.CanRestore || st.HasHistory {
		t.Fatalf("status %+v %v", st, err)
	}
}

func TestStatusReportsWhyTheHistoryIsNotSaved(t *testing.T) {
	// The recorded archive issue is exposed with its reason, size and time.
	// Arrange
	s := newScripted(t, workspace.Options{}, always(""))
	_ = s.st.SetWorkspaceGitIssue(s.ctx, store.WorkspaceGitIssue{ThreadID: s.th.ID, Reason: store.GitIssueTooLarge, SizeBytes: 123})

	// Act
	st, err := s.ws.Status(s.ctx, s.th)

	// Assert
	if err != nil || st.HistoryNotSaved == nil || st.HistoryNotSaved.Reason != store.GitIssueTooLarge || st.HistoryNotSaved.SizeBytes != 123 {
		t.Fatalf("status %+v %v", st, err)
	}
	if _, perr := time.Parse(time.RFC3339, st.HistoryNotSaved.At); perr != nil {
		t.Fatalf("time %q", st.HistoryNotSaved.At)
	}
}

func TestStatusOfAFinishedRunDoesNotBlockRestore(t *testing.T) {
	// Only a queued or running run blocks the restore.
	// Arrange
	s := newScripted(t, workspace.Options{}, always(""))
	thread := *s.th
	thread.ActiveRun = &store.ActiveRun{ID: "r", Status: store.StatusCompleted}

	// Act
	st, err := s.ws.Status(s.ctx, &thread)

	// Assert
	if err != nil || st.RestoreBlockedReason != nil {
		t.Fatalf("status %+v %v", st, err)
	}
}

func TestRestoreAcquiresAQuietSandboxAndFailsCleanly(t *testing.T) {
	// A stopped workspace is recreated without telling the model; a status failure, disabled sandboxes and an acquisition failure are returned.
	boom := errors.New("quota reached")
	t.Run("recreates", func(t *testing.T) {
		// Arrange
		s := newScripted(t, workspace.Options{}, always(""))
		s.mgr.sb = nil
		saveFile(s)
		s.mgr.acqErr = nil
		restored := &quietRecorder{wsMgr: s.mgr, sb: s.sb}
		s.ws.Sandbox = restored

		// Act
		out, err := s.ws.Restore(s.ctx, s.th)

		// Assert
		if err != nil || !out.Restored || out.RestoreDiagnostic.Status != "restored" || out.RestoreDiagnostic.Message != "Workspace restored." || !out.Ready {
			t.Fatalf("outcome %+v %v", out, err)
		}
		if !restored.quiet {
			t.Fatal("the restore must not leave a notice for the model")
		}
	})
	t.Run("acquire failure", func(t *testing.T) {
		// Arrange
		s := newScripted(t, workspace.Options{}, always(""))
		s.mgr.sb = nil
		saveFile(s)
		s.mgr.acqErr = boom

		// Act
		_, err := s.ws.Restore(s.ctx, s.th)

		// Assert
		if !errors.Is(err, boom) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("status failure", func(t *testing.T) {
		// Arrange
		s := newScripted(t, workspace.Options{}, always(""))
		s.mgr.getErr = boom

		// Act
		_, err := s.ws.Restore(s.ctx, s.th)

		// Assert
		if !errors.Is(err, boom) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("sandboxes disabled", func(t *testing.T) {
		// Arrange: a manager that reports disabled only after the status was computed
		s := newScripted(t, workspace.Options{}, always(""))
		s.mgr.sb = nil
		saveFile(s)
		flip := &disableAfterStatus{wsMgr: s.mgr}
		s.ws.Sandbox = flip

		// Act
		_, err := s.ws.Restore(s.ctx, s.th)

		// Assert
		if !errors.Is(err, sandbox.ErrDisabled) {
			t.Fatalf("err %v", err)
		}
	})
}

// quietRecorder records whether Acquire got a quiet context and serves sb afterwards.
type quietRecorder struct {
	*wsMgr
	sb    sandbox.Sandbox
	quiet bool
}

func (q *quietRecorder) Acquire(ctx context.Context, _, _ string) (sandbox.Sandbox, error) {
	q.quiet = sandbox.Quieted(ctx)
	q.wsMgr.mu.Lock()
	q.wsMgr.sb = q.sb
	q.wsMgr.mu.Unlock()
	return q.sb, nil
}

// disableAfterStatus is enabled for the status check (one read) and disabled after.
type disableAfterStatus struct {
	*wsMgr
	mu    sync.Mutex
	calls int
}

func (d *disableAfterStatus) Enabled() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	return d.calls <= 1
}

func TestArchiveIsRecordedAsFailedWhenTheGitLockCannotBeTaken(t *testing.T) {
	// A lock that cannot be taken means no archive this time and an unsaved-history flag (the previous archive stays).
	// Arrange
	s := newScripted(t, workspace.Options{}, archiveScript(sha40, "7"))
	s.ws.Locks = failLocker{err: store.ErrLockTimeout}

	// Act
	s.ws.Archive(s.ctx, s.th.ID, s.sb)

	// Assert
	if i := s.issue(); i == nil || i.Reason != store.GitIssueFailed || s.archived() != nil || s.count("tar -czf") != 0 {
		t.Fatalf("issue %+v archive %+v", i, s.archived())
	}
}

func TestListReportsAFailingScript(t *testing.T) {
	// A history script that fails is a GitError, not an empty history.
	// Arrange
	s := newScripted(t, workspace.Options{}, func(sandbox.ExecRequest) (string, string, int, error) {
		return "", "fatal: not a git repository", 128, nil
	})

	// Act
	list, err := s.ws.List(s.ctx, s.th.ID, "a.txt")

	// Assert
	var gitErr *workspace.GitError
	if !errors.As(err, &gitErr) || list != nil {
		t.Fatalf("list %v err %v", list, err)
	}
}

func TestBeforeTurnWithoutGitInTheImageDoesNotFailTheRun(t *testing.T) {
	// A sandbox image without git answers no_git: the run goes on and nothing is recorded as a checkpoint.
	// Arrange
	s := newScripted(t, workspace.Options{}, always(markNoGit+"\n"))

	// Act
	s.ws.BeforeTurn(s.ctx, s.th.ID, "run-1", s.sb)

	// Assert
	if s.count("g add -A") != 1 {
		t.Fatalf("commands %d", len(s.cmds))
	}
}

func TestRestoreReportsAStatusFailureAfterTheSandboxWasCreated(t *testing.T) {
	// If the status cannot be computed after the sandbox came back, the error is returned rather than a stale outcome.
	boom := errors.New("db down")
	// Arrange
	s := newScripted(t, workspace.Options{}, always(""))
	s.mgr.sb = nil
	saveFile(s)
	s.ws.Sandbox = &breakAfterAcquire{wsMgr: s.mgr, sb: s.sb, err: boom}

	// Act
	_, err := s.ws.Restore(s.ctx, s.th)

	// Assert
	if !errors.Is(err, boom) {
		t.Fatalf("err %v", err)
	}
}

// breakAfterAcquire creates the sandbox, then fails every later lookup.
type breakAfterAcquire struct {
	*wsMgr
	sb  sandbox.Sandbox
	err error
	mu  sync.Mutex
	got bool
}

func (b *breakAfterAcquire) Acquire(context.Context, string, string) (sandbox.Sandbox, error) {
	b.mu.Lock()
	b.got = true
	b.mu.Unlock()
	return b.sb, nil
}

func (b *breakAfterAcquire) Get(ctx context.Context, id string) (sandbox.Sandbox, error) {
	b.mu.Lock()
	acquired := b.got
	b.mu.Unlock()
	if acquired {
		return nil, b.err
	}
	return b.wsMgr.Get(ctx, id)
}
