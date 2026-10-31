package workspace_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/runtime"
	"github.com/7ar-chat/harness/engine/syncx"
	"github.com/7ar-chat/harness/engine/tools"
	"github.com/7ar-chat/harness/features/artifacts"
	"github.com/7ar-chat/harness/features/artifacts/workspace"
)

// Markers printed by the checkpoint scripts (their last stdout line).
const (
	markNoGit       = "__WC_NO_GIT__"
	markNoChanges   = "__WC_CHECKPOINT_NO_CHANGES__"
	markNoRepo      = "__WC_NO_REPOSITORY__"
	markUnknownSHA  = "__WC_UNKNOWN_CHECKPOINT__"
	markRestoreNoop = "__WC_RESTORE_NO_CHANGES__"
	markRestoreOK   = "__WC_RESTORE_OK__"
	markRestoreDel  = "__WC_RESTORE_DELETED__"
	markRestoreBad  = "__WC_RESTORE_INVALID__"
)

var (
	sha40  = strings.Repeat("a", 40)
	sha40b = strings.Repeat("b", 40)
)

// wsMgr is a sandbox manager handing out one scripted sandbox.
type wsMgr struct {
	mu       sync.Mutex
	disabled bool
	sb       sandbox.Sandbox
	getErr   error
	acqErr   error
	gets     int
	acquires int
}

func (m *wsMgr) Enabled() bool { return !m.disabled }

func (m *wsMgr) Acquire(context.Context, string, string) (sandbox.Sandbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.acquires++
	if m.acqErr != nil {
		return nil, m.acqErr
	}
	return m.sb, nil
}

func (m *wsMgr) Get(context.Context, string) (sandbox.Sandbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gets++
	if m.getErr != nil {
		return nil, m.getErr
	}
	if m.sb == nil {
		return nil, sandbox.ErrNoSandbox
	}
	return m.sb, nil
}

func (m *wsMgr) Kill(context.Context, string) error         { return nil }
func (m *wsMgr) Invalidate(context.Context, string, string) {}
func (m *wsMgr) getCalls() int                              { m.mu.Lock(); defer m.mu.Unlock(); return m.gets }

// failStore wraps a memstore, failing chosen workspace-git operations.
type failStore struct {
	*memstore.Store
	getErr, putErr, issueErr, setIssueErr, clearErr error
}

func (f *failStore) WorkspaceGit(ctx context.Context, id string) (*store.WorkspaceGitArchive, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.Store.WorkspaceGit(ctx, id)
}

func (f *failStore) PutWorkspaceGit(ctx context.Context, a store.WorkspaceGitArchive) error {
	if f.putErr != nil {
		return f.putErr
	}
	return f.Store.PutWorkspaceGit(ctx, a)
}

func (f *failStore) WorkspaceGitIssue(ctx context.Context, id string) (*store.WorkspaceGitIssue, error) {
	if f.issueErr != nil {
		return nil, f.issueErr
	}
	return f.Store.WorkspaceGitIssue(ctx, id)
}

func (f *failStore) SetWorkspaceGitIssue(ctx context.Context, i store.WorkspaceGitIssue) error {
	if f.setIssueErr != nil {
		return f.setIssueErr
	}
	return f.Store.SetWorkspaceGitIssue(ctx, i)
}

func (f *failStore) ClearWorkspaceGitIssue(ctx context.Context, id string) error {
	if f.clearErr != nil {
		return f.clearErr
	}
	return f.Store.ClearWorkspaceGitIssue(ctx, id)
}

// failLocker is a cluster lock that cannot be taken.
type failLocker struct{ err error }

func (f failLocker) AdvisoryLock(context.Context, string, time.Duration) (func(), error) {
	return nil, f.err
}

// scripted is a service over a sandbox whose commands are answered by a script
// (no git needed); every command is recorded.
type scripted struct {
	t    *testing.T
	ctx  context.Context
	st   *memstore.Store
	fs   *failStore
	arts *artifacts.Service
	mgr  *wsMgr
	sb   *sandboxtest.Sandbox
	ws   *workspace.Service
	th   *store.Thread

	mu   sync.Mutex
	cmds []string
	fn   func(req sandbox.ExecRequest) (string, string, int, error)
}

