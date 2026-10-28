package workspace_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/GhaziBenDahmane/harness/agent"
	"github.com/GhaziBenDahmane/harness/artifacts"
	"github.com/GhaziBenDahmane/harness/sandbox"
	"github.com/GhaziBenDahmane/harness/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/store"
	"github.com/GhaziBenDahmane/harness/store/memstore"
	"github.com/GhaziBenDahmane/harness/workspace"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

type rig struct {
	t       *testing.T
	ctx     context.Context
	st      *memstore.Store
	rdb     *redis.Client
	backend *sandboxtest.Backend
	mgr     *sandbox.DefaultManager
	arts    *artifacts.Service
	ws      *workspace.Service
	th      *store.Thread
	clock   time.Time
}

// requireShell skips tests that run real git / tar scripts when the tools are missing.
func requireShell(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"bash", "git", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
}

func newRig(t *testing.T, o workspace.Options) *rig {
	t.Helper()
	requireShell(t)
	mr := miniredis.RunT(t)
	r := &rig{t: t, ctx: context.Background(), st: memstore.New(), backend: sandboxtest.NewBackend(), clock: time.Now()}
	r.st.Now = func() time.Time { return r.clock }
	r.rdb = redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = r.rdb.Close() })
	r.backend.OnCreate = func(sb *sandboxtest.Sandbox) { sb.SetExec(sandboxtest.FindExec(sb, sandboxtest.ShellExec(sb))) }
	r.arts = artifacts.New(r.st, artifacts.PGBlobs{S: r.st}, nil, artifacts.Options{BlobGCGrace: time.Hour})
	r.ws = workspace.New(nil, r.st, r.st, r.arts, o)
	r.newManager()
	th, err := r.st.CreateThread(r.ctx, "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	r.th = th
	return r
}

// newManager builds the sandbox manager with the OnCreate restore of cmd/work (history, then files).
func (r *rig) newManager() {
	r.mgr = sandbox.NewManager(r.backend, r.st, r.rdb, sandbox.ManagerOptions{MaxPerUser: 5, Timeout: time.Hour,
		OnCreate: func(ctx context.Context, threadID string, sb sandbox.Sandbox, recreated bool) string {
			r.ws.OnCreate(ctx, threadID, sb)
			return r.arts.RestoreWorkspace(ctx, threadID, sb, recreated)
		}})
	r.arts.Sandbox, r.ws.Sandbox = r.mgr, r.mgr
}

func (r *rig) sandbox() *sandboxtest.Sandbox {
	r.t.Helper()
	if _, err := r.mgr.Acquire(r.ctx, r.th.ID, "alice"); err != nil {
		r.t.Fatal(err)
	}
	created := r.backend.Created()
	return created[len(created)-1]
}

// expire simulates a sandbox that expired at the provider (fresh process: no cached handle).
func (r *rig) expire(sb *sandboxtest.Sandbox) {
	r.backend.Forget(sb.ID())
	r.newManager()
}

func (r *rig) checkpoint(sb sandbox.Sandbox) workspace.CheckpointResult {
	r.t.Helper()
	res, err := r.ws.Checkpoint(r.ctx, r.th.ID, sb, workspace.LabelBeforeTurn)
	if err != nil {
		r.t.Fatal(err)
	}
	return res
}

func (r *rig) list(rel string) []workspace.Checkpoint {
	r.t.Helper()
	list, err := r.ws.List(r.ctx, r.th.ID, rel)
	if err != nil {
		r.t.Fatal(err)
	}
	return list
}

