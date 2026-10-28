//go:build unix

package example_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/sandbox"
	"github.com/GhaziBenDahmane/harness/sandbox/example"
	"github.com/GhaziBenDahmane/harness/sandbox/local"
	"github.com/GhaziBenDahmane/harness/sandbox/sandboxtest"
)

// fakeService is a tiny in-process implementation of the REST service the
// example adapter speaks to (see the package comment). It borrows sandbox/local
// for the actual shell and files, so the conformance suite exercises the whole
// path: adapter -> HTTP -> service -> shell.
type fakeService struct {
	be *local.Backend
	mu sync.Mutex
	in map[string]sandbox.Instance
}

func newFakeService(t *testing.T) *httptest.Server {
	be, err := local.New(local.Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeService{be: be, in: map[string]sandbox.Instance{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sandboxes", f.create)
	mux.HandleFunc("GET /sandboxes/{id}", f.with(func(w http.ResponseWriter, r *http.Request, sb sandbox.Instance) {}))
	mux.HandleFunc("DELETE /sandboxes/{id}", f.kill)
	mux.HandleFunc("POST /sandboxes/{id}/renew", f.with(func(http.ResponseWriter, *http.Request, sandbox.Instance) {}))
	mux.HandleFunc("POST /sandboxes/{id}/exec", f.with(f.exec))
	mux.HandleFunc("GET /sandboxes/{id}/file", f.with(f.readFile))
	mux.HandleFunc("PUT /sandboxes/{id}/file", f.with(f.writeFile))
	mux.HandleFunc("POST /sandboxes/{id}/mkdir", f.with(func(w http.ResponseWriter, r *http.Request, sb sandbox.Instance) {
		f.check(w, sb.MkdirAll(r.Context(), r.URL.Query().Get("path")))
	}))
	mux.HandleFunc("GET /sandboxes/{id}/list", f.with(f.list))
	mux.HandleFunc("GET /sandboxes/{id}/stat", f.with(func(w http.ResponseWriter, r *http.Request, sb sandbox.Instance) {
		fi, err := sb.Stat(r.Context(), r.URL.Query().Get("path"))
		if f.check(w, err) {
			_ = json.NewEncoder(w).Encode(wire(fi))
		}
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func wire(fi sandbox.FileInfo) map[string]any {
	return map[string]any{"path": fi.Path, "type": fi.Type, "size": fi.Size, "mtime": fi.ModifiedAt}
}

func (f *fakeService) create(w http.ResponseWriter, r *http.Request) {
	sb, err := f.be.Create(r.Context(), sandbox.CreateOptions{})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	f.mu.Lock()
	f.in[sb.ID()] = sb
	f.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]string{"id": sb.ID()})
}

func (f *fakeService) kill(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f.mu.Lock()
	delete(f.in, id)
	f.mu.Unlock()
	_ = f.be.Kill(r.Context(), id)
}

// with resolves the sandbox of the path; an unknown one is a sandbox 404.
func (f *fakeService) with(h func(http.ResponseWriter, *http.Request, sandbox.Instance)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		sb := f.in[r.PathValue("id")]
		f.mu.Unlock()
		if sb == nil {
			w.Header().Set("X-Not-Found", "sandbox")
			http.Error(w, "no such sandbox", 404)
			return
		}
		h(w, r, sb)
	}
}

// check answers a file error: ErrNotFound is a plain 404.
func (f *fakeService) check(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, sandbox.ErrNotFound):
		http.Error(w, "not found", 404)
	default:
		http.Error(w, err.Error(), 500)
	}
	return false
}

func (f *fakeService) exec(w http.ResponseWriter, r *http.Request, sb sandbox.Instance) {
	var req struct {
		Command, Cwd string
		TimeoutMS    int64 `json:"timeout_ms"`
		Env          map[string]string
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	enc, fl := json.NewEncoder(w), w.(http.Flusher)
	var mu sync.Mutex
	send := func(v map[string]any) { mu.Lock(); _ = enc.Encode(v); fl.Flush(); mu.Unlock() }
	res, err := sb.Exec(r.Context(), sandbox.ExecRequest{Command: req.Command, Cwd: req.Cwd, Env: req.Env,
		Timeout:  time.Duration(req.TimeoutMS) * time.Millisecond,
		OnStdout: func(s string) { send(map[string]any{"o": s}) }, OnStderr: func(s string) { send(map[string]any{"e": s}) }})
	if err != nil {
		res.ExitCode = 124 // killed by the timeout
	}
	send(map[string]any{"exit": res.ExitCode})
}

func (f *fakeService) readFile(w http.ResponseWriter, r *http.Request, sb sandbox.Instance) {
	max, _ := strconv.ParseInt(r.URL.Query().Get("max"), 10, 64)
	data, size, err := sb.ReadFile(r.Context(), r.URL.Query().Get("path"), max)
	if f.check(w, err) {
		w.Header().Set("X-File-Size", strconv.FormatInt(size, 10))
		_, _ = w.Write(data)
	}
}

func (f *fakeService) writeFile(w http.ResponseWriter, r *http.Request, sb sandbox.Instance) {
	data, _ := io.ReadAll(r.Body)
	f.check(w, sb.WriteFile(r.Context(), r.URL.Query().Get("path"), data))
}

func (f *fakeService) list(w http.ResponseWriter, r *http.Request, sb sandbox.Instance) {
	depth, _ := strconv.Atoi(r.URL.Query().Get("depth"))
	fis, err := sb.List(r.Context(), r.URL.Query().Get("path"), depth)
	if !f.check(w, err) {
		return
	}
	out := make([]map[string]any, len(fis))
	for i, fi := range fis {
		out[i] = wire(fi)
	}
	_ = json.NewEncoder(w).Encode(out)
}

func TestExampleAdapterPassesTheConformanceSuite(t *testing.T) {
	srv := newFakeService(t)
	sandboxtest.RunConformanceWith(t, func(t *testing.T) sandbox.Backend {
		return &example.Backend{BaseURL: srv.URL}
	}, sandboxtest.ConformanceOptions{
		// The fake service is sandbox/local underneath, whose file API is host-confined too.
		Confined: true,
		Skip: map[string]string{
			"Files/PrepareWorkspaceBuildsTheLayout": "sandbox/local does not create /workspace/outputs",
		},
	})
}

func TestExampleAdapterMapsErrors(t *testing.T) {
	srv := newFakeService(t)
	b := &example.Backend{BaseURL: srv.URL}
	if _, err := b.Connect(t.Context(), "nope"); !errors.Is(err, sandbox.ErrNoSandbox) {
		t.Fatalf("Connect(unknown) = %v", err)
	}
	down := &example.Backend{BaseURL: "http://127.0.0.1:1"}
	if _, err := down.Connect(t.Context(), "x"); err == nil || errors.Is(err, sandbox.ErrNoSandbox) || strings.Contains(err.Error(), "SANDBOX_NOT_FOUND") {
		t.Fatalf("an unreachable service must be a transient error, not a gone sandbox: %v", err)
	}
}
