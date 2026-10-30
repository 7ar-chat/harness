package sandboxtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/core/sandbox"
)

// ConformanceOptions tunes RunConformanceWith for an adapter.
type ConformanceOptions struct {
	// Skip lists subtests the adapter cannot pass, with the reason. A key is a
	// group ("Exec") or a subtest ("Exec/Cwd"). Skipping is for test doubles
	// and known gaps: a real adapter should pass everything.
	Skip map[string]string
	// Confined asserts that the file API refuses paths that leave the sandbox
	// through "..", or through a symbolic link (host-level adapters such as
	// sandbox/local, which have no container around the file API). A
	// containerised adapter does not need it: the container is the boundary.
	Confined bool
	// KillGrace is how long a killed sandbox may still answer Connect before
	// it must report sandbox.ErrNoSandbox (providers that kill
	// asynchronously). Default 20 s.
	KillGrace time.Duration
}

// RunConformance runs the conformance suite against a sandbox adapter: the
// contract of sandbox.Backend, sandbox.Instance and the file and exec
// behaviour the harness' tools rely on (see docs/sandbox-adapters.md). Each
// call of newBackend must return a Backend over the same provider (the suite
// may call it once per group); it is called with the test, so it can register
// cleanups or t.Skip when the provider is not configured.
//
//	func TestConformance(t *testing.T) {
//		sandboxtest.RunConformance(t, func(t *testing.T) sandbox.Backend { return myadapter.New(...) })
//	}
//
// The suite needs a shell in the sandbox for its Exec, Protocol and symlink
// subtests: bash, GNU coreutils (realpath, stat, head, base64, mv -T...) and
// python3, as the image requirements of docs/sandbox-adapters.md list.
func RunConformance(t *testing.T, newBackend func(t *testing.T) sandbox.Backend) {
	t.Helper()
	RunConformanceWith(t, newBackend, ConformanceOptions{})
}

// RunConformanceWith is RunConformance with options.
func RunConformanceWith(t *testing.T, newBackend func(t *testing.T) sandbox.Backend, o ConformanceOptions) {
	t.Helper()
	if o.KillGrace <= 0 {
		o.KillGrace = 20 * time.Second
	}
	c := &conformance{o: o, newBackend: newBackend}
	t.Run("Lifecycle", c.lifecycle)
	t.Run("Exec", c.execGroup)
	t.Run("Files", c.filesGroup)
	t.Run("Protocol", c.protocolGroup)
}

type conformance struct {
	o          ConformanceOptions
	newBackend func(t *testing.T) sandbox.Backend
}

// sub runs a subtest unless skipped.
func (c *conformance) sub(t *testing.T, group, name string, fn func(t *testing.T)) {
	t.Run(name, func(t *testing.T) {
		for _, k := range []string{group, group + "/" + name} {
			if why, ok := c.o.Skip[k]; ok {
				t.Skipf("skipped: %s", why)
			}
		}
		fn(t)
	})
}

func (c *conformance) groupSkipped(t *testing.T, group string) bool {
	if why, ok := c.o.Skip[group]; ok {
		t.Skipf("skipped: %s", why)
		return true
	}
	return false
}

