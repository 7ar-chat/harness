package opensandbox_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/7ar-chat/harness/adapters/sandbox/opensandbox"
	"github.com/7ar-chat/harness/core/sandbox"
)

const (
	runningSandboxJSON = `{"id":"sbx-1","status":{"state":"Running"},"entrypoint":[],"createdAt":"2026-10-06T00:00:00Z"}`
	notFoundBody       = `{"code":"DOCKER::SANDBOX_NOT_FOUND","message":"Sandbox sbx-1 not found."}`
	fileNotFoundBody   = `{"code":"FILE_NOT_FOUND","message":"no such file"}`
)

// recorded is one request the fake received.
type recorded struct {
	Method string
	Path   string
	Query  url.Values
	Header http.Header
	Body   []byte
}

// upload is one multipart file the fake accepted.
type upload struct {
	Path     string
	FileName string
	Mode     int
	Data     []byte
}

// fakeOSB emulates the OpenSandbox server for a single sandbox "sbx-1": the lifecycle API under /v1 and, under
// /execd, the proxied execd (commands over SSE, file and directory operations). A route can be replaced per test
// with handle; everything else behaves like a healthy server over an in-memory file tree.
type fakeOSB struct {
	srv *httptest.Server

	mu       sync.Mutex
	state    string // lifecycle state reported for the sandbox
	files    map[string][]byte
	dirs     map[string]bool
	reqs     []recorded
	routes   map[string]http.HandlerFunc // "METHOD /path" -> override
	uploads  []upload
	epHeader map[string]string // headers the endpoint lookup hands out
	ignoreRg bool              // answer ranged downloads with the whole file
}