func (r *rig) snapshot(sb sandbox.Sandbox) {
	r.t.Helper()
	if _, err := r.arts.Snapshot(r.ctx, sb, artifacts.RunRef{ThreadID: r.th.ID, Login: "alice"}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) artifactAt(p string) *store.Artifact {
	r.t.Helper()
	list, _ := r.st.ListArtifacts(r.ctx, r.th.ID)
	for _, a := range list {
		if a.Path == p {
			return a
		}
	}
	return nil
}

func labels(list []workspace.Checkpoint) []string {
	out := make([]string, 0, len(list))
	for _, c := range list {
		out = append(out, c.Kind+": "+c.Label)
	}
	return out
}

func TestCheckpointInitializesTheRepositoryAndCommitsOnlyChanges(t *testing.T) {
	// The first checkpoint creates the repository ("Initial workspace checkpoint"); without changes nothing is committed; a change is committed as "automatic: Before AI turn".
	// Arrange
	r := newRig(t, workspace.Options{})
	sb := r.sandbox()
	sb.Put("/workspace/a.txt", "1")

	// Act
	first := r.checkpoint(sb)
	unchanged := r.checkpoint(sb)
	sb.Put("/workspace/a.txt", "2")
	changed := r.checkpoint(sb)
	list := r.list("")

	// Assert
	if !first.Created || len(first.SHA) != 40 || unchanged.Created || unchanged.Reason != "no_changes" || !changed.Created {
		t.Fatalf("first %+v unchanged %+v changed %+v", first, unchanged, changed)
	}
	if got := labels(list); len(got) != 2 || got[0] != "automatic: Before AI turn" || got[1] != "automatic: Initial workspace checkpoint" {
		t.Fatalf("labels %v", got)
	}
	if list[0].SHA != changed.SHA || list[0].ShortSHA != changed.SHA[:7] || list[0].CreatedAt == "" ||
		len(list[0].ChangedFiles) != 1 || list[0].ChangedFiles[0] != "/workspace/a.txt" {
		t.Fatalf("newest %+v", list[0])
	}
	if email, _ := sb.File("/workspace/.git/config"); !strings.Contains(email, "harness@local") {
		t.Fatalf("local identity missing: %s", email)
	}
}

func TestCheckpointKeepsScratchAndDependencyDirectoriesOut(t *testing.T) {
	// /workspace/.work, node_modules, __pycache__ and .venv are excluded (.git/info/exclude), like the artifact snapshots.
	// Arrange
	r := newRig(t, workspace.Options{})
	sb := r.sandbox()
	sb.Put("/workspace/keep.txt", "k")
	sb.Put("/workspace/.work/scratch.txt", "s")
	sb.Put("/workspace/app/node_modules/x/index.js", "x")
	sb.Put("/workspace/src/__pycache__/m.pyc", "c")
	sb.Put("/workspace/.venv/bin/python", "p")

	// Act
	r.checkpoint(sb)
	list := r.list("")
	exclude, _ := sb.File("/workspace/.git/info/exclude")

	// Assert
	if len(list) != 1 || strings.Join(list[0].ChangedFiles, ",") != "/workspace/keep.txt" {
		t.Fatalf("checkpoint %+v", list)
	}
	for _, p := range []string{"/.work/", "node_modules/", "__pycache__/", ".venv/", ".codex/log/"} {
		if !strings.Contains(exclude, p+"\n") {
			t.Fatalf("exclude misses %q: %s", p, exclude)
		}
	}
}

func TestCheckpointFiltersTheHistoryByPath(t *testing.T) {
	// ?path= lists only the checkpoints that changed that file, with only that file.
	// Arrange
	r := newRig(t, workspace.Options{})
	sb := r.sandbox()
	sb.Put("/workspace/a.txt", "a")
	r.checkpoint(sb)
	sb.Put("/workspace/b.txt", "b")
	r.checkpoint(sb)
	sb.Put("/workspace/a.txt", "a2")
	sb.Put("/workspace/b.txt", "b2")
	r.checkpoint(sb)

	// Act
	onlyB := r.list("b.txt")
	absolute := r.list("/workspace/b.txt")

	// Assert
	if len(onlyB) != 2 || len(absolute) != 2 || strings.Join(onlyB[0].ChangedFiles, ",") != "/workspace/b.txt" {
		t.Fatalf("b history %+v / %+v", onlyB, absolute)
	}
}

func TestBeforeTurnCheckpointsOncePerRunAndSandbox(t *testing.T) {
	// Concurrent first acquisitions of a run share one checkpoint; later acquisitions of the same run do not checkpoint again; the next run does.
	// Arrange
	r := newRig(t, workspace.Options{})
	sb := r.sandbox()
	sb.Put("/workspace/a.txt", "1")

	// Act
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() { defer wg.Done(); r.ws.BeforeTurn(r.ctx, r.th.ID, "run-1", sb) }()
	}
	wg.Wait()
	sb.Put("/workspace/a.txt", "2") // the agent's change during run-1
	r.ws.BeforeTurn(r.ctx, r.th.ID, "run-1", sb)
	afterRun1 := len(r.list(""))
	r.ws.BeforeTurn(r.ctx, r.th.ID, "run-2", sb)

	// Assert
	if afterRun1 != 1 || len(r.list("")) != 2 {
		t.Fatalf("after run-1 %d, after run-2 %d", afterRun1, len(r.list("")))
	}
}

