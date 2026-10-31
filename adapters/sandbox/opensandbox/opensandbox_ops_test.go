package opensandbox_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/sandbox/opensandbox"
	"github.com/7ar-chat/harness/core/sandbox"
)

// output collects the streamed text of a command under a lock (callbacks may come from another goroutine).
type output struct {
	mu             sync.Mutex
	stdout, stderr []string
}

func (o *output) req(cmd string) sandbox.ExecRequest {
	return sandbox.ExecRequest{
		Command:  cmd,
		Cwd:      "/workspace",
		OnStdout: func(s string) { o.mu.Lock(); o.stdout = append(o.stdout, s); o.mu.Unlock() },
		OnStderr: func(s string) { o.mu.Lock(); o.stderr = append(o.stderr, s); o.mu.Unlock() },
	}
}

func (f *fakeOSB) commandReply(events ...string) {
	f.handle("POST /execd/command", func(w http.ResponseWriter, _ *http.Request) { writeSSE(w, events...) })
}

func TestExecStreamsOutputLinesAndReturnsTheExitCode(t *testing.T) {
	// Each stdout/stderr event reaches its callback as a newline-terminated line, and a non-zero exit is a result, not an error.
	// Arrange
	f := newFakeOSB(t)
	inst := f.instance(t)
	f.commandReply(
		`{"type":"init","text":"exec-7"}`,
		`{"type":"stdout","text":"hello"}`,
		`{"type":"stderr","text":"warn\n"}`,
		`{"type":"stdout","text":"world"}`,
		`{"type":"error","error":{"ename":"CommandExecError","evalue":"3","exit_code":3}}`,
		`{"type":"execution_complete","execution_time":12}`,
	)
	var out output

	// Act
	res, err := inst.Exec(context.Background(), out.req("make"))

	// Assert
	if err != nil {
		t.Fatalf("a non-zero exit must not be an error: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}
	if got := strings.Join(out.stdout, "|"); got != "hello\n|world\n" {
		t.Errorf("stdout = %q", got)
	}
	if got := strings.Join(out.stderr, "|"); got != "warn\n" {
		t.Errorf("stderr = %q (an already terminated line must not get a second newline)", got)
	}
}

func TestExecSendsCommandCwdTimeoutAndEnvironment(t *testing.T) {
	// Arrange
	f := newFakeOSB(t)
	inst := f.instance(t)

	// Act
	res, err := inst.Exec(context.Background(), sandbox.ExecRequest{
		Command: "echo $A", Cwd: "/workspace/sub", Timeout: 1500 * time.Millisecond, Env: map[string]string{"A": "1"},
	})

	// Assert
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("Exec = %+v, %v", res, err)
	}
	var got struct {
		Command string
		Cwd     string
		Timeout int64
		Envs    map[string]string
	}
	if err := json.Unmarshal(f.requests("POST /execd/command")[0].Body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Command != "echo $A" || got.Cwd != "/workspace/sub" || got.Timeout != 1500 || got.Envs["A"] != "1" {
		t.Fatalf("command request = %+v (timeout is in milliseconds)", got)
	}
}

func TestExecWithoutCallbacksStillRuns(t *testing.T) {
	// Arrange
	f := newFakeOSB(t)
	inst := f.instance(t)
	f.commandReply(`{"type":"stdout","text":"ignored"}`, `{"type":"stderr","text":"ignored"}`, `{"type":"execution_complete","exit_code":0}`)

	// Act
	res, err := inst.Exec(context.Background(), sandbox.ExecRequest{Command: "true"})

	// Assert
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("Exec = %+v, %v", res, err)
	}
}

func TestExecExitCodePrecedence(t *testing.T) {
	for _, c := range []struct {
		name    string
		events  []string
		want    int
		wantErr string
	}{
		{"complete carries the exit code", []string{`{"type":"execution_complete","exit_code":7}`}, 7, ""},
		{"error exit code wins over a later complete", []string{`{"type":"error","error":{"ename":"E","evalue":"x","exit_code":2}}`, `{"type":"execution_complete","exit_code":0}`}, 2, ""},
		{"numeric error value (older servers)", []string{`{"type":"error","ename":"CommandExecError","evalue":"126"}`, `{"type":"execution_complete"}`}, 126, ""},
		{"a non-numeric error without exit code is an error", []string{`{"type":"error","ename":"Timeout","evalue":"deadline exceeded"}`, `{"type":"execution_complete"}`}, 0, "Timeout: deadline exceeded"},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			f := newFakeOSB(t)
			inst := f.instance(t)
			f.commandReply(c.events...)

			// Act
			res, err := inst.Exec(context.Background(), sandbox.ExecRequest{Command: "x"})

			// Assert
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil || res.ExitCode != c.want {
				t.Fatalf("Exec = %+v, %v; want exit %d", res, err, c.want)
			}
		})
	}
}

