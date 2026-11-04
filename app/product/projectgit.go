package product

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/7ar-chat/harness/core/blobs"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/runtime"
)

// A project without a repository URL keeps its files in a git repository of its own. Its canonical copy is a git bundle
// in the blob store (S3 in production), not the sandbox: every thread works in its sandbox on a branch of the
// repository restored from the bundle, and when a run ends the thread's branch is merged into main and the bundle is
// written back. Sandboxes can then come and go, and several threads can work on one project.
//
// Writes to the bundle are serialised by a lock (the coordinator's when there is one, else the process's), because a
// bundle is replaced as a whole.

// The blob stores keep the first value written under a key (their keys are content hashes), so a bundle that changes at
// every run cannot be overwritten in place. It lives in one of two slots instead: a save deletes the slot that holds the
// older generation and writes the new one there, so the last complete copy is never touched and a crash between the two
// steps loses nothing but the save that was in progress (the session's branch still has the work).
func localBundleKey(projectID string, slot int) string {
	return fmt.Sprintf("projects/%s/repo.%d.bundle", projectID, slot)
}

var bundleMagic = []byte("HRB1")

// loadBundle returns the newest complete bundle of a project and where it is stored; found is false for a project that
// has never been saved.
func loadBundle(ctx context.Context, b blobs.Blobs, projectID string) (data []byte, gen uint64, slot int, found bool, err error) {
	for s := 0; s < 2; s++ {
		raw, gerr := b.Get(ctx, localBundleKey(projectID, s))
		if errors.Is(gerr, blobs.ErrNotFound) {
			continue
		}
		if gerr != nil {
			return nil, 0, 0, false, gerr
		}
		if len(raw) < len(bundleMagic)+8 || !bytes.HasPrefix(raw, bundleMagic) {
			slog.Warn("project files: ignoring a damaged bundle slot", "project", projectID, "slot", s)
			continue
		}
		g := binary.BigEndian.Uint64(raw[len(bundleMagic):])
		if !found || g > gen {
			data, gen, slot, found = raw[len(bundleMagic)+8:], g, s, true
		}
	}
	return data, gen, slot, found, nil
}

// saveBundle stores a new generation in the slot that does not hold the newest one.
func saveBundle(ctx context.Context, b blobs.Blobs, projectID string, bundle []byte, prevGen uint64, prevSlot int, hadPrev bool) error {
	slot, gen := 0, uint64(1)
	if hadPrev {
		slot, gen = 1-prevSlot, prevGen+1
	}
	buf := make([]byte, 0, len(bundleMagic)+8+len(bundle))
	buf = append(buf, bundleMagic...)
	buf = binary.BigEndian.AppendUint64(buf, gen)
	buf = append(buf, bundle...)
	key := localBundleKey(projectID, slot)
	if err := b.Delete(ctx, key); err != nil {
		return err
	}
	return b.Put(ctx, key, buf)
}

// localTemplateKey holds the template a project was created from until its first run seeds the repository.
func localTemplateKey(projectID string) string { return "projects/" + projectID + "/template" }

// SeedProjectTemplate records the template a new repo-less project starts from; its first sandbox is filled with it.
func SeedProjectTemplate(ctx context.Context, b blobs.Blobs, projectID, templateID string) error {
	if _, ok := TemplateByID(templateID); !ok {
		return fmt.Errorf("unknown template %q", templateID)
	}
	return b.Put(ctx, localTemplateKey(projectID), []byte(templateID))
}

// DeleteProjectFiles removes what the blob store holds for a project.
func DeleteProjectFiles(ctx context.Context, b blobs.Blobs, projectID string) {
	for _, k := range []string{localBundleKey(projectID, 0), localBundleKey(projectID, 1), localTemplateKey(projectID)} {
		if err := b.Delete(ctx, k); err != nil {
			slog.Warn("project files: delete failed", "project", projectID, "key", k, "error", err)
		}
	}
}

const (
	bundleIn  = sandbox.ScratchDir + "/project-in.bundle"
	bundleOut = sandbox.ScratchDir + "/project-out.bundle"
	// maxBundle bounds the bundle read back from a sandbox when the blob store sets no cap of its own.
	maxBundle = 512 << 20
)

const localSetupScript = `set -e
mkdir -p /workspace/.work
if [ ! -d "$REPO_DIR/.git" ]; then
  if [ -f "$BUNDLE" ]; then
    git clone -q -b main -- "$BUNDLE" "$REPO_DIR"
    # Work that could not be merged earlier is kept on conflict/* branches.
    git -C "$REPO_DIR" for-each-ref --format='%(refname:lstrip=3)' refs/remotes/origin/conflict | while read -r b; do
      git -C "$REPO_DIR" branch "$b" "origin/$b"
    done
    git -C "$REPO_DIR" remote remove origin
  else
    mkdir -p "$REPO_DIR"
    git init -q -b main "$REPO_DIR"
  fi
fi
rm -f "$BUNDLE"
cd "$REPO_DIR"
git config user.name "${GIT_AUTHOR_NAME:-harness agent}"
git config user.email "${GIT_AUTHOR_EMAIL:-agent@harness.local}"
for p in node_modules/ .venv/ venv/ __pycache__/ '*.pyc' .DS_Store; do
  grep -qxF "$p" .git/info/exclude 2>/dev/null || printf '%s\n' "$p" >> .git/info/exclude
done
if ! git rev-parse -q --verify HEAD >/dev/null 2>&1; then
  git add -A
  git commit -q --allow-empty -m "Initial commit"
fi
git checkout -q -B "$THREAD_BRANCH"
echo "CONFLICTS=$(git for-each-ref --format='%(refname:short)' refs/heads/conflict | tr '\n' ' ')"
`