func TestBeforeRunNeverCreatesASandbox(t *testing.T) {
	// A run starting without a live sandbox does not create one (the first tool acquisition checkpoints it), and a live one is checkpointed.
	// Arrange
	r := newRig(t, workspace.Options{})
	run := &store.Run{ID: "run-1", ThreadID: r.th.ID, UserLogin: "alice"}

	// Act
	r.ws.BeforeRun(r.ctx, run)
	none := len(r.backend.Created())
	sb := r.sandbox()
	sb.Put("/workspace/a.txt", "1")
	r.ws.BeforeRun(r.ctx, &store.Run{ID: "run-2", ThreadID: r.th.ID, UserLogin: "alice"})

	// Assert
	if none != 0 || len(r.list("")) != 1 {
		t.Fatalf("created %d before, checkpoints %d", none, len(r.list("")))
	}
}

func TestCheckpointFailuresNeverFailTheRun(t *testing.T) {
	// Without git in the image the checkpoint reports no_git; a failing script is an error BeforeTurn only logs.
	// Arrange
	r := newRig(t, workspace.Options{})
	sb := r.sandbox()
	sb.SetExec(func(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
		req.OnStdout("__WC_NO_GIT__\n")
		return sandbox.ExecResult{}, nil
	})
	noGit := r.checkpoint(sb)
	sb.SetExec(func(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
		req.OnStderr("fatal: Unable to create '.git/index.lock'\n")
		return sandbox.ExecResult{ExitCode: 128}, nil
	})

	// Act
	_, err := r.ws.Checkpoint(r.ctx, r.th.ID, sb, workspace.LabelBeforeTurn)
	r.ws.BeforeTurn(r.ctx, r.th.ID, "run-1", sb) // must not panic or block

	// Assert
	var gitErr *workspace.GitError
	if noGit.Reason != "no_git" || !errors.As(err, &gitErr) || !strings.Contains(gitErr.Detail, "index.lock") {
		t.Fatalf("no git %+v err %v", noGit, err)
	}
}

func TestNormalizePathAcceptsOnlyFilesUnderTheWorkspace(t *testing.T) {
	// Paths are relative to /workspace or absolute under it; "..", .git, control characters and empty paths are refused.
	cases := []struct {
		in, want string
		err      error
	}{
		{"a.txt", "a.txt", nil},
		{"/workspace/dir/a.txt", "dir/a.txt", nil},
		{"./dir//a.txt", "dir/a.txt", nil},
		{`dir\a.txt`, "dir/a.txt", nil},
		{"-rf", "-rf", nil}, // passed after "--" and quoted: never an option
		{"", "", workspace.ErrPathRequired},
		{"/workspace", "", workspace.ErrPathRequired},
		{"/etc/passwd", "", workspace.ErrPathOutside},
		{"/workspacex/a", "", workspace.ErrPathOutside},
		{"../a", "", workspace.ErrPathOutside},
		{"dir/../../a", "", workspace.ErrPathOutside},
		{".git/config", "", workspace.ErrPathGit},
		{"sub/.git/HEAD", "", workspace.ErrPathGit},
		{"a\nb", "", workspace.ErrPathControl},
		{"a\x00b", "", workspace.ErrPathControl},
	}
	for _, c := range cases {
		got, err := workspace.NormalizePath(c.in)
		if got != c.want || !errors.Is(err, c.err) {
			t.Errorf("NormalizePath(%q) = %q, %v; want %q, %v", c.in, got, err, c.want, c.err)
		}
	}
}