func newScripted(t *testing.T, o workspace.Options, fn func(req sandbox.ExecRequest) (stdout, stderr string, exit int, err error)) *scripted {
	t.Helper()
	s := &scripted{t: t, ctx: context.Background(), st: memstore.New(), fn: fn}
	s.fs = &failStore{Store: s.st}
	s.arts = artifacts.New(s.st, artifacts.PGBlobs{S: s.st}, nil, artifacts.Options{})
	s.sb = sandboxtest.NewSandbox("sbx-1")
	s.mgr = &wsMgr{sb: s.sb}
	s.sb.SetExec(func(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
		s.mu.Lock()
		s.cmds = append(s.cmds, req.Command)
		fn := s.fn
		s.mu.Unlock()
		if fn == nil {
			return sandbox.ExecResult{}, nil
		}
		out, errOut, exit, err := fn(req)
		if err != nil {
			return sandbox.ExecResult{}, err
		}
		if out != "" && req.OnStdout != nil {
			req.OnStdout(out)
		}
		if errOut != "" && req.OnStderr != nil {
			req.OnStderr(errOut)
		}
		return sandbox.ExecResult{ExitCode: exit}, nil
	})
	s.ws = workspace.New(s.mgr, s.fs, nil, s.arts, o)
	th, err := s.st.CreateThread(s.ctx, "alice", "t")
	if err != nil {
		t.Fatal(err)
	}
	s.th = th
	return s
}

// count is how many recorded commands contain sub.
func (s *scripted) count(sub string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.cmds {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

// always makes every command print out and exit 0.
func always(out string) func(sandbox.ExecRequest) (string, string, int, error) {
	return func(sandbox.ExecRequest) (string, string, int, error) { return out, "", 0, nil }
}

func TestCheckpointReportsEveryOutcomeOfTheScript(t *testing.T) {
	// The last stdout line decides: a sha is a new checkpoint, the markers mean no changes or no git, anything else is a GitError.
	cases := []struct {
		name    string
		out     string
		want    workspace.CheckpointResult
		wantErr string
	}{
		{"created", "chatter\n" + sha40 + "\n", workspace.CheckpointResult{Created: true, SHA: sha40}, ""},
		{"sha256 repository", strings.Repeat("c", 64) + "\n", workspace.CheckpointResult{Created: true, SHA: strings.Repeat("c", 64)}, ""},
		{"no changes", markNoChanges + "\n", workspace.CheckpointResult{Reason: "no_changes"}, ""},
		{"no git", markNoGit + "\n", workspace.CheckpointResult{Reason: "no_git"}, ""},
		{"garbage", "fatal: something\n", workspace.CheckpointResult{}, "unexpected output: fatal: something"},
		{"short sha", "abc123\n", workspace.CheckpointResult{}, "unexpected output"},
		{"empty output", "", workspace.CheckpointResult{}, "unexpected output"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			s := newScripted(t, workspace.Options{}, always(c.out))

			// Act
			got, err := s.ws.Checkpoint(s.ctx, s.th.ID, s.sb, "Label")

			// Assert
			var gitErr *workspace.GitError
			if c.wantErr == "" && (err != nil || got != c.want) {
				t.Fatalf("got %+v %v", got, err)
			}
			if c.wantErr != "" && (!errors.As(err, &gitErr) || !strings.Contains(gitErr.Detail, c.wantErr)) {
				t.Fatalf("err %v", err)
			}
		})
	}
}

func TestCheckpointRefusesBadLabelsAndSkipsWhenDisabled(t *testing.T) {
	// A label with control characters never reaches a script; a disabled service runs nothing.
	// Arrange
	s := newScripted(t, workspace.Options{}, always(sha40))
	off := newScripted(t, workspace.Options{Disabled: true}, always(sha40))

	// Act
	_, newline := s.ws.Checkpoint(s.ctx, s.th.ID, s.sb, "two\nlines")
	_, del := s.ws.Checkpoint(s.ctx, s.th.ID, s.sb, "bell\x7f")
	disabled, disabledErr := off.ws.Checkpoint(off.ctx, off.th.ID, off.sb, "Label")

	// Assert
	if newline == nil || del == nil || len(s.cmds) != 0 {
		t.Fatalf("labels accepted: %v %v, commands %d", newline, del, len(s.cmds))
	}
	if disabledErr != nil || disabled.Reason != "disabled" || len(off.cmds) != 0 {
		t.Fatalf("disabled: %+v %v commands %d", disabled, disabledErr, len(off.cmds))
	}
}

func TestScriptFailuresCarryGitsMessage(t *testing.T) {
	// A non-zero exit is a GitError with git's stderr, or the exit status when git said nothing; an exec error is returned as is.
	boom := errors.New("sandbox exec channel closed")
	cases := []struct {
		name   string
		fn     func(sandbox.ExecRequest) (string, string, int, error)
		detail string
		err    error
	}{
		{"stderr", func(sandbox.ExecRequest) (string, string, int, error) { return "", "  fatal: bad object \n", 128, nil }, "fatal: bad object", nil},
		{"silent failure", func(sandbox.ExecRequest) (string, string, int, error) { return "", "", 3, nil }, "exit status 3", nil},
		{"exec error", func(sandbox.ExecRequest) (string, string, int, error) { return "", "", 0, boom }, "", boom},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			s := newScripted(t, workspace.Options{}, c.fn)

			// Act
			_, err := s.ws.Checkpoint(s.ctx, s.th.ID, s.sb, "L")

			// Assert
			var gitErr *workspace.GitError
			if c.err != nil && !errors.Is(err, c.err) {
				t.Fatalf("err %v", err)
			}
			if c.err == nil && (!errors.As(err, &gitErr) || gitErr.Detail != c.detail) {
				t.Fatalf("err %v", err)
			}
		})
	}
}