func newFakeOSB(t *testing.T) *fakeOSB {
	t.Helper()
	f := &fakeOSB{state: "Running", files: map[string][]byte{}, dirs: map[string]bool{"/": true}, routes: map[string]http.HandlerFunc{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// handle replaces the answer of one route ("GET /execd/files/info").
func (f *fakeOSB) handle(route string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[route] = h
}

// reply makes a route answer a fixed status and JSON body.
func (f *fakeOSB) reply(route string, status int, body string) {
	f.handle(route, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
}

func (f *fakeOSB) setEndpointHeaders(h map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.epHeader = h
}

func (f *fakeOSB) setState(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = s
}

func (f *fakeOSB) putFile(p string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[p] = data
	for d := path.Dir(p); ; d = path.Dir(d) {
		f.dirs[d] = true
		if d == "/" || d == "." {
			break
		}
	}
}

// requests returns the recorded requests matching "METHOD /path".
func (f *fakeOSB) requests(route string) []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recorded
	for _, r := range f.reqs {
		if r.Method+" "+r.Path == route {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeOSB) uploaded() []upload {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]upload(nil), f.uploads...)
}

func (f *fakeOSB) host() string { return strings.TrimPrefix(f.srv.URL, "http://") }

func jsonReply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if s, ok := v.(string); ok {
		_, _ = w.Write([]byte(s))
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeOSB) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	route := r.Method + " " + r.URL.Path
	f.mu.Lock()
	f.reqs = append(f.reqs, recorded{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Header: r.Header.Clone(), Body: body})
	override := f.routes[route]
	state, epHeader := f.state, f.epHeader
	f.mu.Unlock()
	if override != nil {
		override(w, r)
		return
	}
	switch {
	case route == "POST /v1/sandboxes":
		jsonReply(w, 202, `{"id":"sbx-1","status":{"state":"Pending"},"entrypoint":[],"createdAt":"2026-10-06T00:00:00Z"}`)
	case route == "GET /v1/sandboxes/sbx-1":
		jsonReply(w, 200, `{"id":"sbx-1","status":{"state":"`+state+`"},"entrypoint":[],"createdAt":"2026-10-06T00:00:00Z"}`)
	case route == "DELETE /v1/sandboxes/sbx-1":
		w.WriteHeader(204)
	case route == "POST /v1/sandboxes/sbx-1/renew-expiration":
		jsonReply(w, 200, `{"expiresAt":"2030-01-01T00:00:00Z"}`)
	case strings.HasPrefix(r.URL.Path, "/v1/sandboxes/sbx-1/endpoints/"):
		jsonReply(w, 200, map[string]any{"endpoint": f.host() + "/execd", "headers": epHeader})
	case route == "GET /execd/ping":
		w.WriteHeader(200)
	case route == "POST /execd/command":
		writeSSE(w, `{"type":"execution_complete","exit_code":0}`)
	case route == "GET /execd/files/info":
		f.fileInfo(w, r)
	case route == "GET /execd/files/download":
		f.download(w, r)
	case route == "POST /execd/files/upload":
		f.upload(w, r)
	case route == "POST /execd/directories":
		f.mkdir(w, body)
	case route == "GET /execd/directories/list":
		f.list(w, r)
	default:
		jsonReply(w, 500, `{"code":"UNEXPECTED","message":"`+route+`"}`)
	}
}

// writeSSE streams execd events (raw JSON blobs separated by blank lines, like the real server).
func writeSSE(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	for _, e := range events {
		_, _ = io.WriteString(w, e+"\n\n")
	}
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}

func fileJSON(p string, data []byte, dir bool) map[string]any {
	typ := "file"
	if dir {
		typ = "directory"
	}
	return map[string]any{"path": p, "type": typ, "size": len(data), "modified_at": "2026-10-06T01:02:03Z"}
}

func (f *fakeOSB) fileInfo(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	f.mu.Lock()
	defer f.mu.Unlock()
	if data, ok := f.files[p]; ok {
		jsonReply(w, 200, map[string]any{p: fileJSON(p, data, false)})
		return
	}
	if f.dirs[p] {
		jsonReply(w, 200, map[string]any{p: fileJSON(p, nil, true)})
		return
	}
	jsonReply(w, 404, fileNotFoundBody)
}

func (f *fakeOSB) download(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	data, ok := f.files[r.URL.Query().Get("path")]
	ignore := f.ignoreRg
	f.mu.Unlock()
	if !ok {
		jsonReply(w, 404, fileNotFoundBody)
		return
	}
	if rg := r.Header.Get("Range"); rg != "" && !ignore {
		end, _ := strconv.Atoi(strings.TrimPrefix(rg, "bytes=0-"))
		if end+1 < len(data) {
			data = data[:end+1]
		}
		w.WriteHeader(206)
	}
	_, _ = w.Write(data)
}

func (f *fakeOSB) upload(w http.ResponseWriter, r *http.Request) {
	mr, err := r.MultipartReader()
	if err != nil {
		jsonReply(w, 400, `{"code":"BAD_UPLOAD","message":"not multipart"}`)
		return
	}
	var meta struct {
		Path string
		Mode int
	}
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		data, _ := io.ReadAll(part)
		switch part.FormName() {
		case "metadata":
			_ = json.Unmarshal(data, &meta)
		case "file":
			f.recordUpload(meta.Path, part, meta.Mode, data)
		}
	}
	w.WriteHeader(200)
}

func (f *fakeOSB) recordUpload(p string, part *multipart.Part, mode int, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads = append(f.uploads, upload{Path: p, FileName: part.FileName(), Mode: mode, Data: data})
	f.files[p] = data
	f.dirs[path.Dir(p)] = true
}

func (f *fakeOSB) mkdir(w http.ResponseWriter, body []byte) {
	var req map[string]struct{ Mode int }
	if err := json.Unmarshal(body, &req); err != nil {
		jsonReply(w, 400, `{"code":"BAD_REQUEST","message":"bad body"}`)
		return
	}
	f.mu.Lock()
	for p := range req {
		for d := p; ; d = path.Dir(d) {
			f.dirs[d] = true
			if d == "/" || d == "." {
				break
			}
		}
	}
	f.mu.Unlock()
	w.WriteHeader(200)
}

// list answers like execd: the directory itself first, then its descendants up to depth levels below it.
func (f *fakeOSB) list(w http.ResponseWriter, r *http.Request) {
	dir := path.Clean(r.URL.Query().Get("path"))
	depth, _ := strconv.Atoi(r.URL.Query().Get("depth"))
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.dirs[dir] {
		jsonReply(w, 404, fileNotFoundBody)
		return
	}
	out := []map[string]any{fileJSON(dir, nil, true)}
	var names []string
	for p := range f.files {
		names = append(names, p)
	}
	for p := range f.dirs {
		names = append(names, p)
	}
	sort.Strings(names)
	seen := map[string]bool{}
	for _, p := range names {
		if seen[p] || p == dir || !strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/") {
			continue
		}
		seen[p] = true
		if strings.Count(strings.TrimPrefix(p, strings.TrimSuffix(dir, "/")+"/"), "/")+1 > depth {
			continue
		}
		data, isFile := f.files[p]
		out = append(out, fileJSON(p, data, !isFile))
	}
	jsonReply(w, 200, out)
}

// opts returns options pointing at the fake with the fields the adapter needs to create a sandbox.
func (f *fakeOSB) opts() opensandbox.OpenSandboxOptions {
	return opensandbox.OpenSandboxOptions{URL: f.srv.URL, Image: "img:1", CPU: "1", Memory: "1Gi", TimeoutSeconds: 60}
}

func (f *fakeOSB) backend(t *testing.T) *opensandbox.OpenSandbox {
	t.Helper()
	b, err := opensandbox.NewOpenSandbox(f.opts())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// instance connects to the fake sandbox with default options.
func (f *fakeOSB) instance(t *testing.T) sandbox.Instance {
	t.Helper()
	inst, err := f.backend(t).Connect(context.Background(), "sbx-1")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return inst
}
