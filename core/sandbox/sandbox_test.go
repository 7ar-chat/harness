package sandbox_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
)

func TestIsGoneRecognisesTheSandboxNotThePath(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"ErrGone", sandbox.ErrGone, true},
		{"wrapped ErrGone", fmt.Errorf("stat: %w", sandbox.ErrGone), true},
		{"provider code in the text", errors.New("DOCKER::SANDBOX_NOT_FOUND: Sandbox sbx-1 not found."), true},
		{"provider code of another runtime", fmt.Errorf("exec: %w", errors.New("KUBERNETES::SANDBOX_NOT_FOUND")), true},
		{"a missing file is not a gone sandbox", sandbox.ErrNotFound, false},
		{"a wrapped missing file", fmt.Errorf("read: %w", sandbox.ErrNotFound), false},
		{"a transient failure", sandbox.ErrUnavailable, false},
		{"another error", errors.New("boom"), false},
	}
	for _, c := range cases {
		// Act
		got := sandbox.IsGone(c.err)

		// Assert
		if got != c.want {
			t.Errorf("%s: IsGone(%v) = %v, want %v", c.name, c.err, got, c.want)
		}
	}
}

// managerSpy records the Invalidate calls of Retry.
type managerSpy struct {
	sandbox.Manager
	invalidated []string
}

func (m *managerSpy) Invalidate(_ context.Context, threadID, sandboxID string) {
	m.invalidated = append(m.invalidated, threadID+"/"+sandboxID)
}

func TestRetryRunsTheOperationOnceOnASuccessfulOrOrdinaryFailure(t *testing.T) {
	// Arrange
	mgr := &managerSpy{}
	sb := sandboxtest.NewSandbox("sbx-1")
	acquired := 0
	acquire := func(context.Context) (sandbox.Sandbox, error) { acquired++; return sb, nil }
	boom := errors.New("boom")

	// Act
	okErr := sandbox.Retry(context.Background(), mgr, "t1", acquire, func(sandbox.Sandbox) error { return nil })
	opErr := sandbox.Retry(context.Background(), mgr, "t1", acquire, func(sandbox.Sandbox) error { return boom })
	missingErr := sandbox.Retry(context.Background(), mgr, "t1", acquire, func(sandbox.Sandbox) error { return sandbox.ErrNotFound })

	// Assert
	if okErr != nil || !errors.Is(opErr, boom) || !errors.Is(missingErr, sandbox.ErrNotFound) {
		t.Fatalf("errors %v %v %v", okErr, opErr, missingErr)
	}
	if acquired != 3 || len(mgr.invalidated) != 0 {
		t.Fatalf("acquired %d times, invalidated %v; a failure that is not a gone sandbox must not replace it", acquired, mgr.invalidated)
	}
}

func TestRetryReplacesAGoneSandboxOnceAndRunsTheOperationAgain(t *testing.T) {
	// Arrange
	mgr := &managerSpy{}
	first, second := sandboxtest.NewSandbox("sbx-1"), sandboxtest.NewSandbox("sbx-2")
	queue := []sandbox.Sandbox{first, second}
	acquire := func(context.Context) (sandbox.Sandbox, error) { sb := queue[0]; queue = queue[1:]; return sb, nil }
	var ran []string

	// Act
	err := sandbox.Retry(context.Background(), mgr, "t1", acquire, func(sb sandbox.Sandbox) error {
		ran = append(ran, sb.ID())
		if sb.ID() == "sbx-1" {
			return fmt.Errorf("exec: %w", sandbox.ErrGone)
		}
		return nil
	})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ran, ",") != "sbx-1,sbx-2" {
		t.Errorf("operation ran on %v", ran)
	}
	if len(mgr.invalidated) != 1 || mgr.invalidated[0] != "t1/sbx-1" {
		t.Errorf("invalidated %v, want the gone sandbox of the thread", mgr.invalidated)
	}
}