func TestValidSHAAcceptsOnlyHexIDs(t *testing.T) {
	// A checkpoint id is 7 to 64 hexadecimal digits (no option or revision syntax reaches git).
	for in, ok := range map[string]bool{
		"abc1234": true, strings.Repeat("a", 40): true, strings.Repeat("F", 64): true,
		"abc123": false, strings.Repeat("a", 65): false, "--help": false, "HEAD~1": false, "abc1234^{tree}": false, "abc 1234": false,
	} {
		if _, err := workspace.ValidSHA(in); (err == nil) != ok {
			t.Errorf("ValidSHA(%q) err %v, want ok=%v", in, err, ok)
		}
	}
}

func TestRestoreFileRestoresTheCheckpointAndRecordsANewVersion(t *testing.T) {
	// Restoring a file commits pending changes as "Pre-restore", writes the checkpoint's content, commits it and records a new artifact version.
	// Arrange
	r := newRig(t, workspace.Options{})
	sb := r.sandbox()
	sb.Put("/workspace/a.txt", "v1\n")
	r.snapshot(sb)
	v1 := r.checkpoint(sb)
	sb.Put("/workspace/a.txt", "v2\n")
	r.snapshot(sb)

	// Act
	diff, diffErr := r.ws.FileDiff(r.ctx, r.th.ID, v1.SHA, "a.txt")
	res, err := r.ws.RestoreFile(r.ctx, r.th.ID, "alice", v1.SHA, "/workspace/a.txt")

	// Assert
	if diffErr != nil || !strings.Contains(diff.Diff, "-v1") || !strings.Contains(diff.Diff, "+v2") || diff.Path != "/workspace/a.txt" {
		t.Fatalf("diff %+v %v", diff, diffErr)
	}
	content, _ := sb.File("/workspace/a.txt")
	a := r.artifactAt("/workspace/a.txt")
	if err != nil || !res.Restored || res.Deleted || content != "v1\n" || res.Change == nil || res.Change.Change != store.ChangeUpdated || a.Version != 3 {
		t.Fatalf("restore %+v %v content %q artifact %+v", res, err, content, a)
	}
	data, _, _ := r.arts.Content(r.ctx, a, 0)
	if string(data) != "v1\n" {
		t.Fatalf("latest version %q", data)
	}
	if got := labels(r.list("a.txt")); len(got) != 3 || got[0] != "automatic: Restore a.txt from "+v1.SHA[:7] || got[1] != "automatic: Pre-restore a.txt" {
		t.Fatalf("history %v", got)
	}
}

func TestRestoreFileRemovesAFileThatDidNotExistAtTheCheckpoint(t *testing.T) {
	// A file absent from the checkpoint is removed from /workspace and its artifact marked deleted.
	// Arrange
	r := newRig(t, workspace.Options{})
	sb := r.sandbox()
	sb.Put("/workspace/keep.txt", "k")
	before := r.checkpoint(sb)
	sb.Put("/workspace/new.txt", "n")
	r.snapshot(sb)

	// Act
	res, err := r.ws.RestoreFile(r.ctx, r.th.ID, "alice", before.SHA, "new.txt")

	// Assert
	_, exists := sb.File("/workspace/new.txt")
	a := r.artifactAt("/workspace/new.txt")
	if err != nil || !res.Restored || !res.Deleted || exists || a == nil || !a.Deleted || res.Change == nil || res.Change.Change != store.ChangeDeleted {
		t.Fatalf("restore %+v %v exists %v artifact %+v", res, err, exists, a)
	}
	if keep, _ := sb.File("/workspace/keep.txt"); keep != "k" {
		t.Fatalf("other file touched: %q", keep)
	}
}

func TestRestoreFileWithoutChangesAndUnknownCheckpoints(t *testing.T) {
	// Restoring the current content changes nothing (no_changes); an unknown checkpoint is ErrUnknownCheckpoint; no sandbox is ErrNotReady.
	// Arrange
	r := newRig(t, workspace.Options{})
	sb := r.sandbox()
	sb.Put("/workspace/a.txt", "same")
	cp := r.checkpoint(sb)

	// Act
	same, sameErr := r.ws.RestoreFile(r.ctx, r.th.ID, "alice", cp.SHA, "a.txt")
	_, unknownErr := r.ws.RestoreFile(r.ctx, r.th.ID, "alice", strings.Repeat("ab", 20), "a.txt")
	_, diffErr := r.ws.FileDiff(r.ctx, r.th.ID, "abcdef1", "a.txt")
	r.expire(sb)
	_, notReady := r.ws.List(r.ctx, r.th.ID, "")

	// Assert
	if sameErr != nil || same.Restored || same.Reason != "no_changes" {
		t.Fatalf("same %+v %v", same, sameErr)
	}
	if !errors.Is(unknownErr, workspace.ErrUnknownCheckpoint) || !errors.Is(diffErr, workspace.ErrUnknownCheckpoint) || !errors.Is(notReady, workspace.ErrNotReady) {
		t.Fatalf("unknown %v diff %v not ready %v", unknownErr, diffErr, notReady)
	}
}