const localPublishScript = `set -e
cd "$REPO_DIR"
git config user.name "${GIT_AUTHOR_NAME:-harness agent}"
git config user.email "${GIT_AUTHOR_EMAIL:-agent@harness.local}"
git add -A
if ! git diff --cached --quiet; then
  git commit -q -m "Changes from $THREAD_BRANCH"
fi
rm -f "$OUT"
if [ -f "$LATEST" ]; then
  git fetch -q "$LATEST" refs/heads/main:refs/remotes/pub/main
  if ! git merge -q --no-edit refs/remotes/pub/main >/dev/null 2>&1; then
    git merge --abort >/dev/null 2>&1 || true
    git branch -f main refs/remotes/pub/main
    git branch -f "conflict/$THREAD_BRANCH" "$THREAD_BRANCH"
    git bundle create "$OUT" main $(git for-each-ref --format='%(refname:short)' refs/heads/conflict) >/dev/null 2>&1
    echo RESULT=conflict
    exit 0
  fi
fi
git branch -f main HEAD
git bundle create "$OUT" main $(git for-each-ref --format='%(refname:short)' refs/heads/conflict) >/dev/null 2>&1
echo RESULT=ok
`

// projectLocks serialises the writers of a project's bundle inside one process.
var projectLocks sync.Map // project id -> *sync.Mutex

func (rc *RepoCheckout) lockProject(ctx context.Context, projectID string) (unlock func(), err error) {
	mu, _ := projectLocks.LoadOrStore(projectID, &sync.Mutex{})
	m := mu.(*sync.Mutex)
	m.Lock()
	if rc.Coordinator == nil {
		return m.Unlock, nil
	}
	key, token := "project-repo:"+projectID, uuid.NewString()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		ok, err := rc.Coordinator.SetNX(ctx, key, token, 3*time.Minute)
		if err == nil && ok {
			return func() {
				_ = rc.Coordinator.Unlock(context.WithoutCancel(ctx), key, token)
				m.Unlock()
			}, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			m.Unlock()
			if err == nil {
				err = errors.New("another run is saving this project")
			}
			return nil, err
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// setupLocal prepares /workspace/repo for a project without a repository URL.
func (rc *RepoCheckout) setupLocal(ctx context.Context, threadID string, sb sandbox.Sandbox, p *store.Project) string {
	branch := branchFor(threadID)
	env := map[string]string{"REPO_DIR": RepoDir, "THREAD_BRANCH": branch, "BUNDLE": bundleIn}
	if err := sb.MkdirAll(ctx, sandbox.ScratchDir); err != nil {
		return "The project files could not be restored: " + err.Error()
	}
	restored := false
	if rc.Blobs != nil {
		data, _, _, found, err := loadBundle(ctx, rc.Blobs, p.ID)
		if err != nil {
			slog.Warn("project files: reading the bundle failed", "project", p.ID, "error", err)
			return "The project files could not be restored from storage; do not create new project files until this is resolved."
		}
		if found {
			if err := sb.WriteFile(ctx, bundleIn, data); err != nil {
				return "The project files could not be restored: " + err.Error()
			}
			restored = true
		} else {
			rc.seedTemplate(ctx, sb, p)
		}
	}
	var out strings.Builder
	res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: localSetupScript, Env: env, Timeout: 5 * time.Minute,
		OnStdout: func(t string) { out.WriteString(t) }})
	if err != nil || res.ExitCode != 0 {
		slog.Warn("project files: setup failed", "thread", threadID, "exit", res.ExitCode, "error", err)
		return "Setting up the project in " + RepoDir + " failed."
	}
	note := fmt.Sprintf("A new project repository was created in %s on branch %s.", RepoDir, branch)
	if restored {
		note = fmt.Sprintf("The project files were restored in %s on branch %s.", RepoDir, branch)
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if names := strings.TrimSpace(strings.TrimPrefix(line, "CONFLICTS=")); strings.HasPrefix(line, "CONFLICTS=") && names != "" {
			note += " Work from earlier conversations that could not be merged into main is kept on the branches " + names +
				"; merge what is still wanted and delete the branch (git branch -D) when done."
		}
	}
	return note
}