// create makes a sandbox killed at the end of the test.
func (c *conformance) create(t *testing.T, b sandbox.Backend) sandbox.Instance {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	sb, err := b.Create(ctx, sandbox.CreateOptions{Metadata: map[string]string{"purpose": "conformance"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() {
		kctx, kcancel := context.WithTimeout(context.Background(), time.Minute)
		defer kcancel()
		_ = b.Kill(kctx, sb.ID())
	})
	return sb
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// ---- Lifecycle ----

func (c *conformance) lifecycle(t *testing.T) {
	if c.groupSkipped(t, "Lifecycle") {
		return
	}
	b := c.newBackend(t)
	c.sub(t, "Lifecycle", "CreateConnectKill", func(t *testing.T) {
		ctx := ctxT(t)
		sb, err := b.Create(ctx, sandbox.CreateOptions{})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		id := sb.ID()
		t.Cleanup(func() { _ = b.Kill(context.Background(), id) })
		if id == "" {
			t.Fatal("Create returned an empty ID")
		}
		if !sb.Healthy(ctx) {
			t.Error("a new sandbox is not Healthy")
		}
		if sb.Gone(ctx) {
			t.Error("a new sandbox is Gone")
		}
		if err := sb.Renew(ctx, 30*time.Minute); err != nil {
			t.Errorf("Renew: %v", err)
		}
		if err := sb.WriteFile(ctx, "/workspace/conformance/lifecycle.txt", []byte("kept")); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		again, err := b.Connect(ctx, id)
		if err != nil {
			t.Fatalf("Connect(%s): %v", id, err)
		}
		if again.ID() != id {
			t.Errorf("Connect returned sandbox %q, want %q", again.ID(), id)
		}
		data, _, err := again.ReadFile(ctx, "/workspace/conformance/lifecycle.txt", 100)
		if err != nil || string(data) != "kept" {
			t.Errorf("a connected sandbox lost its files: %q, %v", data, err)
		}
		if err := b.Kill(ctx, id); err != nil {
			t.Fatalf("Kill: %v", err)
		}
		// Kill may complete asynchronously; after the grace, Connect must
		// report the definitive end of the sandbox with ErrNoSandbox.
		deadline := time.Now().Add(c.o.KillGrace)
		for {
			_, err = b.Connect(ctx, id)
			if errors.Is(err, sandbox.ErrNoSandbox) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("Connect after Kill = %v, want sandbox.ErrNoSandbox", err)
			}
			time.Sleep(300 * time.Millisecond)
		}
		if err := b.Kill(ctx, id); err != nil {
			t.Logf("a second Kill returned %v (allowed, the manager ignores it)", err)
		}
	})
	c.sub(t, "Lifecycle", "ConnectUnknown", func(t *testing.T) {
		_, err := b.Connect(ctxT(t), "conformance-no-such-sandbox")
		if !errors.Is(err, sandbox.ErrNoSandbox) {
			t.Fatalf("Connect(unknown) = %v, want sandbox.ErrNoSandbox (only a definitive absence is ErrNoSandbox; anything else is read as transient)", err)
		}
	})
	c.sub(t, "Lifecycle", "SandboxesAreIsolated", func(t *testing.T) {
		ctx := ctxT(t)
		a, d := c.create(t, b), c.create(t, b)
		if a.ID() == d.ID() {
			t.Fatal("two Creates returned the same ID")
		}
		if err := a.WriteFile(ctx, "/workspace/conformance/only-in-a.txt", []byte("a")); err != nil {
			t.Fatal(err)
		}
		if _, _, err := d.ReadFile(ctx, "/workspace/conformance/only-in-a.txt", 10); !errors.Is(err, sandbox.ErrNotFound) {
			t.Fatalf("sandbox B sees sandbox A's file: %v (one sandbox per thread: they must not share /workspace)", err)
		}
	})
}

// ---- Exec ----

// collector gathers the streamed output of an Exec.
type collector struct {
	mu          sync.Mutex
	out, errOut strings.Builder
	firstOut    time.Time
}

func (k *collector) req(r sandbox.ExecRequest) sandbox.ExecRequest {
	r.OnStdout = func(s string) {
		k.mu.Lock()
		defer k.mu.Unlock()
		if k.firstOut.IsZero() {
			k.firstOut = time.Now()
		}
		k.out.WriteString(s)
	}
	r.OnStderr = func(s string) {
		k.mu.Lock()
		defer k.mu.Unlock()
		k.errOut.WriteString(s)
	}
	return r
}

func (k *collector) stdout() string { k.mu.Lock(); defer k.mu.Unlock(); return k.out.String() }
func (k *collector) stderr() string { k.mu.Lock(); defer k.mu.Unlock(); return k.errOut.String() }

// lines splits output into non-empty trimmed lines: transports that deliver
// message by message add newlines (docs/sandbox-adapters.md), so tests never
// depend on where newlines fall.
func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimRight(l, "\r"); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func sh(t *testing.T, sb sandbox.Sandbox, cmd string) (stdout string, code int) {
	t.Helper()
	var k collector
	res, err := sb.Exec(ctxT(t), k.req(sandbox.ExecRequest{Command: cmd, Cwd: "/workspace", Timeout: time.Minute}))
	if err != nil {
		t.Fatalf("Exec(%q): %v", cmd, err)
	}
	return k.stdout(), res.ExitCode
}

func mustSh(t *testing.T, sb sandbox.Sandbox, cmd string) string {
	t.Helper()
	out, code := sh(t, sb, cmd)
	if code != 0 {
		t.Fatalf("%q exited %d: %s", cmd, code, out)
	}
	return out
}

// dir is a fresh working directory for a subtest.
func dir(t *testing.T) string {
	name := strings.NewReplacer("/", "-", " ", "_").Replace(t.Name())
	return "/workspace/conformance/" + name
}

func (c *conformance) execGroup(t *testing.T) {
	if c.groupSkipped(t, "Exec") {
		return
	}
	sb := c.create(t, c.newBackend(t))
	c.sub(t, "Exec", "StdoutAndStderrAreSeparate", func(t *testing.T) {
		var k collector
		res, err := sb.Exec(ctxT(t), k.req(sandbox.ExecRequest{Command: "echo to-stdout; echo to-stderr >&2", Cwd: "/workspace"}))
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("exit %d, err %v", res.ExitCode, err)
		}
		if !strings.Contains(k.stdout(), "to-stdout") || strings.Contains(k.stdout(), "to-stderr") {
			t.Errorf("stdout = %q", k.stdout())
		}
		if !strings.Contains(k.stderr(), "to-stderr") || strings.Contains(k.stderr(), "to-stdout") {
			t.Errorf("stderr = %q", k.stderr())
		}
	})
	c.sub(t, "Exec", "OutputStreamsInOrderWhileRunning", func(t *testing.T) {
		var k collector
		start := time.Now()
		res, err := sb.Exec(ctxT(t), k.req(sandbox.ExecRequest{Command: "for i in 1 2 3; do echo line$i; sleep 0.5; done", Cwd: "/workspace", Timeout: time.Minute}))
		took := time.Since(start)
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("exit %d, err %v", res.ExitCode, err)
		}
		if got := strings.Join(lines(k.stdout()), ","); got != "line1,line2,line3" {
			t.Errorf("stdout lines = %s, want line1,line2,line3 in order", got)
		}
		// Live output: the first line must reach the callback long before the
		// command ends (tools show it to the user while a command runs).
		if lead := start.Add(took).Sub(k.firstOut); lead < 600*time.Millisecond {
			t.Errorf("the first line arrived %v before the end of a %v command: output is not streamed", lead, took)
		}
	})
	c.sub(t, "Exec", "ExitCodeIsAResultNotAnError", func(t *testing.T) {
		for _, code := range []int{0, 1, 7, 42} {
			var k collector
			res, err := sb.Exec(ctxT(t), k.req(sandbox.ExecRequest{Command: fmt.Sprintf("exit %d", code), Cwd: "/workspace"}))
			if err != nil || res.ExitCode != code {
				t.Errorf("exit %d: got code %d, err %v (a non-zero exit is not an error)", code, res.ExitCode, err)
			}
		}
		var k collector
		res, err := sb.Exec(ctxT(t), k.req(sandbox.ExecRequest{Command: "no-such-command-conformance", Cwd: "/workspace"}))
		if err != nil || res.ExitCode != 127 {
			t.Errorf("unknown command: code %d, err %v, want 127", res.ExitCode, err)
		}
	})
	c.sub(t, "Exec", "Cwd", func(t *testing.T) {
		d := dir(t)
		mustSh(t, sb, "mkdir -p "+d)
		for _, want := range []string{"/workspace", d} {
			var k collector
			res, err := sb.Exec(ctxT(t), k.req(sandbox.ExecRequest{Command: "pwd", Cwd: want}))
			if err != nil || res.ExitCode != 0 || strings.TrimSpace(k.stdout()) != want {
				t.Errorf("Cwd %s: pwd = %q (exit %d, err %v)", want, k.stdout(), res.ExitCode, err)
			}
		}
	})
	c.sub(t, "Exec", "Env", func(t *testing.T) {
		var k collector
		res, err := sb.Exec(ctxT(t), k.req(sandbox.ExecRequest{
			Command: `printf '%s|%s' "$CONF_PLAIN" "$CONF_ODD"`, Cwd: "/workspace",
			Env: map[string]string{"CONF_PLAIN": "hello world", "CONF_ODD": `a'b"c$d` + "`e`"},
		}))
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("exit %d, err %v", res.ExitCode, err)
		}
		if want, got := "hello world|"+`a'b"c$d`+"`e`", strings.TrimSuffix(k.stdout(), "\n"); got != want { // transports may add a trailing newline
			t.Errorf("env round trip = %q, want %q (values are data: never re-parsed by a shell)", k.stdout(), want)
		}
	})
	c.sub(t, "Exec", "TimeoutKillsTheProcessGroup", func(t *testing.T) {
		d := dir(t)
		mustSh(t, sb, "mkdir -p "+d)
		var k collector
		start := time.Now()
		res, err := sb.Exec(ctxT(t), k.req(sandbox.ExecRequest{
			Command: "sleep 300 & echo $! > " + d + "/pid; wait", Cwd: "/workspace", Timeout: 2 * time.Second}))
		if took := time.Since(start); took > 30*time.Second {
			t.Errorf("a 2 s timeout took %v", took)
		}
		if err == nil && res.ExitCode == 0 {
			t.Error("a command killed by its timeout reported success")
		}
		deadline := time.Now().Add(15 * time.Second)
		for {
			out, _ := sh(t, sb, `kill -0 "$(cat `+d+`/pid)" 2>/dev/null && echo alive || echo dead`)
			if strings.Contains(out, "dead") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the child of a timed-out command is still running: the timeout must kill the whole process group")
			}
			time.Sleep(500 * time.Millisecond)
		}
	})
	c.sub(t, "Exec", "ContextCancelStopsTheCommand", func(t *testing.T) {
		ctx, cancel := context.WithCancel(ctxT(t))
		time.AfterFunc(500*time.Millisecond, cancel)
		start := time.Now()
		_, _ = sb.Exec(ctx, sandbox.ExecRequest{Command: "sleep 120", Cwd: "/workspace", Timeout: 5 * time.Minute})
		if took := time.Since(start); took > 20*time.Second {
			t.Fatalf("Exec ignored its cancelled context for %v", took)
		}
	})
	c.sub(t, "Exec", "LargeOutput", func(t *testing.T) {
		const n = 3_000_000
		var k collector
		res, err := sb.Exec(ctxT(t), k.req(sandbox.ExecRequest{Command: fmt.Sprintf("head -c %d /dev/zero | tr '\\0' 'x'", n), Cwd: "/workspace", Timeout: time.Minute}))
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("exit %d, err %v", res.ExitCode, err)
		}
		if got := strings.Count(k.stdout(), "x"); got != n {
			t.Errorf("received %d of %d bytes of stdout", got, n)
		}
	})
	c.sub(t, "Exec", "FilesWrittenByCommandsAreSeenByTheFileAPI", func(t *testing.T) {
		d := dir(t)
		mustSh(t, sb, "mkdir -p "+d+" && printf 'from-shell' > "+d+"/f.txt")
		data, size, err := sb.ReadFile(ctxT(t), d+"/f.txt", 100)
		if err != nil || string(data) != "from-shell" || size != 10 {
			t.Fatalf("ReadFile = %q, %d, %v", data, size, err)
		}
		if err := sb.WriteFile(ctxT(t), d+"/g.txt", []byte("from-api")); err != nil {
			t.Fatal(err)
		}
		if out := strings.TrimSuffix(mustSh(t, sb, "cat "+d+"/g.txt"), "\n"); out != "from-api" { // transports may add a trailing newline
			t.Fatalf("cat = %q", out)
		}
	})
}