func TestHistorySurvivesSandboxRecreation(t *testing.T) {
	// After a run the .git directory is archived into the blob store; a new sandbox of the thread gets it back with the files.
	// Arrange
	r := newRig(t, workspace.Options{})
	sb := r.sandbox()
	sb.Put("/workspace/a.txt", "1")
	r.snapshot(sb)
	r.checkpoint(sb)
	sb.Put("/workspace/a.txt", "2")
	r.snapshot(sb)
	second := r.checkpoint(sb)
	r.ws.Archive(r.ctx, r.th.ID, sb)
	arch, archErr := r.st.WorkspaceGit(r.ctx, r.th.ID)
	r.expire(sb)

	// Act
	fresh := r.sandbox()
	list := r.list("")
	content, _ := fresh.File("/workspace/a.txt")

	// Assert
	if archErr != nil || arch.Head != second.SHA || !r.st.Blob(arch.BlobKey) || !strings.HasPrefix(arch.BlobKey, "workspace-git/"+r.th.ID+"/") {
		t.Fatalf("archive %+v %v", arch, archErr)
	}
	if fresh.ID() == sb.ID() || len(list) != 2 || list[0].SHA != second.SHA || content != "2" {
		t.Fatalf("fresh %s list %v content %q", fresh.ID(), labels(list), content)
	}
	if _, left := fresh.File("/workspace/.work/git-restore.tar.gz"); left {
		t.Fatal("restore archive left in the workspace")
	}
	if next := r.checkpoint(fresh); next.Created {
		t.Fatalf("restored workspace seen as changed: %+v", next)
	}
}

func TestArchiveOnlyWhenHeadMovesAndReplacedArchivesAreCollected(t *testing.T) {
	// An unchanged HEAD keeps the archive; a new one replaces it and queues the old blob for garbage collection, collected after the grace period.
	// Arrange
	r := newRig(t, workspace.Options{})
	sb := r.sandbox()
	sb.Put("/workspace/a.txt", "1")
	r.checkpoint(sb)
	r.ws.Archive(r.ctx, r.th.ID, sb)
	first, _ := r.st.WorkspaceGit(r.ctx, r.th.ID)

	// Act
	r.ws.Archive(r.ctx, r.th.ID, sb)
	same, _ := r.st.WorkspaceGit(r.ctx, r.th.ID)
	sb.Put("/workspace/a.txt", "2")
	r.checkpoint(sb)
	r.ws.Archive(r.ctx, r.th.ID, sb)
	second, _ := r.st.WorkspaceGit(r.ctx, r.th.ID)
	r.clock = r.clock.Add(2 * time.Hour)
	_, blobs, purgeErr := r.arts.Purge(r.ctx, 24*time.Hour)

	// Assert
	if same.BlobKey != first.BlobKey || second.BlobKey == first.BlobKey {
		t.Fatalf("first %s same %s second %s", first.BlobKey, same.BlobKey, second.BlobKey)
	}
	if purgeErr != nil || blobs != 1 || r.st.Blob(first.BlobKey) || !r.st.Blob(second.BlobKey) {
		t.Fatalf("purge %d %v old kept %v current kept %v", blobs, purgeErr, r.st.Blob(first.BlobKey), r.st.Blob(second.BlobKey))
	}
}