func TestGitLockWaitsAreBoundedAndReleasedOnFailure(t *testing.T) {
	// A checkpoint queued behind another of the same thread times out; a cluster lock that cannot be taken fails the call without leaving the local lock held.
	// Arrange
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s := newScripted(t, workspace.Options{LockWait: 20 * time.Millisecond}, func(sandbox.ExecRequest) (string, string, int, error) {
		once.Do(func() { close(started) })
		<-release
		return markNoChanges, "", 0, nil
	})
	done := make(chan error, 1)
	go func() { _, err := s.ws.Checkpoint(s.ctx, s.th.ID, s.sb, "first"); done <- err }()
	<-started

	// Act
	_, timeout := s.ws.Checkpoint(s.ctx, s.th.ID, s.sb, "second")
	close(release)
	holder := <-done
	s.ws.Locks = failLocker{err: store.ErrLockTimeout}
	_, cluster := s.ws.Checkpoint(s.ctx, s.th.ID, s.sb, "third")
	s.ws.Locks = nil
	_, after := s.ws.Checkpoint(s.ctx, s.th.ID, s.sb, "fourth")

	// Assert
	if !errors.Is(timeout, syncx.ErrLockTimeout) || !strings.Contains(timeout.Error(), "git lock") {
		t.Fatalf("local timeout %v", timeout)
	}
	if holder != nil {
		t.Fatalf("holder %v", holder)
	}
	if !errors.Is(cluster, store.ErrLockTimeout) {
		t.Fatalf("cluster lock %v", cluster)
	}
	if after != nil {
		t.Fatalf("the local lock was left held after a cluster lock failure: %v", after)
	}
}

func TestBeforeTurnIgnoresIncompleteRequestsAndBoundsItsMemory(t *testing.T) {
	// No sandbox or run id means no checkpoint; once-markers of runs that never ended are dropped past 10000 so their run can checkpoint again.
	// Arrange
	s := newScripted(t, workspace.Options{}, always(markNoChanges))

	// Act
	s.ws.BeforeTurn(s.ctx, s.th.ID, "", s.sb)
	s.ws.BeforeTurn(s.ctx, s.th.ID, "run-x", nil)
	none := s.count("g add -A")
	s.ws.BeforeTurn(s.ctx, s.th.ID, "run-0", s.sb)
	s.ws.BeforeTurn(s.ctx, s.th.ID, "run-0", s.sb)
	once := s.count("g add -A")
	for i := 1; i <= 10001; i++ {
		s.ws.BeforeTurn(s.ctx, s.th.ID, "run-"+strconv.Itoa(i), s.sb)
	}
	before := s.count("g add -A")
	s.ws.BeforeTurn(s.ctx, s.th.ID, "run-0", s.sb)

	// Assert
	if none != 0 || once != 1 {
		t.Fatalf("incomplete requests ran %d checkpoints; the same run twice ran %d", none, once)
	}
	if after := s.count("g add -A"); after != before+1 {
		t.Fatalf("a run forgotten by the bound did not checkpoint again (%d → %d)", before, after)
	}
}

