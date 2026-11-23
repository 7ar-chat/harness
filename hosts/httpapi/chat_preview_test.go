package httpapi_test

import (
	"strings"
	"testing"
)

func TestFilePreviewIsServedInlineAsAnOpaqueOrigin(t *testing.T) {
	r := newRig(t, rigOpts{})
	id := r.newThread("alice")
	sb, err := r.h.Sandboxes.Acquire(r.ctx, id, "alice")
	if err != nil {
		t.Fatal(err)
	}
	_ = sb.WriteFile(r.ctx, "/workspace/page.html", []byte("<style>b{}</style><b>hi</b>"))
	_ = sb.WriteFile(r.ctx, "/workspace/notes.txt", []byte("text"))

	resp := r.do("GET", "/api/threads/"+id+"/files/preview?path=page.html", "alice", "")
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("html preview: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") || !strings.Contains(csp, "connect-src 'none'") {
		t.Fatalf("csp = %q", csp)
	}
	if resp.Header.Get("X-Frame-Options") != "SAMEORIGIN" {
		t.Fatalf("x-frame-options = %q", resp.Header.Get("X-Frame-Options"))
	}
	if resp := r.do("GET", "/api/threads/"+id+"/files/preview?path=notes.txt", "alice", ""); resp.StatusCode != 400 {
		t.Fatalf("unpreviewable type: status %d, want 400", resp.StatusCode)
	}
	if resp := r.do("GET", "/api/threads/"+id+"/files/preview?path=page.html", "mallory", ""); resp.StatusCode != 404 {
		t.Fatalf("another user: status %d, want 404", resp.StatusCode)
	}
}