// ---- Files ----

func (c *conformance) filesGroup(t *testing.T) {
	if c.groupSkipped(t, "Files") {
		return
	}
	sb := c.create(t, c.newBackend(t))
	c.sub(t, "Files", "WriteReadRoundTripCreatesParents", func(t *testing.T) {
		ctx := ctxT(t)
		data := make([]byte, 0, 1024)
		for i := range 1024 {
			data = append(data, byte(i)) // every byte value, NULs and invalid UTF-8 included
		}
		p := dir(t) + "/a/b/c.bin"
		if err := sb.WriteFile(ctx, p, data); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		got, size, err := sb.ReadFile(ctx, p, 1<<20)
		if err != nil || !bytes.Equal(got, data) || size != int64(len(data)) {
			t.Fatalf("ReadFile = %d bytes (size %d), %v; want the %d bytes written", len(got), size, err, len(data))
		}
	})
	c.sub(t, "Files", "ReadFileCutsAtMaxBytesAndReportsTheSize", func(t *testing.T) {
		ctx := ctxT(t)
		p := dir(t) + "/big.txt"
		if err := sb.WriteFile(ctx, p, bytes.Repeat([]byte("0123456789"), 100)); err != nil {
			t.Fatal(err)
		}
		got, size, err := sb.ReadFile(ctx, p, 10)
		if err != nil || len(got) != 10 || size != 1000 {
			t.Fatalf("ReadFile(max 10) = %d bytes, size %d, %v; want 10 bytes and size 1000", len(got), size, err)
		}
	})
	c.sub(t, "Files", "MissingPathsAreErrNotFound", func(t *testing.T) {
		ctx := ctxT(t)
		p := dir(t) + "/absent"
		if _, _, err := sb.ReadFile(ctx, p, 10); !errors.Is(err, sandbox.ErrNotFound) {
			t.Errorf("ReadFile(missing) = %v, want sandbox.ErrNotFound", err)
		}
		if _, err := sb.Stat(ctx, p); !errors.Is(err, sandbox.ErrNotFound) {
			t.Errorf("Stat(missing) = %v, want sandbox.ErrNotFound", err)
		}
		if _, err := sb.List(ctx, p, 1); !errors.Is(err, sandbox.ErrNotFound) {
			t.Errorf("List(missing) = %v, want sandbox.ErrNotFound", err)
		}
	})
	c.sub(t, "Files", "WriteReplacesAndEmptyFilesWork", func(t *testing.T) {
		ctx := ctxT(t)
		p := dir(t) + "/f.txt"
		for _, content := range []string{"a much longer first version", "short", ""} {
			if err := sb.WriteFile(ctx, p, []byte(content)); err != nil {
				t.Fatal(err)
			}
			got, size, err := sb.ReadFile(ctx, p, 100)
			if err != nil || string(got) != content || size != int64(len(content)) {
				t.Fatalf("after writing %q: read %q (size %d), %v", content, got, size, err)
			}
		}
		if fi, err := sb.Stat(ctx, p); err != nil || fi.Type != "file" || fi.Size != 0 {
			t.Fatalf("Stat(empty file) = %+v, %v", fi, err)
		}
	})
	c.sub(t, "Files", "MkdirListStat", func(t *testing.T) {
		ctx := ctxT(t)
		d := dir(t)
		for range 2 { // idempotent
			if err := sb.MkdirAll(ctx, d+"/x/y"); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
		}
		if err := sb.WriteFile(ctx, d+"/x/y/z.txt", []byte("zz")); err != nil {
			t.Fatal(err)
		}
		if fi, err := sb.Stat(ctx, d+"/x/y"); err != nil || fi.Type != "directory" {
			t.Errorf("Stat(dir) = %+v, %v", fi, err)
		}
		if fi, err := sb.Stat(ctx, d+"/x/y/z.txt"); err != nil || fi.Type != "file" || fi.Size != 2 || fi.ModifiedAt.IsZero() {
			t.Errorf("Stat(file) = %+v, %v (Type \"file\", Size 2, ModifiedAt set)", fi, err)
		}
		one, err := sb.List(ctx, d, 1)
		if err != nil || !paths(one).equal(d+"/x") {
			t.Errorf("List(depth 1) = %v, %v; want [%s/x]", paths(one), err, d)
		}
		all, err := sb.List(ctx, d, 3)
		if err != nil || !paths(all).equal(d+"/x", d+"/x/y", d+"/x/y/z.txt") {
			t.Errorf("List(depth 3) = %v, %v; want x, x/y, x/y/z.txt as absolute sandbox paths", paths(all), err)
		}
		for _, fi := range all {
			if want := map[string]string{d + "/x": "directory", d + "/x/y": "directory", d + "/x/y/z.txt": "file"}[fi.Path]; fi.Type != want {
				t.Errorf("List: %s has Type %q, want %q", fi.Path, fi.Type, want)
			}
		}
	})
	c.sub(t, "Files", "UnusualNames", func(t *testing.T) {
		ctx := ctxT(t)
		p := dir(t) + "/with space/é ü #1 (copy).txt"
		if err := sb.WriteFile(ctx, p, []byte("ok")); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if got, _, err := sb.ReadFile(ctx, p, 10); err != nil || string(got) != "ok" {
			t.Fatalf("ReadFile = %q, %v", got, err)
		}
	})
	c.sub(t, "Files", "LargeFile", func(t *testing.T) {
		ctx := ctxT(t)
		data := bytes.Repeat([]byte("0123456789abcdef"), 4<<16) // 4 MiB
		p := dir(t) + "/large.bin"
		if err := sb.WriteFile(ctx, p, data); err != nil {
			t.Fatalf("WriteFile(4 MiB): %v", err)
		}
		got, size, err := sb.ReadFile(ctx, p, 8<<20)
		if err != nil || !bytes.Equal(got, data) || size != int64(len(data)) {
			t.Fatalf("ReadFile = %d bytes, size %d, %v", len(got), size, err)
		}
	})
	c.sub(t, "Files", "PrepareWorkspaceBuildsTheLayout", func(t *testing.T) {
		ctx := ctxT(t)
		for range 2 {
			if err := sandbox.PrepareWorkspace(ctx, sb); err != nil {
				t.Fatalf("PrepareWorkspace: %v", err)
			}
		}
		for _, d := range sandbox.Layout {
			if fi, err := sb.Stat(ctx, d); err != nil || fi.Type != "directory" {
				t.Errorf("Stat(%s) = %+v, %v", d, fi, err)
			}
		}
	})
	c.sub(t, "Files", "FileAPIStaysInsideTheSandbox", func(t *testing.T) {
		if !c.o.Confined {
			t.Skip("only for host-level adapters (ConformanceOptions.Confined): a container is the boundary of the others")
		}
		ctx := ctxT(t)
		d := dir(t)
		mustSh(t, sb, "mkdir -p "+d+" && ln -s /etc "+d+"/escape")
		if _, _, err := sb.ReadFile(ctx, d+"/escape/hosts", 100); err == nil {
			t.Error("ReadFile followed a symlink out of the sandbox")
		}
		if err := sb.WriteFile(ctx, d+"/escape/conformance-probe", []byte("x")); err == nil {
			t.Error("WriteFile followed a symlink out of the sandbox")
		}
		if _, _, err := sb.ReadFile(ctx, "/workspace/../etc/hosts", 100); err == nil {
			t.Error("ReadFile accepted a path leaving /workspace with ..")
		}
	})
}

