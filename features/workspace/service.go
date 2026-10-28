package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/GhaziBenDahmane/harness/engine/syncx"
	"github.com/GhaziBenDahmane/harness/features/artifacts"
	"github.com/GhaziBenDahmane/harness/sandbox"
	"github.com/GhaziBenDahmane/harness/store"
)

// Options configure the service.
type Options struct {
	// Disabled turns checkpoints off (HARNESS_WORKSPACE_CHECKPOINTS=false): no
	// repository, no archive; the endpoints answer from an absent history.
	Disabled bool
	// ArchiveMaxBytes caps the archived .git (tar.gz; default 50 MiB, also
	// bounded by the blob backend's cap). A larger history is not archived
	// (the previous archive is kept).
	ArchiveMaxBytes int64
	// MaxFileBytes keeps larger files out of the checkpoints (≤ 0: no limit;
	// HARNESS_WORKSPACE_CHECKPOINT_MAX_FILE_BYTES).
	MaxFileBytes int64
	// Timeout bounds one git script (default 30 s, like Work).
	Timeout time.Duration
	// LockWait bounds the wait for a thread's Git lock (default 1 min).
	LockWait time.Duration
}

// DefaultArchiveMaxBytes is the default Options.ArchiveMaxBytes.
const DefaultArchiveMaxBytes = 50 << 20

// Locker is the cluster-wide lock of the store (store.ArtifactStore.AdvisoryLock).
type Locker interface {
	AdvisoryLock(ctx context.Context, name string, wait time.Duration) (func(), error)
}

// Service implements the workspace checkpoints, their persistence and the
// workspace status / restore.
type Service struct {
	Sandbox   sandbox.Manager
	Store     store.WorkspaceGitStore
	Locks     Locker
	Artifacts *artifacts.Service
	Opts      Options

	locks syncx.KeyedMutex
	mu    sync.Mutex
	done  map[string]*onceDone // "<runID>/<sandboxID>": the run's checkpoint in that sandbox

	archMu  sync.Mutex
	pending map[string]*archiveJob // thread → archive in progress (ArchiveAsync)
	archWG  sync.WaitGroup
}

// archiveJob is a thread's background archive; next is the sandbox of a
// request made while it ran (archived again once it is done).
type archiveJob struct{ next sandbox.Sandbox }

type onceDone struct {
	once sync.Once
}

// New returns a service with defaults applied.
func New(mgr sandbox.Manager, st store.WorkspaceGitStore, locks Locker, arts *artifacts.Service, o Options) *Service {
	if o.ArchiveMaxBytes <= 0 {
		o.ArchiveMaxBytes = DefaultArchiveMaxBytes
	}
	if o.Timeout <= 0 {
		o.Timeout = 30 * time.Second
	}
	if o.LockWait <= 0 {
		o.LockWait = time.Minute
	}
	return &Service{Sandbox: mgr, Store: st, Locks: locks, Artifacts: arts, Opts: o, done: map[string]*onceDone{}}
}

// Enabled reports whether checkpoints are on (a sandbox is configured).
func (s *Service) Enabled() bool {
	return s != nil && !s.Opts.Disabled && s.Sandbox != nil && s.Sandbox.Enabled()
}

// lock serializes the Git operations of a thread across processes.
func (s *Service) lock(ctx context.Context, threadID string) (func(), error) {
	name := "work:git:" + threadID
	start := time.Now()
	release, err := s.locks.LockWait(ctx, name, s.Opts.LockWait) // bounded by the wait and ctx (C-23)
	if err != nil {
		return nil, fmt.Errorf("git lock: %w", err)
	}
	if s.Locks == nil {
		return release, nil
	}
	unlock, err := s.Locks.AdvisoryLock(ctx, name, max(s.Opts.LockWait-time.Since(start), time.Millisecond))
	if err != nil {
		release()
		return nil, fmt.Errorf("git lock: %w", err)
	}
	return func() { unlock(); release() }, nil
}