func TestArchiveOfAPurgedThreadIsCollected(t *testing.T) {
	// Purging a deleted thread queues its archive blob, deleted after the grace period.
	// Arrange
	r := newRig(t, workspace.Options{})
	sb := r.sandbox()
	sb.Put("/workspace/a.txt", "1")
	r.checkpoint(sb)
	r.ws.Archive(r.ctx, r.th.ID, sb)
	arch, _ := r.st.WorkspaceGit(r.ctx, r.th.ID)
	if _, err := r.st.DeleteThread(r.ctx, "alice", r.th.ID); err != nil {
		t.Fatal(err)
	}
	r.st.SetThreadDeletedAt(r.th.ID, r.clock.Add(-48*time.Hour))

	// Act
	threads, _, err := r.arts.Purge(r.ctx, 24*time.Hour)
	kept := r.st.Blob(arch.BlobKey)
	r.clock = r.clock.Add(2 * time.Hour)
	_, blobs, _ := r.arts.Purge(r.ctx, 24*time.Hour)

	// Assert
	if err != nil || threads != 1 || !kept || blobs < 1 || r.st.Blob(arch.BlobKey) {
		t.Fatalf("threads %d %v kept during grace %v blobs %d still there %v", threads, err, kept, blobs, r.st.Blob(arch.BlobKey))
	}
	if _, err := r.st.WorkspaceGit(r.ctx, r.th.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("archive record kept: %v", err)
	}
}

func TestArchiveAboveTheCapIsSkipped(t *testing.T) {
	// A history larger than ArchiveMaxBytes is not archived.
	// Arrange
	r := newRig(t, workspace.Options{ArchiveMaxBytes: 64})
	sb := r.sandbox()
	sb.Put("/workspace/a.txt", strings.Repeat("x", 1000))
	r.checkpoint(sb)

	// Act
	r.ws.Archive(r.ctx, r.th.ID, sb)
	_, err := r.st.WorkspaceGit(r.ctx, r.th.ID)

	// Assert
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("archived above the cap: %v", err)
	}
	if _, left := sb.File("/workspace/.work/git-archive.tar.gz"); left {
		t.Fatal("archive file left in the workspace")
	}
}

func TestBrokenArchiveStartsANewHistory(t *testing.T) {
	// An unreadable archive leaves the new sandbox without .git; the next checkpoint starts a new history.
	// Arrange
	r := newRig(t, workspace.Options{})
	_ = r.st.PutBlob(r.ctx, "workspace-git/broken.tar.gz", []byte("not a tarball"))
	_ = r.st.PutWorkspaceGit(r.ctx, store.WorkspaceGitArchive{ThreadID: r.th.ID, BlobKey: "workspace-git/broken.tar.gz", Head: strings.Repeat("a", 40)})

	// Act
	sb := r.sandbox()
	sb.Put("/workspace/a.txt", "1")
	res := r.checkpoint(sb)

	// Assert
	if !res.Created || len(r.list("")) != 1 {
		t.Fatalf("checkpoint %+v history %v", res, labels(r.list("")))
	}
}

func TestDisabledCheckpointsDoNothing(t *testing.T) {
	// HARNESS_WORKSPACE_CHECKPOINTS=false: no repository, no prompt section.
	// Arrange
	r := newRig(t, workspace.Options{Disabled: true})
	sb := r.sandbox()
	sb.Put("/workspace/a.txt", "1")

	// Act
	r.ws.BeforeTurn(r.ctx, r.th.ID, "run-1", sb)
	section := r.ws.PromptSection(r.ctx, agent.Input{Sandbox: true})

	// Assert
	if _, ok := sb.File("/workspace/.git/HEAD"); ok || section != "" {
		t.Fatalf("repository created or prompt %q", section)
	}
}

func TestPromptSectionIsWorksLocalGitInstructions(t *testing.T) {
	// Runs with sandbox tools get Work's local Git section (local commits only, never push or remotes).
	// Arrange
	r := newRig(t, workspace.Options{})

	// Act
	with := r.ws.PromptSection(r.ctx, agent.Input{Sandbox: true})
	without := r.ws.PromptSection(r.ctx, agent.Input{})

	// Assert
	if !strings.HasPrefix(with, workspace.GitPromptHeading) || !strings.Contains(with, "Do not push, fetch, pull, create remotes") || without != "" {
		t.Fatalf("with %q without %q", with, without)
	}
}