func TestExecErrorClassification(t *testing.T) {
	// A provider 404 for the command means "sandbox gone" only when the lifecycle confirms it; server errors stay plain errors.
	for _, c := range []struct {
		name     string
		status   int
		body     string
		state    string
		wantGone bool
	}{
		{"SANDBOX_NOT_FOUND code", 404, notFoundBody, "Running", true},
		{"404 and lifecycle says terminated", 404, `{"code":"NOT_FOUND","message":"gone"}`, "Terminated", true},
		{"404 but the sandbox still runs", 404, `{"code":"NOT_FOUND","message":"no route"}`, "Running", false},
		{"500", 500, `{"code":"BOOM","message":"exec failed"}`, "Running", false},
		{"400", 400, `{"code":"BAD","message":"empty command"}`, "Running", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			f := newFakeOSB(t)
			inst := f.instance(t)
			f.reply("POST /execd/command", c.status, c.body)
			f.setState(c.state)

			// Act
			_, err := inst.Exec(context.Background(), sandbox.ExecRequest{Command: "x"})

			// Assert
			if err == nil {
				t.Fatal("expected an error")
			}
			if sandbox.IsGone(err) != c.wantGone {
				t.Fatalf("IsGone = %v for %v", !c.wantGone, err)
			}
			if errors.Is(err, sandbox.ErrNotFound) {
				t.Fatalf("a command error must never read as a missing file: %v", err)
			}
		})
	}
}

func TestExecSurfacesABrokenEventStream(t *testing.T) {
	// A stream that ends without any event is an error, not a silent success with exit code 0.
	// Arrange
	f := newFakeOSB(t)
	inst := f.instance(t)
	f.handle("POST /execd/command", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
	})

	// Act
	res, err := inst.Exec(context.Background(), sandbox.ExecRequest{Command: "x"})

	// Assert
	if err == nil {
		t.Fatalf("empty stream accepted: %+v", res)
	}
}