// run executes a script in /workspace and returns its stdout (exit status ≠ 0 is a GitError).
func (s *Service) run(ctx context.Context, sb sandbox.Sandbox, script string, maxOut int) (string, error) {
	var out, errOut bytes.Buffer
	truncated := false
	res, err := sb.Exec(ctx, sandbox.ExecRequest{
		Command: script, Cwd: sandbox.Workspace, Timeout: s.Opts.Timeout,
		Env: map[string]string{"GIT_TERMINAL_PROMPT": "0", "LC_ALL": "C"},
		OnStdout: func(t string) {
			if maxOut > 0 && out.Len()+len(t) > maxOut {
				truncated = true
				t = t[:max(0, maxOut-out.Len())]
			}
			out.WriteString(t)
		},
		OnStderr: func(t string) {
			if errOut.Len() < 4096 {
				errOut.WriteString(t)
			}
		},
	})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		detail := strings.TrimSpace(errOut.String())
		if detail == "" {
			detail = "exit status " + strconv.Itoa(res.ExitCode)
		}
		return "", &GitError{Detail: detail}
	}
	if truncated {
		return out.String(), errTruncated
	}
	return out.String(), nil
}

var errTruncated = errors.New("output truncated")

// CheckpointResult is the outcome of a checkpoint.
type CheckpointResult struct {
	Created bool
	SHA     string
	// Reason is "no_changes", "no_git" (git missing in the image) or "disabled".
	Reason string
}

// Checkpoint commits every change of /workspace as "automatic: <label>"
// (initializing the repository on first use).
func (s *Service) Checkpoint(ctx context.Context, threadID string, sb sandbox.Sandbox, label string) (CheckpointResult, error) {
	if s.Opts.Disabled {
		return CheckpointResult{Reason: "disabled"}, nil
	}
	if hasControl(label) {
		return CheckpointResult{}, errors.New("label cannot contain ASCII control characters")
	}
	unlock, err := s.lock(ctx, threadID)
	if err != nil {
		return CheckpointResult{}, err
	}
	defer unlock()
	out, err := s.run(ctx, sb, checkpointScript(KindAutomatic+": "+label, s.Opts.MaxFileBytes), 4096)
	if err != nil {
		return CheckpointResult{}, err
	}
	switch l := lastLine(out); l {
	case markNoChanges:
		return CheckpointResult{Reason: "no_changes"}, nil
	case markNoGit:
		return CheckpointResult{Reason: "no_git"}, nil
	default:
		if !validParsedSHA(l) {
			return CheckpointResult{}, &GitError{Detail: "unexpected output: " + l}
		}
		return CheckpointResult{Created: true, SHA: l}, nil
	}
}

// BeforeTurn takes the run's "Before AI turn" checkpoint in sb, once per run
// and sandbox (a sandbox recreated mid-run gets its own); concurrent callers
// wait for it. It never fails the run: errors are logged.
func (s *Service) BeforeTurn(ctx context.Context, threadID, runID string, sb sandbox.Sandbox) {
	if !s.Enabled() || sb == nil || runID == "" {
		return
	}
	key := runID + "/" + sb.ID()
	s.mu.Lock()
	d := s.done[key]
	if d == nil {
		if len(s.done) > 10000 { // runs whose end hook never ran (crashes): bounded
			s.done = map[string]*onceDone{}
		}
		d = &onceDone{}
		s.done[key] = d
	}
	s.mu.Unlock()
	d.once.Do(func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.Opts.Timeout+10*time.Second)
		defer cancel()
		res, err := s.Checkpoint(cctx, threadID, sb, LabelBeforeTurn)
		switch {
		case err != nil:
			slog.Warn("workspace: checkpoint before the AI turn failed; continuing", "thread_id", threadID, "run_id", runID, "error", err)
		case res.Reason == "no_git":
			slog.Warn("workspace: git is not installed in the sandbox image; no checkpoints", "thread_id", threadID)
		case res.Created:
			slog.Debug("workspace: checkpoint saved", "thread_id", threadID, "run_id", runID, "sha", res.SHA)
		}
	})
}

// forgetRun drops the run's once-markers (end of run).
func (s *Service) forgetRun(runID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.done {
		if strings.HasPrefix(k, runID+"/") {
			delete(s.done, k)
		}
	}
}

// live returns the thread's running sandbox (ErrNotReady when there is none).
func (s *Service) live(ctx context.Context, threadID string) (sandbox.Sandbox, error) {
	if s.Sandbox == nil || !s.Sandbox.Enabled() {
		return nil, ErrNotReady
	}
	sb, err := s.Sandbox.Get(ctx, threadID)
	if errors.Is(err, sandbox.ErrNoSandbox) {
		return nil, ErrNotReady
	}
	return sb, err
}

