package browsersandbox

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/sandbox"
)

// fakeBox is a browser sandbox of a fake provider: it records what ran in it and answers the readiness check after
// readyAfter polls.
type fakeBox struct {
	id         string
	backend    *fakeBackend
	mu         sync.Mutex
	cmds       []string
	files      map[string][]byte
	polls      int
	readyAfter int
	noStart    bool
	dead       bool
	renewed    int
}

func (f *fakeBox) ID() string { return f.id }
func (f *fakeBox) Exec(_ context.Context, r sandbox.ExecRequest) (sandbox.ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = append(f.cmds, r.Command)
	if strings.HasPrefix(r.Command, "test -f /tmp/browser.ready") {
		f.polls++
		if f.polls > f.readyAfter {
			return sandbox.ExecResult{}, nil
		}
		return sandbox.ExecResult{ExitCode: 1}, nil
	}
	if f.noStart {
		return sandbox.ExecResult{ExitCode: 127}, nil
	}
	return sandbox.ExecResult{}, nil
}
func (f *fakeBox) ReadFile(context.Context, string, int64) ([]byte, int64, error) {
	return nil, 0, errors.New("unused")
}
func (f *fakeBox) WriteFile(_ context.Context, p string, d []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.files == nil {
		f.files = map[string][]byte{}
	}
	f.files[p] = d
	return nil
}
func (f *fakeBox) MkdirAll(context.Context, string) error                        { return nil }
func (f *fakeBox) List(context.Context, string, int) ([]sandbox.FileInfo, error) { return nil, nil }
func (f *fakeBox) Stat(context.Context, string) (sandbox.FileInfo, error) {
	return sandbox.FileInfo{}, nil
}
func (f *fakeBox) Healthy(context.Context) bool               { return !f.dead }
func (f *fakeBox) Gone(context.Context) bool                  { return f.dead }
func (f *fakeBox) Renew(context.Context, time.Duration) error { f.renewed++; return nil }
func (f *fakeBox) Endpoint(_ context.Context, port int) (sandbox.Endpoint, error) {
	return sandbox.Endpoint{URL: "https://" + string(rune('0'+port%10)) + "-" + f.id + ".example", Headers: map[string]string{"x-token": "t-" + f.id}}, nil
}

type fakeBackend struct {
	mu      sync.Mutex
	created []*fakeBox
	killed  []string
	opts    []sandbox.CreateOptions
	failNew error
	noPorts bool
	mk      func(*fakeBox)
}

func (b *fakeBackend) Create(_ context.Context, o sandbox.CreateOptions) (sandbox.Instance, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failNew != nil {
		return nil, b.failNew
	}
	fb := &fakeBox{id: "sbx" + string(rune('1'+len(b.created))), backend: b}
	if b.mk != nil {
		b.mk(fb)
	}
	b.created = append(b.created, fb)
	b.opts = append(b.opts, o)
	if b.noPorts {
		return noPortsBox{fb}, nil
	}
	return fb, nil
}
func (b *fakeBackend) Connect(context.Context, string) (sandbox.Instance, error) {
	return nil, sandbox.ErrNoSandbox
}
func (b *fakeBackend) Kill(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.killed = append(b.killed, id)
	return nil
}

// noPortsBox hides the port access of a box.
type noPortsBox struct{ *fakeBox }

func (noPortsBox) Endpoint() {}

func newManager(b *fakeBackend) *Manager {
	m := New(b)
	m.StartTimeout = 5 * time.Second
	return m
}

func TestAConversationGetsABrowserSandboxThatItKeepsAcrossRuns(t *testing.T) {
	b := &fakeBackend{}
	m := newManager(b)
	defer m.Close()

	first, err := m.Servers(context.Background(), "thread-1", "alice")
	if err != nil || len(first) != 1 {
		t.Fatalf("first run: %v, %v", first, err)
	}
	s := first[0]
	if s.URL != "https://1-sbx1.example/mcp" || s.Headers["x-token"] != "t-sbx1" {
		t.Fatalf("server = %+v", s)
	}
	if b.opts[0].Profile != Profile || b.opts[0].Metadata["thread"] != "thread-1" {
		t.Fatalf("create options = %+v", b.opts[0])
	}
	if !strings.Contains(b.created[0].cmds[0], "browser-up") {
		t.Fatalf("the browser was not started: %v", b.created[0].cmds)
	}

	second, err := m.Servers(context.Background(), "thread-1", "alice")
	if err != nil || second[0].URL != s.URL || len(b.created) != 1 {
		t.Fatalf("the next run must reuse the sandbox: %+v %v (%d created)", second, err, len(b.created))
	}
	if b.created[0].renewed == 0 {
		t.Error("a reused sandbox must have its lifetime extended")
	}
	other, err := m.Servers(context.Background(), "thread-2", "alice")
	if err != nil || other[0].URL == s.URL || len(b.created) != 2 {
		t.Fatalf("another conversation needs its own browser: %+v %v", other, err)
	}
}