func TestHooksCheckpointLiveSandboxesOnly(t *testing.T) {
	// BeforeRun skips runs without sandbox and disabled services and tolerates an unavailable one; OnAcquire checkpoints the sandbox a run just got.
	t.Run("sandbox off", func(t *testing.T) {
		// Arrange
		s := newScripted(t, workspace.Options{}, always(markNoChanges))

		// Act
		s.ws.BeforeRun(s.ctx, &store.Run{ID: "r", ThreadID: s.th.ID, Sandbox: store.SandboxOff})

		// Assert
		if s.mgr.getCalls() != 0 || len(s.cmds) != 0 {
			t.Fatalf("gets %d commands %d", s.mgr.getCalls(), len(s.cmds))
		}
	})
	t.Run("disabled service", func(t *testing.T) {
		// Arrange
		s := newScripted(t, workspace.Options{Disabled: true}, always(markNoChanges))

		// Act
		s.ws.BeforeRun(s.ctx, &store.Run{ID: "r", ThreadID: s.th.ID})

		// Assert
		if s.mgr.getCalls() != 0 || len(s.cmds) != 0 {
			t.Fatalf("gets %d commands %d", s.mgr.getCalls(), len(s.cmds))
		}
	})
	t.Run("sandbox unavailable", func(t *testing.T) {
		// Arrange
		s := newScripted(t, workspace.Options{}, always(markNoChanges))
		s.mgr.getErr = sandbox.ErrUnavailable

		// Act
		s.ws.BeforeRun(s.ctx, &store.Run{ID: "r", ThreadID: s.th.ID})

		// Assert
		if len(s.cmds) != 0 {
			t.Fatalf("commands %d", len(s.cmds))
		}
	})
	t.Run("acquisition by a tool", func(t *testing.T) {
		// Arrange
		s := newScripted(t, workspace.Options{}, always(markNoChanges))

		// Act
		s.ws.OnAcquire(s.ctx, tools.RunContext{ThreadID: s.th.ID, RunID: "r1"}, s.sb)
		s.ws.OnAcquire(s.ctx, tools.RunContext{ThreadID: s.th.ID, RunID: "r1"}, s.sb)

		// Assert
		if n := s.count("g add -A"); n != 1 {
			t.Fatalf("checkpoints %d, want 1 per run and sandbox", n)
		}
	})
}

func TestEndOfRunHookArchivesInTheBackgroundAndForgetsTheRun(t *testing.T) {
	// A finished run is archived without waiting; its once-marker is dropped so the thread's next run with the same id would checkpoint again; compaction runs only forget.
	// Arrange
	s := newScripted(t, workspace.Options{}, func(req sandbox.ExecRequest) (string, string, int, error) {
		switch {
		case strings.Contains(req.Command, "g add -A"):
			return markNoChanges, "", 0, nil
		case strings.Contains(req.Command, "rev-parse --verify -q HEAD ||"):
			return markNoRepo, "", 0, nil
		}
		return "", "", 0, nil
	})
	hook := workspace.EndOfRunHook{Svc: s.ws}
	run := &store.Run{ID: "run-1", ThreadID: s.th.ID, UserLogin: "alice"}
	s.ws.BeforeTurn(s.ctx, s.th.ID, run.ID, s.sb)

	// Act
	next := hook.Finishing(s.ctx, &runtime.Finish{Run: run})
	archived := s.ws.WaitArchives(s.ctx)
	archives := s.count("rev-parse --verify -q HEAD ||")
	s.ws.BeforeTurn(s.ctx, s.th.ID, run.ID, s.sb) // would be skipped if the run had not been forgotten
	compact := hook.Finishing(s.ctx, &runtime.Finish{Run: &store.Run{ID: "run-2", ThreadID: s.th.ID, Kind: store.KindCompact}})
	nilSvc := workspace.EndOfRunHook{}.Finishing(s.ctx, &runtime.Finish{Run: run})

	// Assert
	if next != nil || compact != nil || nilSvc != nil || !archived {
		t.Fatalf("continuations registered: %v %v %v, archived %v", next != nil, compact != nil, nilSvc != nil, archived)
	}
	if archives != 1 || s.count("g add -A") != 2 {
		t.Fatalf("archives %d checkpoints %d", archives, s.count("g add -A"))
	}
	if after := s.count("rev-parse --verify -q HEAD ||"); after != 1 {
		t.Fatalf("a compaction run archived the history (%d)", after)
	}
}