func TestStatusAndExplicitRestore(t *testing.T) {
	// Status: missing without saved state, ready with a live sandbox, stopped once it expired; restore recreates it with the files and the history; an active run blocks it.
	// Arrange
	r := newRig(t, workspace.Options{})
	missing, _ := r.ws.Status(r.ctx, r.th)
	sb := r.sandbox()
	sb.Put("/workspace/a.txt", "1")
	r.snapshot(sb)
	r.checkpoint(sb)
	r.ws.Archive(r.ctx, r.th.ID, sb)
	ready, _ := r.ws.Status(r.ctx, r.th)
	already, _ := r.ws.Restore(r.ctx, r.th)
	r.expire(sb)
	stopped, _ := r.ws.Status(r.ctx, r.th)
	busy := *r.th
	busy.ActiveRun = &store.ActiveRun{ID: "r", Status: store.StatusRunning}
	blocked, _ := r.ws.Status(r.ctx, &busy)
	_, blockedErr := r.ws.Restore(r.ctx, &busy)

	// Act
	restored, err := r.ws.Restore(r.ctx, r.th)

	// Assert
	if missing.Availability != "missing" || missing.CanRestore || missing.RootPath != "/workspace" {
		t.Fatalf("missing %+v", missing)
	}
	if !ready.Ready || ready.Status != "ready" || !ready.CanRestore || !ready.HasHistory || already.Restored || already.RestoreDiagnostic.Reason != "already_ready" {
		t.Fatalf("ready %+v already %+v", ready, already)
	}
	if stopped.Availability != "stopped" || stopped.Ready || !stopped.CanRestore {
		t.Fatalf("stopped %+v", stopped)
	}
	if blocked.CanRestore || blocked.RestoreBlockedReason == nil || !errors.Is(blockedErr, workspace.ErrRestoreBlocked) {
		t.Fatalf("blocked %+v %v", blocked, blockedErr)
	}
	created := r.backend.Created()
	fresh := created[len(created)-1]
	content, _ := fresh.File("/workspace/a.txt")
	if err != nil || !restored.Restored || !restored.Ready || content != "1" || len(r.list("")) != 1 {
		t.Fatalf("restored %+v %v content %q", restored, err, content)
	}
}

func TestRestoreFileRefusesDirectoriesIgnoredAndUnknownPathsWithoutTouchingThem(t *testing.T) {
	// A directory, an ignored file absent from the checkpoint and a path known nowhere are refused before anything changes (no deletion, no commit).
	// Arrange
	r := newRig(t, workspace.Options{})
	sb := r.sandbox()
	sb.Put("/workspace/docs/a.txt", "a")
	sb.Put("/workspace/.gitignore", "secret.txt\n")
	cp := r.checkpoint(sb)
	sb.Put("/workspace/secret.txt", "keep me")
	before := len(r.list(""))

	// Act
	_, dirErr := r.ws.RestoreFile(r.ctx, r.th.ID, "alice", cp.SHA, "docs")
	_, ignoredErr := r.ws.RestoreFile(r.ctx, r.th.ID, "alice", cp.SHA, "secret.txt")
	_, scratchErr := r.ws.RestoreFile(r.ctx, r.th.ID, "alice", cp.SHA, "node_modules/x.js")
	_, unknownErr := r.ws.RestoreFile(r.ctx, r.th.ID, "alice", cp.SHA, "nowhere.txt")

	// Assert
	if !errors.Is(dirErr, workspace.ErrRestoreDirectory) || !errors.Is(ignoredErr, workspace.ErrRestoreIgnored) ||
		!errors.Is(scratchErr, workspace.ErrRestoreIgnored) && !errors.Is(scratchErr, workspace.ErrRestoreUntracked) || !errors.Is(unknownErr, workspace.ErrRestoreUntracked) {
		t.Fatalf("dir %v ignored %v scratch %v unknown %v", dirErr, ignoredErr, scratchErr, unknownErr)
	}
	if secret, ok := sb.File("/workspace/secret.txt"); !ok || secret != "keep me" {
		t.Fatalf("ignored file touched: %q %v", secret, ok)
	}
	if a, ok := sb.File("/workspace/docs/a.txt"); !ok || a != "a" {
		t.Fatalf("directory touched: %q %v", a, ok)
	}
	if after := len(r.list("")); after != before {
		t.Fatalf("checkpoints %d → %d", before, after)
	}
}