// seedTemplate writes the files of the project's template into a repository that does not exist yet.
func (rc *RepoCheckout) seedTemplate(ctx context.Context, sb sandbox.Sandbox, p *store.Project) {
	raw, err := rc.Blobs.Get(ctx, localTemplateKey(p.ID))
	if err != nil {
		return
	}
	files, err := TemplateFiles(strings.TrimSpace(string(raw)))
	if err != nil {
		slog.Warn("project files: template unavailable", "project", p.ID, "error", err)
		return
	}
	for _, f := range files {
		dst := path.Join(RepoDir, f.Path)
		if err := sb.MkdirAll(ctx, path.Dir(dst)); err != nil {
			slog.Warn("project files: template write failed", "project", p.ID, "file", f.Path, "error", err)
			return
		}
		if err := sb.WriteFile(ctx, dst, f.Data); err != nil {
			slog.Warn("project files: template write failed", "project", p.ID, "file", f.Path, "error", err)
			return
		}
	}
}

// PublishLocal merges the thread's work into the project's main branch and stores the bundle. It reports whether the
// merge conflicted (the thread's branch is then kept as conflict/<branch> in the bundle next to main, so nothing is lost).
func (rc *RepoCheckout) PublishLocal(ctx context.Context, threadID string, sb sandbox.Sandbox, p *store.Project) (conflict bool, err error) {
	if rc.Blobs == nil {
		return false, errors.New("no blob store")
	}
	unlock, err := rc.lockProject(ctx, p.ID)
	if err != nil {
		return false, err
	}
	defer unlock()

	env := map[string]string{"REPO_DIR": RepoDir, "THREAD_BRANCH": branchFor(threadID), "LATEST": bundleIn, "OUT": bundleOut}
	_ = sb.MkdirAll(ctx, sandbox.ScratchDir)
	latest, prevGen, prevSlot, hadPrev, gerr := loadBundle(ctx, rc.Blobs, p.ID)
	if gerr != nil {
		return false, gerr
	}
	if hadPrev {
		if err := sb.WriteFile(ctx, bundleIn, latest); err != nil {
			return false, err
		}
	} else {
		_, _ = sb.Exec(ctx, sandbox.ExecRequest{Command: "rm -f " + bundleIn, Timeout: 30 * time.Second})
	}
	var out strings.Builder
	res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: localPublishScript, Env: env, Timeout: 3 * time.Minute,
		OnStdout: func(t string) { out.WriteString(t) }})
	if err != nil {
		return false, err
	}
	if res.ExitCode != 0 {
		return false, fmt.Errorf("saving the project failed (exit %d)", res.ExitCode)
	}
	limit := int64(maxBundle)
	if m := rc.Blobs.MaxBytes(); m > 0 && m < limit {
		limit = m
	}
	data, size, err := sb.ReadFile(ctx, bundleOut, limit)
	if err != nil {
		return false, err
	}
	if size > limit {
		return false, fmt.Errorf("the project is larger than the storage limit (%d bytes)", limit)
	}
	if err := saveBundle(ctx, rc.Blobs, p.ID, data, prevGen, prevSlot, hadPrev); err != nil {
		return false, err
	}
	// The template only seeds the first repository.
	_ = rc.Blobs.Delete(ctx, localTemplateKey(p.ID))
	return strings.Contains(out.String(), "RESULT=conflict"), nil
}

// localPublish saves a repo-less project when one of its runs ends.
type localPublish struct {
	rc  *RepoCheckout
	mgr sandbox.Manager
}

func (h localPublish) Finishing(ctx context.Context, f *runtime.Finish) func(context.Context) {
	if f.Run.Kind == store.KindCompact || f.Run.Sandbox == store.SandboxOff {
		return nil
	}
	p := h.rc.project(ctx, f.Run.ThreadID)
	if p == nil || p.RepoURL != "" {
		return nil
	}
	threadID := f.Run.ThreadID
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		sb, err := h.mgr.Get(ctx, threadID)
		if err != nil {
			return // no sandbox: nothing was changed
		}
		conflict, err := h.rc.PublishLocal(ctx, threadID, sb, p)
		switch {
		case err != nil:
			slog.Warn("project files: saving failed", "project", p.ID, "thread", threadID, "error", err)
		case conflict:
			slog.Warn("project files: merge conflict, the thread's branch was kept", "project", p.ID, "thread", threadID, "branch", branchFor(threadID))
		}
	}()
	return nil
}

var _ runtime.Hook = localPublish{}

// localSection tells the agent how a project without a repository is kept.
func localSection(p *store.Project, threadID string) string {
	return fmt.Sprintf("# Project files\n\nThe files of the project %q are in %s, a git repository on your own branch %s. Work there and keep the "+
		"project's files in it: they are saved with the project when the run ends and are what the next conversation starts from. "+
		"Other conversations of the project work in their own copies and their changes are merged into the project's main branch. "+
		"%s in the repository says how to install and run the project; keep it correct when you change how the project starts. "+
		"Do not push or add remotes: there is none.", p.Name, RepoDir, branchFor(threadID), ManifestPath)
}