func TestEndOfRunHookSkipsAThreadWithoutASandbox(t *testing.T) {
	// Without a live sandbox or with checkpoints disabled the hook archives nothing.
	// Arrange
	none := newScripted(t, workspace.Options{}, always(""))
	none.mgr.sb = nil
	off := newScripted(t, workspace.Options{Disabled: true}, always(""))

	// Act
	a := workspace.EndOfRunHook{Svc: none.ws}.Finishing(none.ctx, &runtime.Finish{Run: &store.Run{ID: "r", ThreadID: none.th.ID}})
	b := workspace.EndOfRunHook{Svc: off.ws}.Finishing(off.ctx, &runtime.Finish{Run: &store.Run{ID: "r", ThreadID: off.th.ID}})
	none.ws.WaitArchives(none.ctx)

	// Assert
	if a != nil || b != nil || len(none.cmds) != 0 || len(off.cmds) != 0 {
		t.Fatalf("commands %d %d", len(none.cmds), len(off.cmds))
	}
}

func TestListParsesHistoryFromTheScript(t *testing.T) {
	// The log output becomes checkpoints (newest first): agent and automatic commits are told apart, invalid ids and a truncated tail are dropped.
	// Arrange
	rec := func(sha, date, subject string, files ...string) string {
		return sha + "\x00" + date + "\x00" + subject + "\x00" + strings.Join(files, "\x00") + "\x00\x00"
	}
	out := rec(sha40, "2026-01-02T00:00:00Z", "automatic: Before AI turn", "a.txt", "dir/b.txt") +
		rec("not-a-sha", "2026-01-01T00:00:00Z", "automatic: junk", "x") +
		rec(sha40b, "2026-01-01T00:00:00Z", "agent: Tidy up") +
		rec(strings.Repeat("c", 40), "2026-01-01T00:00:00Z", "hand commit", "c.txt") +
		strings.Repeat("d", 40) + "\x00" + "2026-01-01T00:00:00Z" // cut off mid-record
	s := newScripted(t, workspace.Options{}, always(out))

	// Act
	list, err := s.ws.List(s.ctx, s.th.ID, "")

	// Assert
	if err != nil || len(list) != 3 {
		t.Fatalf("list %+v %v", list, err)
	}
	first, second, third := list[0], list[1], list[2]
	if first.SHA != sha40 || first.ShortSHA != "aaaaaaa" || first.Kind != workspace.KindAutomatic || first.Label != "Before AI turn" ||
		len(first.ChangedFiles) != 2 || first.ChangedFiles[1] != "/workspace/dir/b.txt" || first.CreatedAt != "2026-01-02T00:00:00Z" {
		t.Fatalf("first %+v", first)
	}
	if second.Kind != workspace.KindAgent || second.Label != "Tidy up" || len(second.ChangedFiles) != 0 || second.ChangedFiles == nil {
		t.Fatalf("second %+v", second)
	}
	if third.Kind != workspace.KindAgent || third.Label != "hand commit" {
		t.Fatalf("third %+v", third)
	}
}

