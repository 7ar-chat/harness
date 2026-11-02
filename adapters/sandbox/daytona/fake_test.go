package daytona

import (
	"context"
	"net/http"
	"sync"
	"time"

	sdkerrors "github.com/daytonaio/daytona/libs/sdk-go/pkg/errors"
)

func notFound() error   { return sdkerrors.NewDaytonaNotFoundError("sandbox not found", nil) }
func serverErr() error  { return sdkerrors.NewDaytonaServerError("boom", http.StatusBadGateway, nil) }
func quotaErr() error   { return sdkerrors.NewDaytonaForbiddenError("Total CPU quota exceeded", nil) }
func plainErr() error   { return sdkerrors.NewDaytonaError("connection refused", 0, nil) }
func badRequest() error { return sdkerrors.NewDaytonaValidationError("bad path", nil) }

// fakeAPI is a scripted account.
type fakeAPI struct {
	mu        sync.Mutex
	boxes     map[string]*fakeBox
	created   []createParams
	createErr error
	createFn  func(createParams) error // decides per attempt; nil: succeed
	getErr    error
	autoStop  map[string]int
	stopErr   error
	next      *fakeBox // returned by Create
}

func newFakeAPI() *fakeAPI { return &fakeAPI{boxes: map[string]*fakeBox{}, autoStop: map[string]int{}} }

func (a *fakeAPI) add(b *fakeBox) *fakeBox {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.boxes[b.id] = b
	return b
}

func (a *fakeAPI) Create(_ context.Context, p createParams) (box, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.created = append(a.created, p)
	if a.createErr != nil {
		return nil, a.createErr
	}
	if a.createFn != nil {
		if err := a.createFn(p); err != nil {
			return nil, err
		}
	}
	b := a.next
	if b == nil {
		b = newFakeBox("sb-new")
	}
	a.boxes[b.id] = b
	return b, nil
}

func (a *fakeAPI) Get(_ context.Context, id string) (box, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.getErr != nil {
		return nil, a.getErr
	}
	b, ok := a.boxes[id]
	if !ok {
		return nil, notFound()
	}
	return b, nil
}

func (a *fakeAPI) SetAutoStop(_ context.Context, id string, m int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopErr != nil {
		return a.stopErr
	}
	a.autoStop[id] = m
	return nil
}

// fakeBox is a scripted sandbox. Hooks default to a healthy, empty sandbox.
type fakeBox struct {
	mu   sync.Mutex
	id   string
	info boxInfo

	refreshErr error
	startErr   error
	deleteErr  error
	started    bool
	deleted    bool

	// Run
	runs    []string
	runFunc func(cmd string) (int, string, error)

	// sessions
	sessions    []string
	deleted_    []string
	sessionErr  error
	startCmdErr error
	cmdScript   string
	exitAfter   int // polls before the exit code is known
	exitCode    int
	polls       int
	pollErr     error
	chunks      []chunk // streamed in order
	streamErr   error
	streamBlock bool // keep the stream open until the context ends
	logsOut     string
	logsErr     string

	// files
	files      map[string][]byte
	dirs       map[string][]fileEntry
	fileErr    error
	uploadErr  error
	uploads    []string
	downloadFn func(p string) ([]byte, error)
}

type chunk struct {
	stderr bool
	text   string
}

func newFakeBox(id string) *fakeBox {
	return &fakeBox{id: id, info: boxInfo{State: "started"}, files: map[string][]byte{}, dirs: map[string][]fileEntry{}}
}

func (b *fakeBox) ID() string { b.mu.Lock(); defer b.mu.Unlock(); return b.id }
func (b *fakeBox) Info() boxInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.info
}
func (b *fakeBox) Refresh(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.refreshErr
}
func (b *fakeBox) Start(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.started = b.startErr == nil
	if b.started {
		b.info.State = "started"
	}
	return b.startErr
}
func (b *fakeBox) Delete(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deleted = b.deleteErr == nil
	return b.deleteErr
}

func (b *fakeBox) Run(_ context.Context, cmd, _ string, _ map[string]string) (int, string, error) {
	b.mu.Lock()
	b.runs = append(b.runs, cmd)
	f := b.runFunc
	b.mu.Unlock()
	if f != nil {
		return f(cmd)
	}
	return 0, "", nil
}

func (b *fakeBox) ranContaining(s string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, r := range b.runs {
		if contains(r, s) {
			return true
		}
	}
	return false
}

func (b *fakeBox) CreateSession(_ context.Context, s string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sessionErr != nil {
		return b.sessionErr
	}
	b.sessions = append(b.sessions, s)
	return nil
}

func (b *fakeBox) DeleteSession(_ context.Context, s string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deleted_ = append(b.deleted_, s)
	return nil
}

func (b *fakeBox) StartCommand(_ context.Context, _, cmd string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.startCmdErr != nil {
		return "", b.startCmdErr
	}
	b.cmdScript = cmd
	return "cmd-1", nil
}

func (b *fakeBox) CommandExit(context.Context, string, string) (int, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pollErr != nil {
		return 0, false, b.pollErr
	}
	b.polls++
	if b.polls > b.exitAfter {
		return b.exitCode, true, nil
	}
	return 0, false, nil
}

func (b *fakeBox) StreamLogs(ctx context.Context, _, _ string, stdout, stderr chan<- string) error {
	defer close(stdout)
	defer close(stderr)
	b.mu.Lock()
	chunks, err, block := b.chunks, b.streamErr, b.streamBlock
	b.mu.Unlock()
	for _, c := range chunks {
		ch := stdout
		if c.stderr {
			ch = stderr
		}
		select {
		case ch <- c.text:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err != nil {
		return err
	}
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (b *fakeBox) CommandLogs(context.Context, string, string) (string, string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.logsOut, b.logsErr, nil
}

func (b *fakeBox) ListFiles(_ context.Context, dir string) ([]fileEntry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fileErr != nil {
		return nil, b.fileErr
	}
	es, ok := b.dirs[dir]
	if !ok {
		return nil, notFound()
	}
	return es, nil
}

func (b *fakeBox) FileInfo(_ context.Context, p string) (fileEntry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fileErr != nil {
		return fileEntry{}, b.fileErr
	}
	if d, ok := b.files[p]; ok {
		return fileEntry{Name: p, Size: int64(len(d)), MTime: time.Unix(1700000000, 0)}, nil
	}
	if _, ok := b.dirs[p]; ok {
		return fileEntry{Name: p, IsDir: true}, nil
	}
	return fileEntry{}, notFound()
}

func (b *fakeBox) Download(_ context.Context, p string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.downloadFn != nil {
		return b.downloadFn(p)
	}
	if b.fileErr != nil {
		return nil, b.fileErr
	}
	d, ok := b.files[p]
	if !ok {
		return nil, notFound()
	}
	return d, nil
}

func (b *fakeBox) Upload(_ context.Context, p string, data []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.uploadErr != nil {
		return b.uploadErr
	}
	b.uploads = append(b.uploads, p)
	b.files[p] = data
	return nil
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