type paths []sandbox.FileInfo

func (p paths) equal(want ...string) bool {
	if len(p) != len(want) {
		return false
	}
	have := map[string]bool{}
	for _, fi := range p {
		have[fi.Path] = true
	}
	for _, w := range want {
		if !have[w] {
			return false
		}
	}
	return true
}

func (p paths) String() string {
	var s []string
	for _, fi := range p {
		s = append(s, fi.Path)
	}
	return "[" + strings.Join(s, " ") + "]"
}

// ---- Protocol: the helpers that run scripts through Exec ----

func (c *conformance) protocolGroup(t *testing.T) {
	if c.groupSkipped(t, "Protocol") {
		return
	}
	sb := c.create(t, c.newBackend(t))
	// realDir is the sandbox's own view of a subtest directory (a symlinked /workspace resolves).
	setup := func(t *testing.T) (d, real string) {
		d = dir(t)
		if err := sb.MkdirAll(ctxT(t), d); err != nil {
			t.Fatal(err)
		}
		real, err := sandbox.RealPath(ctxT(t), sb, d)
		if err != nil {
			t.Fatalf("RealPath(%s): %v (needs bash, GNU realpath and tr in the image)", d, err)
		}
		return d, real
	}
	c.sub(t, "Protocol", "RealPathResolvesSymlinksAndMissingTails", func(t *testing.T) {
		d, real := setup(t)
		mustSh(t, sb, "mkdir -p "+d+"/real && ln -s "+d+"/real "+d+"/link")
		got, err := sandbox.RealPaths(ctxT(t), sb, []string{d + "/link/x", d + "/real/y", d + "/missing/z", d + "/link"})
		want := []string{real + "/real/x", real + "/real/y", real + "/missing/z", real + "/real"}
		if err != nil || strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("RealPaths = %v, %v\nwant %v", got, err, want)
		}
	})
	c.sub(t, "Protocol", "RealPathRefusesRelativePaths", func(t *testing.T) {
		if _, err := sandbox.RealPaths(ctxT(t), sb, []string{"relative/path"}); !errors.Is(err, sandbox.ErrRealPath) {
			t.Fatalf("RealPaths(relative) = %v, want sandbox.ErrRealPath", err)
		}
	})
	c.sub(t, "Protocol", "WriteFileNoFollowWritesAndCleansTheStaging", func(t *testing.T) {
		d, _ := setup(t)
		want := []byte("placed\x00binary")
		if err := sandbox.WriteFileNoFollow(ctxT(t), sb, d+"/nf/file.bin", want); err != nil {
			t.Fatalf("WriteFileNoFollow: %v (needs bash, realpath, mv -T)", err)
		}
		got, _, err := sb.ReadFile(ctxT(t), d+"/nf/file.bin", 100)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("ReadFile = %q, %v", got, err)
		}
		if left, err := sb.List(ctxT(t), sandbox.StagingDir, 1); err == nil && len(left) != 0 {
			t.Errorf("staged uploads left behind: %v", paths(left))
		}
	})
	c.sub(t, "Protocol", "WriteFileNoFollowRefusesSymlinks", func(t *testing.T) {
		d, _ := setup(t)
		mustSh(t, sb, "mkdir -p "+d+"/real && printf orig > "+d+"/real/target && ln -s "+d+"/real "+d+"/dirlink && ln -s "+d+"/real/target "+d+"/filelink")
		for _, p := range []string{d + "/dirlink/new.txt", d + "/filelink"} {
			err := sandbox.WriteFileNoFollow(ctxT(t), sb, p, []byte("evil"))
			var se *sandbox.SymlinkError
			if !errors.Is(err, sandbox.ErrSymlinkTarget) || !errors.As(err, &se) {
				t.Errorf("WriteFileNoFollow(%s) = %v, want a *sandbox.SymlinkError (ErrSymlinkTarget)", p, err)
			}
		}
		if got, _, _ := sb.ReadFile(ctxT(t), d+"/real/target", 100); string(got) != "orig" {
			t.Errorf("the symlink's target was overwritten: %q", got)
		}
		if _, _, err := sb.ReadFile(ctxT(t), d+"/real/new.txt", 100); !errors.Is(err, sandbox.ErrNotFound) {
			t.Errorf("a file was created through the directory symlink: %v", err)
		}
		if left, err := sb.List(ctxT(t), sandbox.StagingDir, 1); err == nil && len(left) != 0 {
			t.Errorf("staged uploads left behind after a refusal: %v", paths(left))
		}
	})
	c.sub(t, "Protocol", "ReadRegularFilesSkipsSymlinksDirsAndMissing", func(t *testing.T) {
		d, real := setup(t)
		mustSh(t, sb, "mkdir -p "+d+"/sub && printf hello > "+d+"/f.txt && ln -s "+d+"/f.txt "+d+"/l.txt && head -c 100 /dev/zero | tr '\\0' 'y' > "+d+"/long.txt")
		ps := []string{real + "/f.txt", real + "/l.txt", real + "/sub", real + "/missing", real + "/long.txt"}
		got, err := sandbox.ReadRegularFiles(ctxT(t), sb, ps, nil, 1<<20)
		if err != nil {
			t.Fatalf("ReadRegularFiles: %v (needs bash, GNU stat -c, head -c, base64 -w0)", err)
		}
		byPath := map[string]sandbox.FileRead{}
		for _, f := range got {
			byPath[f.Path] = f
		}
		if len(got) != 2 || string(byPath[ps[0]].Data) != "hello" || len(byPath[ps[4]].Data) != 100 {
			t.Fatalf("got %d files, want f.txt and long.txt only: %+v", len(got), got)
		}
		known := []string{byPath[ps[0]].Stamp, "", "", "", ""}
		again, err := sandbox.ReadRegularFiles(ctxT(t), sb, ps[:1], known, 1<<20)
		if err != nil || len(again) != 1 || !again[0].Unchanged || len(again[0].Data) != 0 {
			t.Errorf("a file matching its known stamp must come back Unchanged and empty: %+v, %v", again, err)
		}
		cut, err := sandbox.ReadRegularFiles(ctxT(t), sb, ps[4:], nil, 10)
		if err != nil || len(cut) != 1 || len(cut[0].Data) != 10 {
			t.Errorf("a budget of 10 bytes must cut the content to 10 bytes: %+v, %v", cut, err)
		}
	})
}