func TestListHandlesMissingRepositoriesAndHugeOutput(t *testing.T) {
	// No repository, no git, or no commit yet is an empty (non-nil) list; output above the cap is cut instead of failing.
	empties := map[string]string{"no repository": markNoRepo + "\n", "no git": markNoGit + "\n", "no commit": ""}
	for name, out := range empties {
		t.Run(name, func(t *testing.T) {
			// Arrange
			s := newScripted(t, workspace.Options{}, always(out))

			// Act
			list, err := s.ws.List(s.ctx, s.th.ID, "")

			// Assert
			if err != nil || list == nil || len(list) != 0 {
				t.Fatalf("list %#v %v", list, err)
			}
		})
	}
	t.Run("output above the cap", func(t *testing.T) {
		// Arrange
		first := sha40 + "\x00" + "2026-01-01T00:00:00Z" + "\x00" + "automatic: x" + "\x00" + "a.txt" + "\x00\x00"
		s := newScripted(t, workspace.Options{}, always(first+strings.Repeat("z", 5<<20)))

		// Act
		list, err := s.ws.List(s.ctx, s.th.ID, "a.txt")

		// Assert
		if err != nil || len(list) != 1 || list[0].SHA != sha40 {
			t.Fatalf("list %+v %v", list, err)
		}
	})
}