// List returns the checkpoints touching the file p (all when p is ""), newest first.
func (s *Service) List(ctx context.Context, threadID, p string) ([]Checkpoint, error) {
	rel := ""
	if p != "" {
		var err error
		if rel, err = NormalizePath(p); err != nil {
			return nil, err
		}
	}
	sb, err := s.live(ctx, threadID)
	if err != nil {
		return nil, err
	}
	out, err := s.run(ctx, sb, listScript(rel), 4<<20)
	if err != nil && !errors.Is(err, errTruncated) {
		return nil, err
	}
	if l := lastLine(out); l == markNoRepo || l == markNoGit {
		return []Checkpoint{}, nil
	}
	list := parseLog(out)
	if list == nil {
		list = []Checkpoint{}
	}
	return list, nil
}

// validate normalizes a file path and a checkpoint id.
func validate(p, sha string) (string, string, error) {
	rel, err := NormalizePath(p)
	if err != nil {
		return "", "", err
	}
	sha, err = ValidSHA(sha)
	return rel, sha, err
}

// maxDiffBytes caps a returned diff.
const maxDiffBytes = 1 << 20

// Diff is a file's diff between a checkpoint and the working tree.
type Diff struct {
	Path      string `json:"path"`
	SHA       string `json:"sha"`
	Diff      string `json:"diff"`
	Truncated bool   `json:"truncated,omitempty"`
}

// FileDiff diffs the file p between the checkpoint sha and the working tree.
func (s *Service) FileDiff(ctx context.Context, threadID, sha, p string) (*Diff, error) {
	rel, sha, err := validate(p, sha)
	if err != nil {
		return nil, err
	}
	sb, err := s.live(ctx, threadID)
	if err != nil {
		return nil, err
	}
	out, err := s.run(ctx, sb, diffScript(sha, rel), maxDiffBytes)
	truncated := errors.Is(err, errTruncated)
	if err != nil && !truncated {
		return nil, err
	}
	switch lastLine(out) {
	case markUnknownSHA, markNoRepo, markNoGit:
		return nil, ErrUnknownCheckpoint
	}
	return &Diff{Path: AbsPath(rel), SHA: sha, Diff: out, Truncated: truncated}, nil
}

// RestoreResult is the outcome of a file restore.
type RestoreResult struct {
	Restored bool   `json:"restored"`
	Reason   string `json:"reason,omitempty"`
	Path     string `json:"path"`
	SHA      string `json:"sha"`
	// Deleted is set when the file did not exist at the checkpoint (removed).
	Deleted bool `json:"deleted,omitempty"`
	// Change is the artifact change recorded for the file (nil when none).
	Change *artifacts.Change `json:"-"`
}

// RestoreFile restores rel as it was at the checkpoint sha (Work's
// restore_file), records the new artifact version (or the artifact's
// deletion) and archives the history.
func (s *Service) RestoreFile(ctx context.Context, threadID, login, sha, p string) (*RestoreResult, error) {
	rel, sha, err := validate(p, sha)
	if err != nil {
		return nil, err
	}
	sb, err := s.live(ctx, threadID)
	if err != nil {
		return nil, err
	}
	unlock, err := s.lock(ctx, threadID)
	if err != nil {
		return nil, err
	}
	out, err := s.run(ctx, sb, restoreFileScript(sha, rel, s.Opts.MaxFileBytes), 4096)
	unlock()
	if err != nil {
		return nil, err
	}
	res := &RestoreResult{Path: AbsPath(rel), SHA: sha}
	last := lastLine(out)
	if reason, ok := strings.CutPrefix(last, markRestoreInvalid+" "); ok {
		switch reason {
		case "ignored":
			return nil, ErrRestoreIgnored
		case "untracked":
			return nil, ErrRestoreUntracked
		default:
			return nil, ErrRestoreDirectory
		}
	}
	switch last {
	case markUnknownSHA, markNoRepo, markNoGit:
		return nil, ErrUnknownCheckpoint
	case markRestoreNoop:
		res.Reason = "no_changes"
		return res, nil
	case markRestoreDeleted:
		res.Restored, res.Deleted = true, true
	default:
		res.Restored = true
	}
	if s.Artifacts != nil {
		change, err := s.Artifacts.RecordPath(ctx, sb, threadID, login, res.Path)
		if err != nil {
			slog.Warn("workspace: recording the restored file failed", "thread_id", threadID, "path", res.Path, "error", err)
		}
		res.Change = change
	}
	s.Archive(ctx, threadID, sb)
	return res, nil
}

