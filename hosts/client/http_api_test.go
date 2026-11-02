package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/7ar-chat/harness/hosts/client"
)

// seen is what a recording server received.
type seen struct {
	Method, Path, Query, Auth, ContentType string
	Body                                   []byte
}

// recorder answers every request with status and body and remembers the last request.
type recorder struct {
	srv *httptest.Server
	mu  sync.Mutex
	req seen
}

func newRecorder(t *testing.T, status int, body string) *recorder {
	t.Helper()
	rec := &recorder{}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.req = seen{Method: r.Method, Path: r.URL.EscapedPath(), Query: r.URL.RawQuery, Auth: r.Header.Get("Authorization"),
			ContentType: r.Header.Get("Content-Type"), Body: b}
		rec.mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

func (r *recorder) last() seen {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.req
}

func (r *recorder) client(token string) client.Client { return client.HTTP(r.srv.URL, token) }

func jsonBody(t *testing.T, s seen) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(s.Body, &m); err != nil {
		t.Fatalf("request body %q is not a JSON object: %v", s.Body, err)
	}
	return m
}

func TestHTTPThreadRequestsUseTheDocumentedRoutesAndBodies(t *testing.T) {
	ctx := context.Background()
	thread := `{"id":"t 1","title":"T","pinned":true,"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-02T00:00:00Z"}`

	t.Run("create sends the title and decodes the thread", func(t *testing.T) {
		rec := newRecorder(t, 201, thread)

		got, err := rec.client("tok").CreateThread(ctx, "T")

		req := rec.last()
		if err != nil || got.ID != "t 1" || !got.Pinned || got.UpdatedAt.Day() != 2 {
			t.Fatalf("thread %+v, %v", got, err)
		}
		if req.Method != "POST" || req.Path != "/api/threads" || req.Auth != "Bearer tok" || req.ContentType != "application/json" || jsonBody(t, req)["title"] != "T" {
			t.Fatalf("request %+v", req)
		}
	})
	t.Run("list sends limit only when positive", func(t *testing.T) {
		rec := newRecorder(t, 200, `[`+thread+`]`)
		c := rec.client("")

		list, err := c.ListThreads(ctx, 7)
		withLimit := rec.last()
		_, err2 := c.ListThreads(ctx, 0)
		noLimit := rec.last()

		if err != nil || err2 != nil || len(list) != 1 || list[0].Title != "T" {
			t.Fatalf("list %+v, %v, %v", list, err, err2)
		}
		if withLimit.Query != "limit=7" || noLimit.Query != "" || withLimit.Method != "GET" || withLimit.Path != "/api/threads" {
			t.Fatalf("with limit %+v, without %+v", withLimit, noLimit)
		}
		if noLimit.Auth != "" || noLimit.ContentType != "" {
			t.Fatalf("a request without token or body must carry neither header: %+v", noLimit)
		}
	})
	t.Run("get escapes the id in the path", func(t *testing.T) {
		rec := newRecorder(t, 200, thread)

		_, err := rec.client("").GetThread(ctx, "a/b c")

		if err != nil || rec.last().Path != "/api/threads/a%2Fb%20c" {
			t.Fatalf("path %q, %v", rec.last().Path, err)
		}
	})
	t.Run("rename patches only the fields set", func(t *testing.T) {
		rec := newRecorder(t, 200, thread)
		pinned := true

		_, err := rec.client("").RenameThread(ctx, "t1", client.ThreadPatch{Pinned: &pinned})

		req := rec.last()
		body := jsonBody(t, req)
		if _, hasTitle := body["title"]; err != nil || req.Method != "PATCH" || req.Path != "/api/threads/t1" || hasTitle || body["pinned"] != true {
			t.Fatalf("request %+v (%v)", req, err)
		}
	})
	t.Run("delete accepts an empty 204 answer", func(t *testing.T) {
		rec := newRecorder(t, 204, "")

		err := rec.client("").DeleteThread(ctx, "t1")

		if req := rec.last(); err != nil || req.Method != "DELETE" || req.Path != "/api/threads/t1" || len(req.Body) != 0 {
			t.Fatalf("request %+v, %v", req, err)
		}
	})
}