func TestListAndDiffNeedALiveSandbox(t *testing.T) {
	// Without sandboxes configured or for a thread without one the history is ErrNotReady; other manager failures are returned; invalid paths are refused first.
	boom := errors.New("provider down")
	// Arrange
	s := newScripted(t, workspace.Options{}, always(""))
	noSandbox := newScripted(t, workspace.Options{}, always(""))
	noSandbox.mgr.sb = nil
	disabled := newScripted(t, workspace.Options{}, always(""))
	disabled.mgr.disabled = true
	failing := newScripted(t, workspace.Options{}, always(""))
	failing.mgr.getErr = boom
	nilMgr := newScripted(t, workspace.Options{}, always(""))
	nilMgr.ws.Sandbox = nil

	// Act
	_, badPath := s.ws.List(s.ctx, s.th.ID, "../etc")
	_, none := noSandbox.ws.List(noSandbox.ctx, noSandbox.th.ID, "")
	_, off := disabled.ws.List(disabled.ctx, disabled.th.ID, "")
	_, other := failing.ws.List(failing.ctx, failing.th.ID, "")
	_, noMgr := nilMgr.ws.List(nilMgr.ctx, nilMgr.th.ID, "")
	_, diffNone := noSandbox.ws.FileDiff(noSandbox.ctx, noSandbox.th.ID, sha40, "a.txt")

	// Assert
	if !errors.Is(badPath, workspace.ErrPathOutside) || len(s.cmds) != 0 {
		t.Fatalf("bad path %v", badPath)
	}
	for name, err := range map[string]error{"no sandbox": none, "disabled": off, "no manager": noMgr, "diff": diffNone} {
		if !errors.Is(err, workspace.ErrNotReady) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if !errors.Is(other, boom) {
		t.Errorf("manager failure: %v", other)
	}
}

func TestFileDiffAnswersFromTheScript(t *testing.T) {
	// A diff is returned with its absolute path; unknown checkpoints and missing repositories are ErrUnknownCheckpoint; huge diffs are cut and flagged; failures and bad input are errors.
	t.Run("diff", func(t *testing.T) {
		// Arrange
		s := newScripted(t, workspace.Options{}, always("@@ -1 +1 @@\n-a\n+b\n"))

		// Act
		d, err := s.ws.FileDiff(s.ctx, s.th.ID, " "+sha40+" ", "./docs//a.txt")

		// Assert
		if err != nil || d.Path != "/workspace/docs/a.txt" || d.SHA != sha40 || !strings.Contains(d.Diff, "+b") || d.Truncated {
			t.Fatalf("diff %+v %v", d, err)
		}
	})
	for _, mark := range []string{markUnknownSHA, markNoRepo, markNoGit} {
		t.Run("unknown "+mark, func(t *testing.T) {
			// Arrange
			s := newScripted(t, workspace.Options{}, always(mark+"\n"))

			// Act
			_, err := s.ws.FileDiff(s.ctx, s.th.ID, sha40, "a.txt")

			// Assert
			if !errors.Is(err, workspace.ErrUnknownCheckpoint) {
				t.Fatalf("err %v", err)
			}
		})
	}
	t.Run("truncated", func(t *testing.T) {
		// Arrange
		s := newScripted(t, workspace.Options{}, always(strings.Repeat("+x\n", 600<<10)))

		// Act
		d, err := s.ws.FileDiff(s.ctx, s.th.ID, sha40, "a.txt")

		// Assert
		if err != nil || !d.Truncated || len(d.Diff) != 1<<20 {
			t.Fatalf("truncated %v len %d %v", d != nil && d.Truncated, len(d.Diff), err)
		}
	})
	t.Run("invalid input and failures", func(t *testing.T) {
		// Arrange
		s := newScripted(t, workspace.Options{}, func(sandbox.ExecRequest) (string, string, int, error) { return "", "fatal", 1, nil })

		// Act
		_, badPath := s.ws.FileDiff(s.ctx, s.th.ID, sha40, ".git/config")
		_, badSHA := s.ws.FileDiff(s.ctx, s.th.ID, "xyz", "a.txt")
		_, failed := s.ws.FileDiff(s.ctx, s.th.ID, sha40, "a.txt")

		// Assert
		var gitErr *workspace.GitError
		if !errors.Is(badPath, workspace.ErrPathGit) || !errors.Is(badSHA, workspace.ErrInvalidSHA) || !errors.As(failed, &gitErr) {
			t.Fatalf("errors %v %v %v", badPath, badSHA, failed)
		}
	})
}

func TestRestoreFileMapsEveryScriptOutcome(t *testing.T) {
	// The script's marker decides the result: refusals by reason, unknown checkpoints, nothing to do, a restore, a removal.
	cases := []struct {
		name     string
		out      string
		wantErr  error
		restored bool
		deleted  bool
		reason   string
	}{
		{"ignored", markRestoreBad + " ignored\n", workspace.ErrRestoreIgnored, false, false, ""},
		{"untracked", markRestoreBad + " untracked\n", workspace.ErrRestoreUntracked, false, false, ""},
		{"directory", markRestoreBad + " directory\n", workspace.ErrRestoreDirectory, false, false, ""},
		{"submodule", markRestoreBad + " submodule\n", workspace.ErrRestoreDirectory, false, false, ""},
		{"unknown sha", markUnknownSHA + "\n", workspace.ErrUnknownCheckpoint, false, false, ""},
		{"no repository", markNoRepo + "\n", workspace.ErrUnknownCheckpoint, false, false, ""},
		{"no git", markNoGit + "\n", workspace.ErrUnknownCheckpoint, false, false, ""},
		{"no changes", markRestoreNoop + "\n", nil, false, false, "no_changes"},
		{"restored", markRestoreOK + "\n", nil, true, false, ""},
		{"removed", markRestoreDel + "\n", nil, true, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			s := newScripted(t, workspace.Options{}, always(c.out))

			// Act
			res, err := s.ws.RestoreFile(s.ctx, s.th.ID, "alice", sha40, "docs/a.txt")

			// Assert
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) || res != nil {
					t.Fatalf("res %+v err %v", res, err)
				}
				return
			}
			if err != nil || res.Restored != c.restored || res.Deleted != c.deleted || res.Reason != c.reason || res.Path != "/workspace/docs/a.txt" || res.SHA != sha40 {
				t.Fatalf("res %+v err %v", res, err)
			}
		})
	}
}