// ArchiveAsync archives the thread's history in the background (end of run:
// the run's completion never waits for the upload), serialized per thread —
// a request made while one runs is coalesced into one more archive afterwards
// (the cluster-wide git lock also serializes it with other processes).
func (s *Service) ArchiveAsync(threadID string, sb sandbox.Sandbox) {
	if !s.Enabled() || s.Store == nil || s.Artifacts == nil || sb == nil {
		return
	}
	s.archMu.Lock()
	if s.pending == nil {
		s.pending = map[string]*archiveJob{}
	}
	if job := s.pending[threadID]; job != nil {
		job.next = sb
		s.archMu.Unlock()
		return
	}
	job := &archiveJob{}
	s.pending[threadID] = job
	s.archWG.Add(1)
	s.archMu.Unlock()
	go func() {
		defer s.archWG.Done()
		for {
			s.Archive(context.Background(), threadID, sb)
			s.archMu.Lock()
			if job.next == nil {
				delete(s.pending, threadID)
				s.archMu.Unlock()
				return
			}
			sb, job.next = job.next, nil
			s.archMu.Unlock()
		}
	}()
}

// WaitArchives waits for the background archives (shutdown, tests); false when ctx ended first.
func (s *Service) WaitArchives(ctx context.Context) bool {
	if s == nil {
		return true
	}
	done := make(chan struct{})
	go func() { s.archWG.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// archiveKey is a fresh blob key of the thread's archive (never reused: a
// replaced archive is garbage-collected after the grace period).
func archiveKey(threadID string) string {
	return "workspace-git/" + threadID + "/" + uuid.NewString() + ".tar.gz"
}

const (
	archivePath = ".work/git-archive.tar.gz"
	restorePath = sandbox.Workspace + "/.work/git-restore.tar.gz"
)

func (s *Service) archiveCap() int64 {
	limit := s.Opts.ArchiveMaxBytes
	if s.Artifacts != nil {
		if m := s.Artifacts.Blobs.MaxBytes(); m > 0 && m < limit {
			limit = m
		}
	}
	return limit
}

// Archive stores /workspace/.git of sb (tar.gz) as the thread's archive when
// HEAD moved since the last one. Best effort: errors are logged.
func (s *Service) Archive(ctx context.Context, threadID string, sb sandbox.Sandbox) {
	if !s.Enabled() || s.Store == nil || s.Artifacts == nil || sb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	if err := s.archive(ctx, threadID, sb); err != nil {
		slog.Warn("workspace: archiving the checkpoint history failed", "thread_id", threadID, "error", err)
		s.recordIssue(ctx, store.WorkspaceGitIssue{ThreadID: threadID, Reason: store.GitIssueFailed})
	}
}

// recordIssue remembers that the latest history is not archived (status: "history not saved").
func (s *Service) recordIssue(ctx context.Context, i store.WorkspaceGitIssue) {
	if err := s.Store.SetWorkspaceGitIssue(context.WithoutCancel(ctx), i); err != nil {
		slog.Warn("workspace: recording the unsaved history failed", "thread_id", i.ThreadID, "error", err)
	}
}

// clearIssue forgets a previous archive failure (the history is saved).
func (s *Service) clearIssue(ctx context.Context, threadID string) {
	if err := s.Store.ClearWorkspaceGitIssue(context.WithoutCancel(ctx), threadID); err != nil {
		slog.Warn("workspace: clearing the unsaved history flag failed", "thread_id", threadID, "error", err)
	}
}

func (s *Service) archive(ctx context.Context, threadID string, sb sandbox.Sandbox) error {
	unlock, err := s.lock(ctx, threadID)
	if err != nil {
		return err
	}
	defer unlock()
	out, err := s.run(ctx, sb, scriptHeader+`if [ ! -d .git ]; then printf '`+markNoRepo+`\n'; exit 0; fi
g rev-parse --verify -q HEAD || printf '`+markNoRepo+`\n'
`, 4096)
	if err != nil {
		return err
	}
	head := lastLine(out)
	if !validParsedSHA(head) {
		return nil // no repository or no commit yet
	}
	prev, err := s.Store.WorkspaceGit(ctx, threadID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if prev != nil && prev.Head == head {
		s.clearIssue(ctx, threadID)
		return nil
	}
	limit := s.archiveCap()
	out, err = s.run(ctx, sb, `set -eu
mkdir -p .work
rm -f `+archivePath+`
tar -czf `+archivePath+` .git
wc -c < `+archivePath+`
`, 4096)
	cleanup := func() {
		_, _ = sb.Exec(context.WithoutCancel(ctx), sandbox.ExecRequest{Command: "rm -f " + archivePath, Cwd: sandbox.Workspace, Timeout: 10 * time.Second})
	}
	defer cleanup()
	if err != nil {
		return err
	}
	size, err := strconv.ParseInt(lastLine(out), 10, 64)
	if err != nil {
		return fmt.Errorf("archive size: %w", err)
	}
	if size > limit {
		slog.Warn("workspace: the checkpoint history is too large to archive; keeping the previous archive",
			"thread_id", threadID, "size_bytes", size, "max_bytes", limit)
		s.recordIssue(ctx, store.WorkspaceGitIssue{ThreadID: threadID, Reason: store.GitIssueTooLarge, Head: head, SizeBytes: size})
		return nil
	}
	data, total, err := sb.ReadFile(ctx, sandbox.Workspace+"/"+archivePath, limit)
	if err != nil {
		return err
	}
	if total > int64(len(data)) {
		return fmt.Errorf("archive grew past the cap")
	}
	key := archiveKey(threadID)
	// Queued before it is stored (C-25): an archive whose row is never written
	// is collected after the GC grace; the current one is referenced by its row.
	if err := s.Artifacts.Store.QueueBlob(ctx, key); err != nil {
		return err
	}
	if err := s.Artifacts.Blobs.Put(ctx, key, data); err != nil {
		return err
	}
	if err := s.Store.PutWorkspaceGit(ctx, store.WorkspaceGitArchive{ThreadID: threadID, BlobKey: key, Head: head, SizeBytes: int64(len(data))}); err != nil {
		_ = s.Artifacts.Blobs.Delete(context.WithoutCancel(ctx), key)
		return err
	}
	s.clearIssue(ctx, threadID)
	slog.Debug("workspace: checkpoint history archived", "thread_id", threadID, "head", head, "size_bytes", len(data))
	return nil
}

// RestoreHistory writes the thread's archived .git back into a new sandbox
// (OnCreate, before the artifacts restore). Best effort: a missing or broken
// archive leaves /workspace without history (the next checkpoint starts one).
func (s *Service) RestoreHistory(ctx context.Context, threadID string, sb sandbox.Sandbox) {
	if !s.Enabled() || s.Store == nil || s.Artifacts == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	// The git lock first: the archive read is the one an end-of-run archive
	// cannot replace meanwhile (C-11).
	unlock, err := s.lock(ctx, threadID)
	if err != nil {
		slog.Warn("workspace: restoring the history without the git lock", "thread_id", threadID, "error", err)
	} else {
		defer unlock()
	}
	arch, err := s.Store.WorkspaceGit(ctx, threadID)
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	if err != nil {
		slog.Warn("workspace: reading the archived history failed", "thread_id", threadID, "error", err)
		return
	}
	data, err := s.Artifacts.Blobs.Get(ctx, arch.BlobKey)
	if err != nil {
		slog.Warn("workspace: the archived history is unavailable", "thread_id", threadID, "key", arch.BlobKey, "error", err)
		return
	}
	if err := sb.WriteFile(ctx, restorePath, data); err != nil {
		slog.Warn("workspace: writing the archived history failed", "thread_id", threadID, "error", err)
		return
	}
	out, err := s.run(ctx, sb, `set -eu
rel=.work/git-restore.tar.gz
if [ -e .git ]; then rm -f "$rel"; printf 'exists\n'; exit 0; fi
if tar -xzf "$rel" --no-same-owner .git && git rev-parse --verify -q HEAD >/dev/null 2>&1; then
  rm -f "$rel"
  printf 'restored\n'
else
  rm -rf .git "$rel"
  printf 'broken\n'
fi
`, 4096)
	switch {
	case err != nil:
		slog.Warn("workspace: restoring the archived history failed", "thread_id", threadID, "error", err)
	case lastLine(out) == "broken":
		slog.Warn("workspace: the archived history is unreadable; starting a new one", "thread_id", threadID)
	default:
		slog.Info("workspace: checkpoint history restored", "thread_id", threadID, "head", arch.Head)
	}
}