func TestHTTPRunRequestsUseTheDocumentedRoutesAndBodies(t *testing.T) {
	ctx := context.Background()
	run := `{"id":"r1","threadId":"t1","status":"running","model":"m","lastSeq":4,"errorCode":"x","createdAt":"2026-01-01T00:00:00Z"}`

	t.Run("get run", func(t *testing.T) {
		rec := newRecorder(t, 200, run)

		got, err := rec.client("").GetRun(ctx, "r1")

		if err != nil || got.LastSeq != 4 || got.ErrorCode != "x" || rec.last().Path != "/api/runs/r1" || rec.last().Method != "GET" {
			t.Fatalf("run %+v, request %+v, %v", got, rec.last(), err)
		}
	})
	t.Run("steer posts the text and ignores the answer body", func(t *testing.T) {
		rec := newRecorder(t, 202, `{"status":"accepted"}`)

		err := rec.client("").Steer(ctx, "r1", "also tests")

		if req := rec.last(); err != nil || req.Method != "POST" || req.Path != "/api/runs/r1/steer" || jsonBody(t, req)["text"] != "also tests" {
			t.Fatalf("request %+v, %v", req, err)
		}
	})
	t.Run("cancel posts without a body", func(t *testing.T) {
		rec := newRecorder(t, 202, `{"status":"cancelling"}`)

		err := rec.client("").Cancel(ctx, "r1")

		if req := rec.last(); err != nil || req.Method != "POST" || req.Path != "/api/runs/r1/cancel" || len(req.Body) != 0 || req.ContentType != "" {
			t.Fatalf("request %+v, %v", req, err)
		}
	})
	t.Run("resume posts the interrupt answer to the thread", func(t *testing.T) {
		rec := newRecorder(t, 202, run)

		got, err := rec.client("").Resume(ctx, "t1", client.ResumeRequest{InterruptID: "i1", Decision: client.DecisionDeny, Always: true, Text: "no", Mode: client.ModePlan})

		req := rec.last()
		body := jsonBody(t, req)
		if err != nil || got.ID != "r1" || req.Path != "/api/threads/t1/resume" {
			t.Fatalf("run %+v, request %+v, %v", got, req, err)
		}
		for k, want := range map[string]any{"interruptId": "i1", "decision": "deny", "always": true, "text": "no", "mode": "plan"} {
			if body[k] != want {
				t.Errorf("body[%s] = %v, want %v", k, body[k], want)
			}
		}
		if _, ok := body["reason"]; ok {
			t.Errorf("an empty reason must be left out so the server infers it: %v", body)
		}
	})
	t.Run("send carries the attachments as base64", func(t *testing.T) {
		rec := newRecorder(t, 202, run)

		_, err := rec.client("").Send(ctx, "t1", "look", client.SendOptions{Mode: client.ModePlan,
			Attachments: []client.Attachment{{Name: "a.png", MIME: "image/png", Data: []byte("\x89PNG")}}})

		body := jsonBody(t, rec.last())
		atts, _ := body["attachments"].([]any)
		if err != nil || rec.last().Path != "/api/threads/t1/runs" || body["mode"] != "plan" || len(atts) != 1 {
			t.Fatalf("body %v, %v", body, err)
		}
		if a := atts[0].(map[string]any); a["name"] != "a.png" || a["mime"] != "image/png" || a["data"] != "iVBORw==" {
			t.Fatalf("attachment %v", a)
		}
	})
}