func TestRetryGivesUpAfterOneReplacement(t *testing.T) {
	// A sandbox that is gone again on the replacement is reported, never retried forever.
	// Arrange
	mgr := &managerSpy{}
	calls := 0
	acquire := func(context.Context) (sandbox.Sandbox, error) { return sandboxtest.NewSandbox("sbx"), nil }

	// Act
	err := sandbox.Retry(context.Background(), mgr, "t1", acquire, func(sandbox.Sandbox) error { calls++; return sandbox.ErrGone })

	// Assert
	if !sandbox.IsGone(err) || calls != 2 || len(mgr.invalidated) != 1 {
		t.Fatalf("err %v after %d calls, %d invalidations; want exactly one retry", err, calls, len(mgr.invalidated))
	}
}

func TestRetryDoesNotReplaceTheSandboxOfACancelledCaller(t *testing.T) {
	// A caller that went away sees the operation's error; its sandbox is not thrown away because of it.
	// Arrange
	mgr := &managerSpy{}
	ctx, cancel := context.WithCancel(context.Background())
	acquire := func(context.Context) (sandbox.Sandbox, error) { return sandboxtest.NewSandbox("sbx"), nil }

	// Act
	err := sandbox.Retry(ctx, mgr, "t1", acquire, func(sandbox.Sandbox) error { cancel(); return sandbox.ErrGone })

	// Assert
	if !sandbox.IsGone(err) || len(mgr.invalidated) != 0 {
		t.Fatalf("err %v invalidated %v", err, mgr.invalidated)
	}
}

func TestRetryReturnsAcquireErrorsUnchanged(t *testing.T) {
	// Arrange
	mgr := &managerSpy{}
	quota := sandbox.ErrQuota
	calls := 0
	n := 0
	first := func(context.Context) (sandbox.Sandbox, error) { return nil, quota }
	second := func(context.Context) (sandbox.Sandbox, error) {
		n++
		if n == 1 {
			return sandboxtest.NewSandbox("sbx"), nil
		}
		return nil, sandbox.ErrUnavailable
	}

	// Act
	err1 := sandbox.Retry(context.Background(), mgr, "t", first, func(sandbox.Sandbox) error { calls++; return nil })
	err2 := sandbox.Retry(context.Background(), mgr, "t", second, func(sandbox.Sandbox) error { return sandbox.ErrGone })

	// Assert
	if err1 != quota || calls != 0 {
		t.Errorf("first acquire failure: %v (ops run: %d)", err1, calls)
	}
	if err2 != sandbox.ErrUnavailable {
		t.Errorf("re-acquire failure: %v", err2)
	}
}

func TestPrepareWorkspaceCreatesTheLayoutParentsFirstAndNamesTheFailingDirectory(t *testing.T) {
	// Arrange
	sb := sandboxtest.NewSandbox("sbx")
	broken := &mkdirFails{Sandbox: sb, at: sandbox.ScratchDir}

	// Act
	okErr := sandbox.PrepareWorkspace(context.Background(), sb)
	badErr := sandbox.PrepareWorkspace(context.Background(), broken)

	// Assert
	if okErr != nil {
		t.Fatal(okErr)
	}
	for _, d := range sandbox.Layout {
		if !sb.HasDir(d) {
			t.Errorf("%s was not created", d)
		}
	}
	if badErr == nil || !strings.Contains(badErr.Error(), sandbox.ScratchDir) || !errors.Is(badErr, errMkdir) {
		t.Errorf("failure = %v, want the failing directory named and the cause kept", badErr)
	}
	if got := strings.Join(broken.made, ","); got != sandbox.Workspace+","+sandbox.OutputsDir {
		t.Errorf("created %s before the failure, want the layout in order up to the failing directory", got)
	}
}

var errMkdir = errors.New("read-only file system")

type mkdirFails struct {
	sandbox.Sandbox
	at   string
	made []string
}

func (m *mkdirFails) MkdirAll(ctx context.Context, p string) error {
	if p == m.at {
		return errMkdir
	}
	m.made = append(m.made, p)
	return m.Sandbox.MkdirAll(ctx, p)
}

