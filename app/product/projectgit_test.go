package product

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/core/blobs"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/store"
)

// memBlobs is an in-memory blob store that, like the real ones (Postgres, S3), keeps the first value written under a key.
type memBlobs struct{ m map[string][]byte }

func newMemBlobs() *memBlobs { return &memBlobs{m: map[string][]byte{}} }

func (b *memBlobs) Put(_ context.Context, k string, d []byte) error {
	if _, ok := b.m[k]; !ok {
		b.m[k] = append([]byte(nil), d...)
	}
	return nil
}
func (b *memBlobs) Get(_ context.Context, k string) ([]byte, error) {
	d, ok := b.m[k]
	if !ok {
		return nil, blobs.ErrNotFound
	}
	return d, nil
}
func (b *memBlobs) Has(_ context.Context, k string) (bool, error) { _, ok := b.m[k]; return ok, nil }
func (b *memBlobs) Delete(_ context.Context, k string) error      { delete(b.m, k); return nil }
func (b *memBlobs) MaxBytes() int64                               { return 0 }

// hostSandbox runs commands with bash on the host, with /workspace mapped to a temporary directory. It is enough to
// exercise the git scripts for real.
type hostSandbox struct {
	sandbox.Sandbox
	root string
}

func newHostSandbox(t *testing.T) *hostSandbox { return &hostSandbox{root: t.TempDir()} }

func (h *hostSandbox) host(p string) string {
	return filepath.Join(h.root, strings.TrimPrefix(p, sandbox.Workspace))
}
func (h *hostSandbox) ID() string { return "host" }
func (h *hostSandbox) Exec(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	cmd := exec.CommandContext(ctx, "bash", "-c", strings.ReplaceAll(req.Command, sandbox.Workspace, h.root))
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, k+"="+strings.ReplaceAll(v, sandbox.Workspace, h.root))
	}
	out, err := cmd.Output()
	if req.OnStdout != nil {
		req.OnStdout(string(out))
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return sandbox.ExecResult{ExitCode: ee.ExitCode()}, nil
	}
	return sandbox.ExecResult{}, err
}
func (h *hostSandbox) ReadFile(_ context.Context, p string, max int64) ([]byte, int64, error) {
	d, err := os.ReadFile(h.host(p))
	return d, int64(len(d)), err
}
func (h *hostSandbox) WriteFile(_ context.Context, p string, d []byte) error {
	return os.WriteFile(h.host(p), d, 0o644)
}
func (h *hostSandbox) MkdirAll(_ context.Context, p string) error {
	return os.MkdirAll(h.host(p), 0o755)
}

func localRig(t *testing.T) (*RepoCheckout, *store.Project, *memBlobs) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ctx := context.Background()
	tm := teamstore.NewMem()
	cnUsers(t, tm, "alice")
	p, err := tm.CreateProject(ctx, store.Project{Name: "site"}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	b := newMemBlobs()
	return &RepoCheckout{Team: tm, Blobs: b}, p, b
}