func TestRestoreFileRemovesATrackedFileAddedAfterTheCheckpoint(t *testing.T) {
	// A file committed after the checkpoint (tracked in HEAD) is removed and the removal committed.
	// Arrange
	r := newRig(t, workspace.Options{})
	sb := r.sandbox()
	sb.Put("/workspace/keep.txt", "k")
	before := r.checkpoint(sb)
	sb.Put("/workspace/later.txt", "l")
	r.checkpoint(sb)

	// Act
	res, err := r.ws.RestoreFile(r.ctx, r.th.ID, "alice", before.SHA, "later.txt")

	// Assert
	_, exists := sb.File("/workspace/later.txt")
	if err != nil || !res.Deleted || exists {
		t.Fatalf("restore %+v %v exists %v", res, err, exists)
	}
	if got := labels(r.list("later.txt")); len(got) != 2 || got[0] != "automatic: Restore later.txt from "+before.SHA[:7] {
		t.Fatalf("history %v", got)
	}
}

func TestCheckpointsLeaveFilesAboveTheSizeLimitOut(t *testing.T) {
	// Files larger than MaxFileBytes are not committed (a tracked file that grew past it stops being tracked); smaller ones are, and the limit follows the files.
	// Arrange
	r := newRig(t, workspace.Options{MaxFileBytes: 100})
	sb := r.sandbox()
	sb.Put("/workspace/small.txt", "s")
	sb.Put("/workspace/grows.txt", "g")
	sb.Put("/workspace/uploads/big [1].bin", strings.Repeat("x", 1000))
	first := r.checkpoint(sb)
	sb.Put("/workspace/grows.txt", strings.Repeat("g", 1000))

	// Act
	second := r.checkpoint(sb)
	sb.Put("/workspace/uploads/big [1].bin", "now small")
	third := r.checkpoint(sb)

	// Assert
	list := r.list("")
	if !first.Created || !second.Created || !third.Created || len(list) != 3 {
		t.Fatalf("checkpoints %+v %+v %+v list %v", first, second, third, labels(list))
	}
	has := func(c workspace.Checkpoint, p string) bool {
		for _, f := range c.ChangedFiles {
			if f == p {
				return true
			}
		}
		return false
	}
	initial, grown, shrunk := list[2], list[1], list[0]
	if !has(initial, "/workspace/small.txt") || has(initial, "/workspace/uploads/big [1].bin") {
		t.Fatalf("initial checkpoint files %v", initial.ChangedFiles)
	}
	if !has(grown, "/workspace/grows.txt") { // recorded as no longer tracked
		t.Fatalf("grown checkpoint files %v", grown.ChangedFiles)
	}
	if !has(shrunk, "/workspace/uploads/big [1].bin") {
		t.Fatalf("a file back under the limit is checkpointed: %v", shrunk.ChangedFiles)
	}
	if g, _ := sb.File("/workspace/grows.txt"); len(g) != 1000 {
		t.Fatal("the large file was touched in the workspace")
	}
}

func TestStatusReportsAnUnsavedHistoryUntilTheNextArchive(t *testing.T) {
	// A history over the archive cap is not archived and the status says so (historyNotSaved too_large); a later successful archive clears it.
	// Arrange
	r := newRig(t, workspace.Options{ArchiveMaxBytes: 64})
	sb := r.sandbox()
	sb.Put("/workspace/a.txt", strings.Repeat("x", 1000))
	r.checkpoint(sb)

	// Act
	r.ws.Archive(r.ctx, r.th.ID, sb)
	tooLarge, err := r.ws.Status(r.ctx, r.th)
	r.ws.Opts.ArchiveMaxBytes = workspace.DefaultArchiveMaxBytes
	r.ws.Archive(r.ctx, r.th.ID, sb)
	saved, err2 := r.ws.Status(r.ctx, r.th)

	// Assert
	if err != nil || tooLarge.HistoryNotSaved == nil || tooLarge.HistoryNotSaved.Reason != store.GitIssueTooLarge || tooLarge.HistoryNotSaved.SizeBytes <= 64 {
		t.Fatalf("status %+v %v", tooLarge, err)
	}
	if err2 != nil || saved.HistoryNotSaved != nil || !saved.HasHistory {
		t.Fatalf("after a successful archive %+v %v", saved, err2)
	}
}