// streamingSandbox is a sandbox that accepts uploads from a reader.
type streamingSandbox struct {
	sandbox.Sandbox
	got   []byte
	where string
	err   error
}

func (s *streamingSandbox) WriteFileFrom(_ context.Context, p string, r io.Reader) error {
	s.where = p
	s.got, _ = io.ReadAll(r)
	return s.err
}

func TestWriteFromStreamsWhenTheSandboxCanAndBuffersOtherwise(t *testing.T) {
	// Arrange
	ctx := context.Background()
	plain := sandboxtest.NewSandbox("plain")
	inner := sandboxtest.NewSandbox("stream")
	streaming := &streamingSandbox{Sandbox: inner}

	// Act
	plainErr := sandbox.WriteFrom(ctx, plain, "/workspace/a.txt", strings.NewReader("buffered"))
	streamErr := sandbox.WriteFrom(ctx, streaming, "/workspace/b.txt", strings.NewReader("streamed"))

	// Assert
	if plainErr != nil || streamErr != nil {
		t.Fatal(plainErr, streamErr)
	}
	if got, _ := plain.File("/workspace/a.txt"); got != "buffered" {
		t.Errorf("fallback wrote %q", got)
	}
	if string(streaming.got) != "streamed" || streaming.where != "/workspace/b.txt" {
		t.Errorf("streaming sandbox got %q at %q", streaming.got, streaming.where)
	}
	if _, found := inner.File("/workspace/b.txt"); found {
		t.Error("the streaming path also buffered the file through WriteFile")
	}
}

func TestWriteFromReportsAFailingReaderWithoutWritingAPartialFile(t *testing.T) {
	// Arrange
	ctx := context.Background()
	sb := sandboxtest.NewSandbox("plain")
	boom := errors.New("connection reset")
	r := io.MultiReader(strings.NewReader("partial"), &failingReader{boom})

	// Act
	err := sandbox.WriteFrom(ctx, sb, "/workspace/a.txt", r)
	streamErr := sandbox.WriteFrom(ctx, &streamingSandbox{Sandbox: sb, err: boom}, "/workspace/b.txt", bytes.NewReader(nil))

	// Assert
	if !errors.Is(err, boom) {
		t.Errorf("reader failure = %v", err)
	}
	if _, ok := sb.File("/workspace/a.txt"); ok {
		t.Error("a half-read upload was written")
	}
	if !errors.Is(streamErr, boom) {
		t.Errorf("stream failure = %v", streamErr)
	}
}

type failingReader struct{ err error }

func (f *failingReader) Read([]byte) (int, error) { return 0, f.err }

// brokerSandbox is a sandbox that can hold credentials outside the workload.
type brokerSandbox struct{ sandbox.Sandbox }

func (brokerSandbox) SetCredentials(context.Context, sandbox.CredentialSet) error { return nil }

func TestBrokerOfLooksThroughTheWrappersAManagerAddsButNotThroughForeignOnes(t *testing.T) {
	// Arrange
	plain := sandboxtest.NewSandbox("plain")
	broker := brokerSandbox{plain}

	// Act / Assert
	if sandbox.BrokerOf(plain) != nil {
		t.Error("a sandbox without a broker reports one")
	}
	if sandbox.BrokerOf(broker) == nil {
		t.Error("a broker sandbox reports none")
	}
	if sandbox.BrokerOf(sandbox.MarkRecreated(broker)) == nil {
		t.Error("the recreated wrapper hid the broker")
	}
	if sandbox.BrokerOf(sandbox.MarkRecreated(sandbox.MarkRecreated(plain))) != nil {
		t.Error("nested wrappers around a plain sandbox report a broker")
	}
}

func TestUserTagIsAStableOpaqueIdentifier(t *testing.T) {
	// Arrange / Act
	a, a2, b := sandbox.UserTag("alice"), sandbox.UserTag("alice"), sandbox.UserTag("bob")

	// Assert
	if a != a2 || a == b || len(a) != 40 || strings.Contains(a, "alice") {
		t.Fatalf("tags %q %q %q", a, a2, b)
	}
}