func readHost(t *testing.T, h *hostSandbox, rel string) string {
	t.Helper()
	d, err := os.ReadFile(h.host(sandbox.Workspace + "/repo/" + rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(d)
}

func TestEveryTemplateHasAValidManifestAndFiles(t *testing.T) {
	list := Templates()
	if len(list) < 6 {
		t.Fatalf("got %d templates", len(list))
	}
	for _, tpl := range list {
		files, err := TemplateFiles(tpl.ID)
		if err != nil || len(files) == 0 {
			t.Fatalf("%s: %v (%d files)", tpl.ID, err, len(files))
		}
		var manifest []byte
		for _, f := range files {
			if f.Path == ManifestPath {
				manifest = f.Data
			}
		}
		m, err := ParseManifest(manifest)
		if err != nil || m.Template != tpl.ID {
			t.Errorf("%s: manifest %+v, %v", tpl.ID, m, err)
		}
	}
}

func TestParseManifestRejectsAnIncompleteOne(t *testing.T) {
	for name, in := range map[string]string{
		"no run":   "port: 80",
		"no port":  "run: ./x",
		"bad base": "run: ./x\nport: 80\nbase: both",
		"not yaml": ":\n  - [",
	} {
		if _, err := ParseManifest([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if m, err := ParseManifest([]byte("run: ./x\nport: 80")); err != nil || m.Base != "strip" {
		t.Errorf("default base: %+v %v", m, err)
	}
}

func TestANewProjectStartsFromItsTemplateAndIsSavedToTheBlobStore(t *testing.T) {
	// Arrange
	ctx := context.Background()
	rc, p, b := localRig(t)
	if err := SeedProjectTemplate(ctx, b, p.ID, "static-html"); err != nil {
		t.Fatal(err)
	}
	sb := newHostSandbox(t)

	// Act
	note := rc.setupLocal(ctx, "thread-1", sb, p)
	conflict, err := rc.PublishLocal(ctx, "thread-1", sb, p)

	// Assert
	if err != nil || conflict {
		t.Fatalf("publish: conflict=%v err=%v", conflict, err)
	}
	if !strings.Contains(note, "new project repository") || !strings.Contains(readHost(t, sb, "index.html"), "<h1>Hello</h1>") {
		t.Fatalf("note %q", note)
	}
	if _, _, _, found, _ := loadBundle(ctx, b, p.ID); !found {
		t.Fatal("no bundle stored")
	}
	if _, ok := b.m[localTemplateKey(p.ID)]; ok {
		t.Error("the template marker should be gone once the repository exists")
	}
}

func TestAnotherSandboxRestoresTheSavedProjectAndOnlyTheDeclaredFilesTravel(t *testing.T) {
	// Arrange: the first thread edits a file and installs dependencies (which must not be saved).
	ctx := context.Background()
	rc, p, b := localRig(t)
	_ = SeedProjectTemplate(ctx, b, p.ID, "static-html")
	first := newHostSandbox(t)
	rc.setupLocal(ctx, "thread-1", first, p)
	if err := first.WriteFile(ctx, sandbox.Workspace+"/repo/index.html", []byte("<h1>Edited</h1>")); err != nil {
		t.Fatal(err)
	}
	_ = first.MkdirAll(ctx, sandbox.Workspace+"/repo/node_modules/x")
	_ = first.WriteFile(ctx, sandbox.Workspace+"/repo/node_modules/x/big.js", []byte("junk"))
	if _, err := rc.PublishLocal(ctx, "thread-1", first, p); err != nil {
		t.Fatal(err)
	}

	// Act: a brand-new sandbox for another thread.
	second := newHostSandbox(t)
	note := rc.setupLocal(ctx, "thread-2", second, p)

	// Assert
	if !strings.Contains(note, "restored") || readHost(t, second, "index.html") != "<h1>Edited</h1>" {
		t.Fatalf("note %q, index %q", note, readHost(t, second, "index.html"))
	}
	if _, err := os.Stat(second.host(sandbox.Workspace + "/repo/node_modules")); err == nil {
		t.Error("node_modules was saved")
	}
}

func TestParallelThreadsMergeAndAConflictKeepsTheLoserBranch(t *testing.T) {
	// Arrange: two sandboxes start from the same saved state.
	ctx := context.Background()
	rc, p, b := localRig(t)
	_ = SeedProjectTemplate(ctx, b, p.ID, "static-html")
	a, c := newHostSandbox(t), newHostSandbox(t)
	rc.setupLocal(ctx, "thread-a", a, p)
	if _, err := rc.PublishLocal(ctx, "thread-a", a, p); err != nil {
		t.Fatal(err)
	}
	rc.setupLocal(ctx, "thread-b", c, p)
	write := func(sb *hostSandbox, file, text string) {
		if err := sb.WriteFile(ctx, sandbox.Workspace+"/repo/"+file, []byte(text)); err != nil {
			t.Fatal(err)
		}
	}

	// Act 1: different files merge cleanly.
	write(a, "a.txt", "from a")
	write(c, "c.txt", "from c")
	if conflict, err := rc.PublishLocal(ctx, "thread-a", a, p); err != nil || conflict {
		t.Fatalf("a: %v %v", conflict, err)
	}
	if conflict, err := rc.PublishLocal(ctx, "thread-b", c, p); err != nil || conflict {
		t.Fatalf("b: %v %v", conflict, err)
	}
	fresh := newHostSandbox(t)
	rc.setupLocal(ctx, "thread-c", fresh, p)
	if readHost(t, fresh, "a.txt") != "from a" || readHost(t, fresh, "c.txt") != "from c" {
		t.Fatal("both threads' files should be in the project")
	}

	// Act 2: both edit the same line of the same file.
	write(a, "index.html", "A version")
	write(c, "index.html", "C version")
	if conflict, err := rc.PublishLocal(ctx, "thread-a", a, p); err != nil || conflict {
		t.Fatalf("a: %v %v", conflict, err)
	}
	conflict, err := rc.PublishLocal(ctx, "thread-b", c, p)

	// Assert: the second is reported, main keeps the first, nothing is lost.
	if err != nil || !conflict {
		t.Fatalf("conflict=%v err=%v", conflict, err)
	}
	last := newHostSandbox(t)
	rc.setupLocal(ctx, "thread-d", last, p)
	if readHost(t, last, "index.html") != "A version" {
		t.Errorf("main = %q", readHost(t, last, "index.html"))
	}
	note := rc.setupLocal(ctx, "thread-e", newHostSandbox(t), p)
	if !strings.Contains(note, "conflict/"+branchFor("thread-b")) {
		t.Errorf("the conflicting branch was not kept: %q", note)
	}
}

func TestProjectFilesAreDeletedWithTheProject(t *testing.T) {
	ctx := context.Background()
	b := newMemBlobs()
	_ = b.Put(ctx, localBundleKey("p1", 0), []byte("x"))
	_ = b.Put(ctx, localBundleKey("p1", 1), []byte("y"))
	_ = SeedProjectTemplate(ctx, b, "p1", "astro")
	DeleteProjectFiles(ctx, b, "p1")
	if len(b.m) != 0 {
		t.Fatalf("left %v", b.m)
	}
	if err := SeedProjectTemplate(ctx, b, "p1", "nope"); err == nil {
		t.Error("an unknown template was accepted")
	}
}

func TestTheBundleAlternatesSlotsAndALostSaveKeepsTheLastCompleteCopy(t *testing.T) {
	ctx := context.Background()
	b := newMemBlobs()
	if err := saveBundle(ctx, b, "p", []byte("one"), 0, 0, false); err != nil {
		t.Fatal(err)
	}
	data, gen, slot, found, err := loadBundle(ctx, b, "p")
	if err != nil || !found || string(data) != "one" || gen != 1 || slot != 0 {
		t.Fatalf("first: %q gen=%d slot=%d found=%v err=%v", data, gen, slot, found, err)
	}
	if err := saveBundle(ctx, b, "p", []byte("two"), gen, slot, true); err != nil {
		t.Fatal(err)
	}
	data, gen, slot, _, _ = loadBundle(ctx, b, "p")
	if string(data) != "two" || gen != 2 || slot != 1 {
		t.Fatalf("second: %q gen=%d slot=%d", data, gen, slot)
	}
	// A third save replaces the older slot (0), never the newest one.
	if err := saveBundle(ctx, b, "p", []byte("three"), gen, slot, true); err != nil {
		t.Fatal(err)
	}
	data, gen, slot, _, _ = loadBundle(ctx, b, "p")
	if string(data) != "three" || gen != 3 || slot != 0 {
		t.Fatalf("third: %q gen=%d slot=%d", data, gen, slot)
	}
	// A save that died after deleting the old slot leaves the newest intact.
	_ = b.Delete(ctx, localBundleKey("p", 1))
	if data, _, _, found, _ := loadBundle(ctx, b, "p"); !found || string(data) != "three" {
		t.Fatalf("after a lost save: %q found=%v", data, found)
	}
	// A damaged slot is ignored.
	b.m[localBundleKey("p", 1)] = []byte("garbage")
	if data, _, _, _, _ := loadBundle(ctx, b, "p"); string(data) != "three" {
		t.Fatalf("with a damaged slot: %q", data)
	}
}