func TestHTTPFileRequests(t *testing.T) {
	ctx := context.Background()

	t.Run("list sends the path only when given", func(t *testing.T) {
		rec := newRecorder(t, 200, `[{"name":"a.txt","path":"/workspace/a.txt","type":"file","size":3,"modifiedAt":"2026-01-01T00:00:00Z"}]`)
		c := rec.client("")

		files, err := c.ListFiles(ctx, "t1", "sub dir")
		withPath := rec.last()
		_, err2 := c.ListFiles(ctx, "t1", "")
		root := rec.last()

		if err != nil || err2 != nil || len(files) != 1 || files[0].Size != 3 || files[0].Type != "file" {
			t.Fatalf("files %+v, %v, %v", files, err, err2)
		}
		if withPath.Path != "/api/threads/t1/files" || withPath.Query != "path=sub+dir" || root.Query != "" {
			t.Fatalf("with path %+v, root %+v", withPath, root)
		}
	})
	t.Run("download returns the raw bytes and the base name", func(t *testing.T) {
		rec := newRecorder(t, 200, "\x00binary\xffdata")

		d, err := rec.client("tok").DownloadFile(ctx, "t1", "/workspace/out/report.bin")

		req := rec.last()
		if err != nil || string(d.Data) != "\x00binary\xffdata" || d.Name != "report.bin" || d.Path != "/workspace/out/report.bin" {
			t.Fatalf("download %+v, %v", d, err)
		}
		if req.Path != "/api/threads/t1/files/download" || req.Query != "path=%2Fworkspace%2Fout%2Freport.bin" || req.Auth != "Bearer tok" {
			t.Fatalf("request %+v", req)
		}
	})
	t.Run("download reports the typed error of a refused file", func(t *testing.T) {
		rec := newRecorder(t, 413, `{"error":{"code":"too_large","message":"The file is too large to download."}}`)

		d, err := rec.client("").DownloadFile(ctx, "t1", "big.bin")

		if d != nil || !errors.Is(err, client.ErrTooLarge) || client.AsError(err).Status != 413 {
			t.Fatalf("download %+v, %v", d, err)
		}
	})
	t.Run("download fails when the body is cut short", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "100")
			_, _ = w.Write([]byte("partial"))
		}))
		defer srv.Close()

		d, err := client.HTTP(srv.URL, "").DownloadFile(ctx, "t1", "a.txt")

		if err == nil || d != nil {
			t.Fatalf("a truncated download must not look complete: %+v, %v", d, err)
		}
	})
}

func TestHTTPSessionsAndModels(t *testing.T) {
	ctx := context.Background()

	t.Run("sessions send the limit", func(t *testing.T) {
		rec := newRecorder(t, 200, `[{"id":"s1","title":"T","preview":"p","messages":2,"lastStatus":"completed","pinned":true,"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}]`)

		list, err := rec.client("").Sessions(ctx, 9)

		if err != nil || len(list) != 1 || list[0].Messages != 2 || list[0].LastStatus != "completed" || rec.last().Query != "limit=9" || rec.last().Path != "/api/sessions" {
			t.Fatalf("sessions %+v, request %+v, %v", list, rec.last(), err)
		}
	})
	t.Run("a store without sessions is ErrNotSupported", func(t *testing.T) {
		rec := newRecorder(t, 501, `{"error":{"code":"not_supported","message":"no"}}`)

		_, err := rec.client("").Sessions(ctx, 0)

		if !errors.Is(err, client.ErrNotSupported) || client.AsError(err).Status != 501 {
			t.Fatalf("error %v", err)
		}
	})
	t.Run("models carry the info table", func(t *testing.T) {
		rec := newRecorder(t, 200, `{"models":["a"],"default":"a","info":{"a":{"contextWindow":1000,"priced":true,"inputPerToken":0.5,"tier":{"above":10,"input":1}}}}`)

		m, err := rec.client("").Models(ctx)

		if err != nil || m.Info["a"].ContextWindow != 1000 || m.Info["a"].Tier == nil || m.Info["a"].Tier.Above != 10 {
			t.Fatalf("models %+v, %v", m, err)
		}
	})
}

func TestHTTPDecodesErrorAnswers(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name       string
		status     int
		body       string
		wantCode   string
		wantMsg    string
		wantTarget *client.Error
	}{
		{"envelope", 404, `{"error":{"code":"not_found","message":"No such thread."}}`, "not_found", "No such thread.", client.ErrNotFound},
		{"envelope with a server error", 500, `{"error":{"code":"internal","message":"boom"}}`, "internal", "boom", nil},
		{"plain text body", 502, "  bad gateway upstream\n", "http_502", "bad gateway upstream", nil},
		{"empty body falls back to the status text", 503, "", "http_503", "Service Unavailable", nil},
		{"json without an error code is not an envelope", 400, `{"error":{"message":"m"}}`, "http_400", `{"error":{"message":"m"}}`, nil},
		{"html error page", 404, "<html>nope</html>", "http_404", "<html>nope</html>", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := newRecorder(t, tc.status, tc.body)

			_, err := rec.client("").GetThread(ctx, "t1")

			e := client.AsError(err)
			if e == nil || e.Status != tc.status || e.Code != tc.wantCode || e.Message != tc.wantMsg {
				t.Fatalf("error %#v, want %d %s %q", err, tc.status, tc.wantCode, tc.wantMsg)
			}
			if tc.wantTarget != nil && !errors.Is(err, tc.wantTarget) {
				t.Fatalf("errors.Is(%v, %s) = false", err, tc.wantTarget.Code)
			}
		})
	}
}

