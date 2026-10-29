package opensandbox_test

import (
	"context"
	"errors"
	"github.com/GhaziBenDahmane/harness/adapters/sandbox/opensandbox"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GhaziBenDahmane/harness/core/sandbox"
)

// fakeOpenSandbox emulates the OpenSandbox server (lifecycle API + proxied
// execd) for one sandbox; once removed, every call answers 404 SANDBOX_NOT_FOUND.
func fakeOpenSandbox(t *testing.T, removed *atomic.Bool) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		gone := func() {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"code":"DOCKER::SANDBOX_NOT_FOUND","message":"Sandbox sbx-1 not found."}`))
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/sandboxes/sbx-1/endpoints/"):
			if removed.Load() {
				gone()
				return
			}
			_, _ = w.Write([]byte(`{"endpoint":"` + strings.TrimPrefix(srv.URL, "http://") + `/execd"}`))
		case r.URL.Path == "/v1/sandboxes/sbx-1":
			if removed.Load() {
				gone()
				return
			}
			_, _ = w.Write([]byte(`{"id":"sbx-1","status":{"state":"Running"},"entrypoint":[],"createdAt":"2026-10-06T00:00:00Z"}`))
		case strings.HasPrefix(r.URL.Path, "/execd/"):
			if removed.Load() {
				gone()
				return
			}
			w.WriteHeader(404) // a missing file in a live sandbox
			_, _ = w.Write([]byte(`{"code":"FILE_NOT_FOUND","message":"no such file"}`))
		default:
			w.WriteHeader(500)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOpenSandboxTellsARemovedSandboxFromAMissingFile(t *testing.T) {
	// A 404 for a file of a live sandbox is "no such file"; once the sandbox is removed, every operation reports it gone (never as a missing file).
	// Arrange
	var removed atomic.Bool
	srv := fakeOpenSandbox(t, &removed)
	b, err := opensandbox.NewOpenSandbox(opensandbox.OpenSandboxOptions{URL: srv.URL, UseServerProxy: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	inst, err := b.Connect(ctx, "sbx-1")
	if err != nil {
		t.Fatal(err)
	}
	_, liveErr := inst.Stat(ctx, "/workspace/missing.txt")

	// Act
	removed.Store(true)
	_, statErr := inst.Stat(ctx, "/workspace/notes.md")
	_, _, readErr := inst.ReadFile(ctx, "/workspace/notes.md", 100)
	_, listErr := inst.List(ctx, "/workspace", 1)
	_, execErr := inst.Exec(ctx, sandbox.ExecRequest{Command: "ls", Cwd: "/workspace"})
	writeErr := inst.WriteFile(ctx, "/workspace/out.txt", []byte("x"))

	// Assert
	if !errors.Is(liveErr, sandbox.ErrNotFound) || sandbox.IsGone(liveErr) {
		t.Fatalf("live sandbox, missing file: %v", liveErr)
	}
	for name, err := range map[string]error{"stat": statErr, "read": readErr, "list": listErr, "exec": execErr, "write": writeErr} {
		if !sandbox.IsGone(err) || errors.Is(err, sandbox.ErrNotFound) {
			t.Fatalf("%s on a removed sandbox: %v", name, err)
		}
	}
}