func TestRestoreFileFailuresAreReportedWithoutRecording(t *testing.T) {
	// Invalid input, no sandbox, a lock that cannot be taken and a failing script are errors; nothing is archived or recorded.
	boom := errors.New("lock service down")
	// Arrange
	s := newScripted(t, workspace.Options{}, always(markRestoreOK))
	noSandbox := newScripted(t, workspace.Options{}, always(markRestoreOK))
	noSandbox.mgr.sb = nil
	locked := newScripted(t, workspace.Options{}, always(markRestoreOK))
	locked.ws.Locks = failLocker{err: boom}
	failing := newScripted(t, workspace.Options{}, func(sandbox.ExecRequest) (string, string, int, error) { return "", "fatal: index locked", 128, nil })

	// Act
	_, badPath := s.ws.RestoreFile(s.ctx, s.th.ID, "alice", sha40, "")
	_, badSHA := s.ws.RestoreFile(s.ctx, s.th.ID, "alice", "../../", "a.txt")
	_, notReady := noSandbox.ws.RestoreFile(noSandbox.ctx, noSandbox.th.ID, "alice", sha40, "a.txt")
	_, lockErr := locked.ws.RestoreFile(locked.ctx, locked.th.ID, "alice", sha40, "a.txt")
	_, scriptErr := failing.ws.RestoreFile(failing.ctx, failing.th.ID, "alice", sha40, "a.txt")

	// Assert
	var gitErr *workspace.GitError
	if !errors.Is(badPath, workspace.ErrPathRequired) || !errors.Is(badSHA, workspace.ErrInvalidSHA) || !errors.Is(notReady, workspace.ErrNotReady) ||
		!errors.Is(lockErr, boom) || !errors.As(scriptErr, &gitErr) {
		t.Fatalf("errors: %v %v %v %v %v", badPath, badSHA, notReady, lockErr, scriptErr)
	}
	if len(s.cmds) != 0 || locked.count("g checkout") != 0 {
		t.Fatal("a script ran for a refused request")
	}
}

func TestRestoreFileStillSucceedsWhenTheArtifactCannotBeRecorded(t *testing.T) {
	// The file is restored in the sandbox even if recording the new artifact version fails or no artifacts are configured; the result then carries no change.
	// Arrange
	broken := newScripted(t, workspace.Options{}, always(markRestoreOK))
	broken.ws.Artifacts = artifacts.New(&brokenArtifacts{Store: broken.st}, artifacts.PGBlobs{S: broken.st}, nil, artifacts.Options{})
	broken.sb.Put("/workspace/a.txt", "restored")
	bare := newScripted(t, workspace.Options{}, always(markRestoreOK))
	bare.ws.Artifacts = nil

	// Act
	a, errA := broken.ws.RestoreFile(broken.ctx, broken.th.ID, "alice", sha40, "a.txt")
	b, errB := bare.ws.RestoreFile(bare.ctx, bare.th.ID, "alice", sha40, "a.txt")

	// Assert
	if errA != nil || !a.Restored || a.Change != nil || errB != nil || !b.Restored || b.Change != nil {
		t.Fatalf("broken %+v %v bare %+v %v", a, errA, b, errB)
	}
}

// brokenArtifacts fails listing, so recording a path fails.
type brokenArtifacts struct{ *memstore.Store }

func (b *brokenArtifacts) ListArtifacts(context.Context, string) ([]*store.Artifact, error) {
	return nil, errors.New("db down")
}

func TestRestoreFileRecordsTheNewVersionAndArchivesTheHistory(t *testing.T) {
	// A restored file becomes a new artifact version in the thread, and the history is archived once the script is done.
	// Arrange
	s := newScripted(t, workspace.Options{}, func(req sandbox.ExecRequest) (string, string, int, error) {
		switch {
		case strings.Contains(req.Command, "g checkout"):
			return markRestoreOK, "", 0, nil
		case strings.Contains(req.Command, "rev-parse --verify -q HEAD ||"):
			return sha40, "", 0, nil
		case strings.Contains(req.Command, "tar -czf"):
			return "7", "", 0, nil
		}
		return "", "", 0, nil
	})
	s.sb.Put("/workspace/docs/a.txt", "restored content")
	s.sb.Put("/workspace/.work/git-archive.tar.gz", "tarball")

	// Act
	res, err := s.ws.RestoreFile(s.ctx, s.th.ID, "alice", sha40, "docs/a.txt")
	arch, archErr := s.st.WorkspaceGit(s.ctx, s.th.ID)

	// Assert
	if err != nil || res.Change == nil || res.Change.Change != store.ChangeCreated || res.Change.Artifact.Path != "/workspace/docs/a.txt" {
		t.Fatalf("result %+v %v", res, err)
	}
	if archErr != nil || arch.Head != sha40 {
		t.Fatalf("archive %+v %v", arch, archErr)
	}
}