func TestHTTPErrorBodyIsReadUpToALimit(t *testing.T) {
	rec := newRecorder(t, 500, string(make([]byte, 200_000)))

	_, err := rec.client("").Models(context.Background())

	if e := client.AsError(err); e == nil || len(e.Message) > 1<<16 {
		t.Fatalf("error message of %d bytes", len(client.AsError(err).Message))
	}
}

func TestHTTPTransportAndDecodeFailuresAreNotAPIErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("an unreachable server", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()

		_, err := client.HTTP(url, "").Models(ctx)

		if err == nil || client.AsError(err) != nil {
			t.Fatalf("error %v: a connection failure is not an API answer", err)
		}
	})
	t.Run("an invalid base url", func(t *testing.T) {
		_, err := client.HTTP("http://bad\x7fhost", "").Models(ctx)

		if err == nil || client.AsError(err) != nil {
			t.Fatalf("error %v", err)
		}
	})
	t.Run("a 2xx answer that is not json", func(t *testing.T) {
		rec := newRecorder(t, 200, "<html>login</html>")

		_, err := rec.client("").GetRun(ctx, "r1")

		if err == nil || client.AsError(err) != nil {
			t.Fatalf("error %v: a garbled success body must surface as an error", err)
		}
	})
	t.Run("a cancelled context", func(t *testing.T) {
		rec := newRecorder(t, 200, `{}`)
		cctx, cancel := context.WithCancel(ctx)
		cancel()

		_, err := rec.client("").Models(cctx)

		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error %v", err)
		}
	})
}

type countingTransport struct {
	mu sync.Mutex
	n  int
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return http.DefaultTransport.RoundTrip(r)
}

func TestHTTPUsesTheConfiguredHTTPClient(t *testing.T) {
	rec := newRecorder(t, 200, `{"models":[],"default":""}`)
	tr := &countingTransport{}

	_, err := client.HTTP(rec.srv.URL, "", client.WithHTTPClient(&http.Client{Transport: tr})).Models(context.Background())

	tr.mu.Lock()
	defer tr.mu.Unlock()
	if err != nil || tr.n != 1 {
		t.Fatalf("requests through the custom client: %d, %v", tr.n, err)
	}
}