func TestExecCancellationReturnsTheContextErrorAndInterruptsTheCommand(t *testing.T) {
	// A cancelled run reports ctx.Err() (not a transport error) and best-effort interrupts the remote command by its ID.
	// Arrange
	f := newFakeOSB(t)
	inst := f.instance(t)
	interrupted := make(chan string, 1)
	f.handle("POST /execd/command", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"type":"init","text":"exec-9"}`+"\n\n"+`{"type":"stdout","text":"go"}`+"\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	f.handle("DELETE /execd/command", func(w http.ResponseWriter, r *http.Request) {
		interrupted <- r.URL.Query().Get("id")
		w.WriteHeader(200)
	})
	ctx, cancel := context.WithCancel(context.Background())

	// Act
	// (the first output line proves the init event, carrying the execution ID, was processed before the cancel)
	_, err := inst.Exec(ctx, sandbox.ExecRequest{Command: "sleep 100", OnStdout: func(string) { cancel() }})

	// Assert
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	select {
	case id := <-interrupted:
		if id != "exec-9" {
			t.Fatalf("interrupted command %q, want exec-9", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the remote command was never interrupted")
	}
}

func TestExecInterruptSendsTheProxyAndExecdCredentials(t *testing.T) {
	// Through the server proxy the API key doubles as the execd token; an endpoint-provided token is not overwritten.
	for _, c := range []struct {
		name      string
		endpoint  map[string]string
		wantToken string
	}{
		{"api key becomes the token", nil, "key-1"},
		{"endpoint token wins", map[string]string{"X-EXECD-ACCESS-TOKEN": "from-endpoint"}, "from-endpoint"},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			f := newFakeOSB(t)
			f.setEndpointHeaders(c.endpoint)
			o := f.opts()
			o.APIKey, o.UseServerProxy = "key-1", true
			b, err := opensandbox.NewOpenSandbox(o)
			if err != nil {
				t.Fatal(err)
			}
			inst, err := b.Connect(context.Background(), "sbx-1")
			if err != nil {
				t.Fatal(err)
			}
			got := make(chan http.Header, 1)
			f.handle("POST /execd/command", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				_, _ = io.WriteString(w, `{"type":"init","text":"e1"}`+"\n\n"+`{"type":"stdout","text":"go"}`+"\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			})
			f.handle("DELETE /execd/command", func(w http.ResponseWriter, r *http.Request) { got <- r.Header.Clone() })
			ctx, cancel := context.WithCancel(context.Background())

			// Act
			_, _ = inst.Exec(ctx, sandbox.ExecRequest{Command: "x", OnStdout: func(string) { cancel() }})

			// Assert
			select {
			case h := <-got:
				if h.Get("X-EXECD-ACCESS-TOKEN") != c.wantToken {
					t.Errorf("execd token = %q, want %q", h.Get("X-EXECD-ACCESS-TOKEN"), c.wantToken)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no interrupt reached the server")
			}
		})
	}
}

func TestExecCancelledBeforeTheCommandIsKnownDoesNotInterrupt(t *testing.T) {
	// Without an execution ID there is nothing to interrupt; the caller still gets its context error.
	// Arrange
	f := newFakeOSB(t)
	inst := f.instance(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Act
	_, err := inst.Exec(ctx, sandbox.ExecRequest{Command: "x"})

	// Assert
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if n := len(f.requests("DELETE /execd/command")); n != 0 {
		t.Fatalf("interrupts = %d, want none", n)
	}
}

func TestStatReportsFilesAndDirectories(t *testing.T) {
	// Arrange
	f := newFakeOSB(t)
	f.putFile("/workspace/a.txt", []byte("hello"))
	inst := f.instance(t)

	// Act
	file, fileErr := inst.Stat(context.Background(), "/workspace/a.txt")
	dir, dirErr := inst.Stat(context.Background(), "/workspace")

	// Assert
	if fileErr != nil || file.Type != "file" || file.Size != 5 || file.Path != "/workspace/a.txt" || file.ModifiedAt.IsZero() {
		t.Errorf("file stat = %+v, %v", file, fileErr)
	}
	if dirErr != nil || dir.Type != "directory" {
		t.Errorf("dir stat = %+v, %v", dir, dirErr)
	}
}

func TestStatAcceptsAnEntryKeyedByAnotherPath(t *testing.T) {
	// execd may key the answer by a resolved path; the single entry is still the answer, and a missing "path" field falls back to its key.
	// Arrange
	f := newFakeOSB(t)
	inst := f.instance(t)
	f.reply("GET /execd/files/info", 200, `{"/real/a.txt":{"type":"dir","size":0,"modified_at":"2026-10-06T00:00:00Z"}}`)

	// Act
	fi, err := inst.Stat(context.Background(), "/link/a.txt")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if fi.Path != "/real/a.txt" || fi.Type != "directory" {
		t.Fatalf("stat = %+v", fi)
	}
}

func TestStatOfAnEmptyAnswerIsNotFound(t *testing.T) {
	// Arrange
	f := newFakeOSB(t)
	inst := f.instance(t)
	f.reply("GET /execd/files/info", 200, `{}`)

	// Act
	_, err := inst.Stat(context.Background(), "/workspace/x")

	// Assert
	if !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestStatDistinguishesAMissingFileFromAServerFailure(t *testing.T) {
	// Arrange
	f := newFakeOSB(t)
	inst := f.instance(t)
	ctx := context.Background()
	_, missing := inst.Stat(ctx, "/workspace/missing")
	f.reply("GET /execd/files/info", 500, `{"code":"BOOM","message":"disk error"}`)

	// Act
	_, broken := inst.Stat(ctx, "/workspace/x")

	// Assert
	if !errors.Is(missing, sandbox.ErrNotFound) || sandbox.IsGone(missing) {
		t.Errorf("missing file: %v", missing)
	}
	if broken == nil || errors.Is(broken, sandbox.ErrNotFound) || sandbox.IsGone(broken) {
		t.Fatalf("a 500 must stay a plain error: %v", broken)
	}
}

func TestStatOfAMalformedAnswerFails(t *testing.T) {
	// Arrange
	f := newFakeOSB(t)
	inst := f.instance(t)
	f.reply("GET /execd/files/info", 200, `{"/workspace/x": "not an object"`)

	// Act
	_, err := inst.Stat(context.Background(), "/workspace/x")

	// Assert
	if err == nil || errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("err = %v, want a decode error", err)
	}
}

func TestReadFileHonoursTheByteLimitAndReportsTheFullSize(t *testing.T) {
	for _, c := range []struct {
		name       string
		max        int64
		ignoreRg   bool
		wantData   string
		wantSize   int64
		wantRanged bool
	}{
		{"no limit reads everything", 0, false, "0123456789", 10, false},
		{"limit above the size reads everything", 100, false, "0123456789", 10, false},
		{"limit equal to the size is not ranged", 10, false, "0123456789", 10, false},
		{"limit below the size asks for a range", 4, false, "0123", 10, true},
		{"a server that ignores the range is still cut at the limit", 4, true, "0123", 10, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			f := newFakeOSB(t)
			f.ignoreRg = c.ignoreRg
			f.putFile("/workspace/big.txt", []byte("0123456789"))
			inst := f.instance(t)

			// Act
			data, size, err := inst.ReadFile(context.Background(), "/workspace/big.txt", c.max)

			// Assert
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != c.wantData || size != c.wantSize {
				t.Fatalf("data %q size %d, want %q size %d", data, size, c.wantData, c.wantSize)
			}
			rg := f.requests("GET /execd/files/download")[0].Header.Get("Range")
			if (rg != "") != c.wantRanged || (c.wantRanged && rg != "bytes=0-3") {
				t.Fatalf("Range header = %q", rg)
			}
		})
	}
}

func TestReadFileSizeIsNeverBelowWhatWasRead(t *testing.T) {
	// A file that grew between the stat and the download reports at least the bytes returned.
	// Arrange
	f := newFakeOSB(t)
	f.putFile("/workspace/grow.txt", []byte("abc"))
	inst := f.instance(t)
	f.handle("GET /execd/files/download", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("abcdef")) })

	// Act
	data, size, err := inst.ReadFile(context.Background(), "/workspace/grow.txt", 0)

	// Assert
	if err != nil || string(data) != "abcdef" || size != 6 {
		t.Fatalf("ReadFile = %q, %d, %v", data, size, err)
	}
}

func TestReadFileErrors(t *testing.T) {
	// Arrange
	f := newFakeOSB(t)
	f.putFile("/workspace/d/x", []byte("x"))
	f.putFile("/workspace/a.txt", []byte("abc"))
	inst := f.instance(t)
	ctx := context.Background()

	// Act
	_, _, missing := inst.ReadFile(ctx, "/workspace/nope", 10)
	_, _, dir := inst.ReadFile(ctx, "/workspace/d", 10)
	f.reply("GET /execd/files/download", 404, fileNotFoundBody) // the file vanished after the stat
	_, _, vanished := inst.ReadFile(ctx, "/workspace/a.txt", 10)
	f.reply("GET /execd/files/download", 500, `{"code":"BOOM","message":"io error"}`)
	_, _, broken := inst.ReadFile(ctx, "/workspace/a.txt", 10)

	// Assert
	if !errors.Is(missing, sandbox.ErrNotFound) {
		t.Errorf("missing file: %v", missing)
	}
	if dir == nil || errors.Is(dir, sandbox.ErrNotFound) || !strings.Contains(dir.Error(), "is a directory") {
		t.Errorf("directory: %v", dir)
	}
	if !errors.Is(vanished, sandbox.ErrNotFound) || sandbox.IsGone(vanished) {
		t.Errorf("file vanished between stat and download: %v", vanished)
	}
	if broken == nil || errors.Is(broken, sandbox.ErrNotFound) {
		t.Errorf("500 on download: %v", broken)
	}
}

func TestReadFileFailsWhenTheBodyIsCutShort(t *testing.T) {
	// A connection dropped mid-download must not return a truncated file as if complete.
	// Arrange
	f := newFakeOSB(t)
	f.putFile("/workspace/a.txt", []byte("abcdef"))
	inst := f.instance(t)
	f.handle("GET /execd/files/download", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("abc"))
	})

	// Act
	data, _, err := inst.ReadFile(context.Background(), "/workspace/a.txt", 0)

	// Assert
	if err == nil {
		t.Fatalf("a truncated body was returned as %q", data)
	}
}

func TestWriteFileCreatesParentsThenUploadsWithTheFinalPath(t *testing.T) {
	// Arrange
	f := newFakeOSB(t)
	inst := f.instance(t)

	// Act
	err := inst.WriteFile(context.Background(), "/workspace/out/report.txt", []byte("data"))

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	mk := f.requests("POST /execd/directories")
	if len(mk) != 1 || !strings.Contains(string(mk[0].Body), `"/workspace/out"`) {
		t.Fatalf("mkdir requests = %+v", mk)
	}
	ups := f.uploaded()
	if len(ups) != 1 || ups[0].Path != "/workspace/out/report.txt" || ups[0].FileName != "report.txt" || string(ups[0].Data) != "data" || ups[0].Mode != 644 {
		t.Fatalf("uploads = %+v", ups)
	}
}

func TestWriteFileAtTheRootSkipsMkdir(t *testing.T) {
	// Arrange
	f := newFakeOSB(t)
	inst := f.instance(t)

	// Act
	err := inst.WriteFile(context.Background(), "/top.txt", []byte("x"))

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if n := len(f.requests("POST /execd/directories")); n != 0 {
		t.Fatalf("mkdir requests = %d, want none for a root-level file", n)
	}
	if len(f.uploaded()) != 1 {
		t.Fatal("no upload")
	}
}

func TestWriteFileErrorsNameTheFailingStep(t *testing.T) {
	// Arrange
	f := newFakeOSB(t)
	inst := f.instance(t)
	ctx := context.Background()
	f.reply("POST /execd/directories", 500, `{"code":"BOOM","message":"read-only"}`)

	// Act
	mkdirFail := inst.WriteFile(ctx, "/ro/x.txt", []byte("x"))
	f.reply("POST /execd/directories", 200, `{}`)
	f.reply("POST /execd/files/upload", 507, `{"code":"FULL","message":"disk full"}`)
	uploadFail := inst.WriteFile(ctx, "/workspace/x.txt", []byte("x"))

	// Assert
	if mkdirFail == nil || !strings.Contains(mkdirFail.Error(), "create parent directory") {
		t.Errorf("mkdir failure: %v", mkdirFail)
	}
	if uploadFail == nil || !strings.Contains(uploadFail.Error(), "disk full") || strings.Contains(uploadFail.Error(), "create parent directory") {
		t.Errorf("upload failure: %v", uploadFail)
	}
}

func TestWriteFileFromStreamsTheReaderAndCreatesParents(t *testing.T) {
	// The stream implementation uploads from the reader without buffering the caller's side, and is discoverable through the interface.
	// Arrange
	f := newFakeOSB(t)
	inst := f.instance(t)
	sw, ok := inst.(sandbox.StreamWriter)
	if !ok {
		t.Fatal("the instance does not implement sandbox.StreamWriter")
	}
	payload := bytes.Repeat([]byte("0123456789"), 100_000)

	// Act
	err := sw.WriteFileFrom(context.Background(), "/workspace/in/big.bin", bytes.NewReader(payload))

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	ups := f.uploaded()
	if len(ups) != 1 || ups[0].Path != "/workspace/in/big.bin" || !bytes.Equal(ups[0].Data, payload) {
		t.Fatalf("upload mismatch: %d uploads", len(ups))
	}
	if n := len(f.requests("POST /execd/directories")); n != 1 {
		t.Fatalf("mkdir requests = %d", n)
	}
}

func TestWriteFileFromReportsParentAndReaderFailures(t *testing.T) {
	// Arrange
	f := newFakeOSB(t)
	inst := f.instance(t)
	sw := inst.(sandbox.StreamWriter)
	ctx := context.Background()
	boom := errors.New("source broke")

	// Act
	readerErr := sw.WriteFileFrom(ctx, "/workspace/x.bin", io.MultiReader(strings.NewReader("part"), errReader{boom}))
	f.reply("POST /execd/directories", 500, `{"code":"BOOM","message":"no"}`)
	parentErr := sw.WriteFileFrom(ctx, "/other/x.bin", strings.NewReader("x"))

	// Assert
	if !errors.Is(readerErr, boom) {
		t.Errorf("a failing source reader must surface its error: %v", readerErr)
	}
	if parentErr == nil || !strings.Contains(parentErr.Error(), "create parent directory") {
		t.Errorf("parent failure: %v", parentErr)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func TestMkdirAllSendsTheModeAndMapsErrors(t *testing.T) {
	// Arrange
	f := newFakeOSB(t)
	inst := f.instance(t)
	ctx := context.Background()

	// Act
	err := inst.MkdirAll(ctx, "/workspace/a/b")
	f.reply("POST /execd/directories", 500, `{"code":"BOOM","message":"no"}`)
	failed := inst.MkdirAll(ctx, "/workspace/c")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]struct{ Mode int }
	if err := json.Unmarshal(f.requests("POST /execd/directories")[0].Body, &body); err != nil || body["/workspace/a/b"].Mode != 755 {
		t.Fatalf("body = %s (%v)", f.requests("POST /execd/directories")[0].Body, err)
	}
	if failed == nil {
		t.Fatal("a failed mkdir was reported as success")
	}
}

func TestListSkipsTheDirectoryItselfAndMapsTypes(t *testing.T) {
	// Arrange
	f := newFakeOSB(t)
	f.putFile("/workspace/a.txt", []byte("abc"))
	f.putFile("/workspace/sub/b.txt", []byte("bb"))
	inst := f.instance(t)

	// Act
	shallow, err1 := inst.List(context.Background(), "/workspace/", 1)
	deep, err2 := inst.List(context.Background(), "/workspace", 2)

	// Assert
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	types := map[string]string{}
	for _, fi := range shallow {
		types[fi.Path] = fi.Type
	}
	if len(shallow) != 2 || types["/workspace/a.txt"] != "file" || types["/workspace/sub"] != "directory" {
		t.Fatalf("depth 1 = %+v (the directory itself must be excluded, even with a trailing slash)", shallow)
	}
	if len(deep) != 3 {
		t.Fatalf("depth 2 = %+v, want a.txt, sub and sub/b.txt", deep)
	}
	if q := f.requests("GET /execd/directories/list")[1].Query; q.Get("depth") != "2" {
		t.Fatalf("depth query = %q", q.Get("depth"))
	}
}

func TestListErrors(t *testing.T) {
	// Arrange
	f := newFakeOSB(t)
	inst := f.instance(t)
	ctx := context.Background()

	// Act
	_, missing := inst.List(ctx, "/workspace/none", 1)
	f.reply("GET /execd/directories/list", 200, `[{"path":`)
	_, malformed := inst.List(ctx, "/workspace", 1)
	f.reply("GET /execd/directories/list", 500, `{"code":"BOOM","message":"no"}`)
	_, broken := inst.List(ctx, "/workspace", 1)

	// Assert
	if !errors.Is(missing, sandbox.ErrNotFound) {
		t.Errorf("missing dir: %v", missing)
	}
	if malformed == nil {
		t.Error("malformed JSON accepted")
	}
	if broken == nil || errors.Is(broken, sandbox.ErrNotFound) {
		t.Errorf("500: %v", broken)
	}
}

func TestOperationsHonourContextCancellation(t *testing.T) {
	// A hung server must not hold a cancelled caller: every file operation returns promptly with the context error.
	// Arrange
	f := newFakeOSB(t)
	inst := f.instance(t)
	block := func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }
	for _, r := range []string{"GET /execd/files/info", "GET /execd/directories/list", "POST /execd/directories", "POST /execd/files/upload"} {
		f.handle(r, block)
	}
	ops := map[string]func(ctx context.Context) error{
		"stat":  func(ctx context.Context) error { _, err := inst.Stat(ctx, "/workspace/x"); return err },
		"read":  func(ctx context.Context) error { _, _, err := inst.ReadFile(ctx, "/workspace/x", 1); return err },
		"list":  func(ctx context.Context) error { _, err := inst.List(ctx, "/workspace", 1); return err },
		"mkdir": func(ctx context.Context) error { return inst.MkdirAll(ctx, "/workspace/x") },
		"write": func(ctx context.Context) error { return inst.WriteFile(ctx, "/workspace/x", []byte("x")) },
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()

			// Act
			err := op(ctx)

			// Assert
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err = %v, want a deadline error", err)
			}
		})
	}
}

func TestServerProxyModeSendsTheAPIKeyToTheProxyOnly(t *testing.T) {
	// Behind the server proxy the tenant key authenticates data-plane calls; in direct mode it must never reach the sandbox's execd.
	for _, c := range []struct {
		name  string
		proxy bool
		want  string
	}{
		{"server proxy", true, "key-1"},
		{"direct", false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			f := newFakeOSB(t)
			f.putFile("/workspace/a.txt", []byte("a"))
			o := f.opts()
			o.APIKey, o.UseServerProxy = "key-1", c.proxy
			b, err := opensandbox.NewOpenSandbox(o)
			if err != nil {
				t.Fatal(err)
			}
			inst, err := b.Connect(context.Background(), "sbx-1")
			if err != nil {
				t.Fatal(err)
			}

			// Act
			_, err = inst.Stat(context.Background(), "/workspace")

			// Assert
			if err != nil {
				t.Fatal(err)
			}
			if got := f.requests("GET /execd/files/info")[0].Header.Get("OPEN-SANDBOX-API-KEY"); got != c.want {
				t.Fatalf("execd saw API key %q, want %q", got, c.want)
			}
			if got := f.requests("GET /v1/sandboxes/sbx-1/endpoints/44772")[0].Header.Get("OPEN-SANDBOX-API-KEY"); got != "key-1" {
				t.Fatalf("lifecycle saw API key %q", got)
			}
		})
	}
}