func TestWaitsUntilTheBrowserIsReady(t *testing.T) {
	b := &fakeBackend{mk: func(f *fakeBox) { f.readyAfter = 1 }}
	m := newManager(b)
	defer m.Close()
	if _, err := m.Servers(context.Background(), "t", "alice"); err != nil {
		t.Fatal(err)
	}
	if b.created[0].polls < 2 {
		t.Fatalf("polled %d times, want it to wait for the ready file", b.created[0].polls)
	}
}

func TestAFailedStartDeletesTheSandboxAndTheNextRunTriesAgain(t *testing.T) {
	b := &fakeBackend{mk: func(f *fakeBox) { f.noStart = len(f.backend.created) == 0 }}
	m := newManager(b)
	defer m.Close()
	if _, err := m.Servers(context.Background(), "t", "alice"); err == nil {
		t.Fatal("a browser that cannot start must be an error")
	}
	if len(b.killed) != 1 || b.killed[0] != "sbx1" {
		t.Fatalf("killed = %v, want the half-started sandbox deleted", b.killed)
	}
	if got, err := m.Servers(context.Background(), "t", "alice"); err != nil || len(got) != 1 || len(b.created) != 2 {
		t.Fatalf("retry: %v %v (%d created)", got, err, len(b.created))
	}
}

func TestAProviderThatCannotExposePortsIsRefused(t *testing.T) {
	b := &fakeBackend{noPorts: true}
	m := newManager(b)
	defer m.Close()
	_, err := m.Servers(context.Background(), "t", "alice")
	if err == nil || !strings.Contains(err.Error(), "cannot expose") || len(b.killed) != 1 {
		t.Fatalf("err = %v, killed = %v", err, b.killed)
	}
}

func TestCreateFailureIsReported(t *testing.T) {
	m := newManager(&fakeBackend{failNew: errors.New("quota")})
	defer m.Close()
	if _, err := m.Servers(context.Background(), "t", "alice"); err == nil || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("err = %v", err)
	}
}

func TestADeadSandboxIsReplaced(t *testing.T) {
	b := &fakeBackend{}
	m := newManager(b)
	defer m.Close()
	if _, err := m.Servers(context.Background(), "t", "alice"); err != nil {
		t.Fatal(err)
	}
	b.created[0].dead = true
	got, err := m.Servers(context.Background(), "t", "alice")
	if err != nil || got[0].URL != "https://1-sbx2.example/mcp" || len(b.killed) != 1 {
		t.Fatalf("got %+v, %v, killed %v", got, err, b.killed)
	}
}

func TestIdleSandboxesAreDeleted(t *testing.T) {
	b := &fakeBackend{}
	m := newManager(b)
	m.Idle = 10 * time.Minute
	defer m.Close()
	if _, err := m.Servers(context.Background(), "t", "alice"); err != nil {
		t.Fatal(err)
	}
	if n := m.Sweep(time.Now().Add(5 * time.Minute)); n != 0 {
		t.Fatalf("swept %d sandboxes still in use", n)
	}
	if n := m.Sweep(time.Now().Add(11 * time.Minute)); n != 1 || len(b.killed) != 1 {
		t.Fatalf("swept %d, killed %v", n, b.killed)
	}
	if _, err := m.Servers(context.Background(), "t", "alice"); err != nil || len(b.created) != 2 {
		t.Fatalf("after the sweep a new browser starts: %v (%d created)", err, len(b.created))
	}
}

// fakeLibrary is a document library whose content can change.
type fakeLibrary struct {
	sig    string
	copies int
}

func (l *fakeLibrary) Signature(string) (string, error) { return l.sig, nil }
func (l *fakeLibrary) CopyTo(ctx context.Context, login string, sb sandbox.Sandbox) error {
	l.copies++
	return sb.WriteFile(ctx, "/docs/"+login+"/cv.pdf", []byte(l.sig))
}

func TestTheUsersDocumentsAreCopiedWhenTheyChange(t *testing.T) {
	lib := &fakeLibrary{sig: "v1"}
	b := &fakeBackend{}
	m := newManager(b)
	m.Docs = lib
	defer m.Close()
	for _, run := range []struct {
		sig    string
		copies int
	}{{"v1", 1}, {"v1", 1}, {"v2", 2}} {
		lib.sig = run.sig
		if _, err := m.Servers(context.Background(), "t", "alice"); err != nil {
			t.Fatal(err)
		}
		if lib.copies != run.copies {
			t.Fatalf("after a run with library %q: %d copies, want %d", run.sig, lib.copies, run.copies)
		}
	}
	if string(b.created[0].files["/docs/alice/cv.pdf"]) != "v2" {
		t.Fatalf("files = %v", b.created[0].files)
	}
}

func TestViewURLOnlyForAConversationWithABrowser(t *testing.T) {
	b := &fakeBackend{}
	m := newManager(b)
	defer m.Close()
	if u, _ := m.ViewURL(context.Background(), "t"); u != "" {
		t.Fatalf("view of a conversation without browser = %q", u)
	}
	if _, err := m.Servers(context.Background(), "t", "alice"); err != nil {
		t.Fatal(err)
	}
	u, h := m.ViewURL(context.Background(), "t")
	if !strings.Contains(u, "-sbx1.example/vnc.html") || h["x-token"] != "t-sbx1" {
		t.Fatalf("view = %q %v", u, h)
	}
}