func TestHTTPExtras(t *testing.T) {
	ctx := context.Background()
	ex := func(t *testing.T, rec *recorder) client.Extras {
		t.Helper()
		e, ok := rec.client("tok").(client.Extras)
		if !ok {
			t.Fatal("the HTTP client must implement client.Extras")
		}
		return e
	}

	t.Run("history operations post to the thread and decode the result", func(t *testing.T) {
		rec := newRecorder(t, 200, `{"prompt":"p","files":["a.go"],"canUndo":true,"canRedo":false}`)
		e := ex(t, rec)

		undo, err1 := e.Undo(ctx, "t 1")
		undoReq := rec.last()
		redo, err2 := e.Redo(ctx, "t1")
		redoReq := rec.last()

		if err1 != nil || err2 != nil || undo.Prompt != "p" || len(undo.Files) != 1 || !undo.CanUndo || redo.CanRedo {
			t.Fatalf("undo %+v %v, redo %+v %v", undo, err1, redo, err2)
		}
		if undoReq.Method != "POST" || undoReq.Path != "/api/threads/t%201/undo" || redoReq.Path != "/api/threads/t1/redo" || undoReq.Auth != "Bearer tok" {
			t.Fatalf("requests %+v %+v", undoReq, redoReq)
		}
	})
	t.Run("compact", func(t *testing.T) {
		rec := newRecorder(t, 200, `{"compacted":true,"tokensBefore":900,"tokensAfter":100}`)

		res, err := ex(t, rec).Compact(ctx, "t1")

		if err != nil || !res.Compacted || res.TokensBefore != 900 || res.TokensAfter != 100 || rec.last().Path != "/api/threads/t1/compact" {
			t.Fatalf("result %+v, request %+v, %v", res, rec.last(), err)
		}
	})
	t.Run("fork sends the message id", func(t *testing.T) {
		rec := newRecorder(t, 201, `{"id":"t2","title":"Fork","createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`)

		th, err := ex(t, rec).Fork(ctx, "t1", "m3")

		if req := rec.last(); err != nil || th.ID != "t2" || req.Path != "/api/threads/t1/fork" || jsonBody(t, req)["messageId"] != "m3" {
			t.Fatalf("thread %+v, request %+v, %v", th, req, err)
		}
	})
	t.Run("a server without the history routes is ErrNotSupported", func(t *testing.T) {
		rec := newRecorder(t, 404, "404 page not found\n")
		e := ex(t, rec)

		_, c1 := e.Compact(ctx, "t1")
		_, c2 := e.Undo(ctx, "t1")
		_, c3 := e.Redo(ctx, "t1")
		_, c4 := e.Fork(ctx, "t1", "")

		for i, err := range []error{c1, c2, c3, c4} {
			if !errors.Is(err, client.ErrNotSupported) {
				t.Errorf("operation %d: %v", i, err)
			}
		}
	})
	t.Run("an unknown thread stays not found", func(t *testing.T) {
		rec := newRecorder(t, 404, `{"error":{"code":"not_found","message":"Not found."}}`)

		_, err := ex(t, rec).Undo(ctx, "nope")

		if !errors.Is(err, client.ErrNotFound) || errors.Is(err, client.ErrNotSupported) {
			t.Fatalf("a 404 inside the error envelope is a missing thread, not a missing route: %v", err)
		}
	})
	t.Run("a refused history step keeps its conflict code", func(t *testing.T) {
		rec := newRecorder(t, 409, `{"error":{"code":"conflict","message":"Nothing to undo."}}`)

		_, err := ex(t, rec).Undo(ctx, "t1")

		if !errors.Is(err, client.ErrConflict) || client.AsError(err).Message != "Nothing to undo." {
			t.Fatalf("error %v", err)
		}
	})
	t.Run("commands", func(t *testing.T) {
		rec := newRecorder(t, 200, `[{"name":"review","description":"d","source":"project","argHint":"<file>"}]`)

		list, err := ex(t, rec).Commands(ctx)

		if err != nil || len(list) != 1 || list[0].Name != "review" || list[0].ArgHint != "<file>" || rec.last().Path != "/api/commands" || rec.last().Method != "GET" {
			t.Fatalf("commands %+v, request %+v, %v", list, rec.last(), err)
		}
	})
	t.Run("commands on an older server are ErrNotSupported, other failures are kept", func(t *testing.T) {
		missing := newRecorder(t, 404, "404 page not found")
		broken := newRecorder(t, 500, `{"error":{"code":"internal","message":"boom"}}`)

		_, err1 := ex(t, missing).Commands(ctx)
		_, err2 := ex(t, broken).Commands(ctx)

		if !errors.Is(err1, client.ErrNotSupported) {
			t.Errorf("missing route: %v", err1)
		}
		if e := client.AsError(err2); e == nil || e.Code != "internal" {
			t.Errorf("server error: %v", err2)
		}
	})
	t.Run("expand escapes the name and sends the args", func(t *testing.T) {
		rec := newRecorder(t, 200, `{"prompt":"review main.go"}`)

		prompt, err := ex(t, rec).ExpandCommand(ctx, "re/view", "main.go")

		if req := rec.last(); err != nil || prompt != "review main.go" || req.Path != "/api/commands/re%2Fview/expand" || jsonBody(t, req)["args"] != "main.go" {
			t.Fatalf("prompt %q, request %+v, %v", prompt, req, err)
		}
	})
	t.Run("expanding an unknown command is not found", func(t *testing.T) {
		rec := newRecorder(t, 404, `{"error":{"code":"not_found","message":"no command named \"x\"."}}`)

		prompt, err := ex(t, rec).ExpandCommand(ctx, "x", "")

		if prompt != "" || !errors.Is(err, client.ErrNotFound) {
			t.Fatalf("prompt %q, error %v", prompt, err)
		}
	})
}
