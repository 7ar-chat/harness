package product

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestDocumentLibraryIsPerUserAndRefusesWhatItShould(t *testing.T) {
	r := newRig(t, rigOpts{team: true, env: map[string]string{"HARNESS_DOCUMENTS_DIR": t.TempDir()}})
	put := func(user, name, body string) *http.Response {
		req, _ := http.NewRequest("PUT", r.srv.URL+"/api/me/documents/"+name, strings.NewReader(body))
		req.Header.Set("X-User", user)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if got := put("alice", "cv.pdf", "%PDF-1.4 alice").StatusCode; got != 200 {
		t.Fatalf("upload = %d", got)
	}
	if got := put("alice", "run.sh", "rm -rf /").StatusCode; got != 400 {
		t.Fatalf("a script = %d, want 400", got)
	}
	if got := put("alice", "empty.txt", "").StatusCode; got != 400 {
		t.Fatalf("an empty file = %d, want 400", got)
	}

	list := func(user string) []any {
		resp := r.do("GET", "/api/me/documents", user, "")
		defer resp.Body.Close()
		var out []any
		b, _ := io.ReadAll(resp.Body)
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("%s: %v", b, err)
		}
		return out
	}
	if n := len(list("alice")); n != 1 {
		t.Fatalf("alice has %d documents", n)
	}
	if n := len(list("bob")); n != 0 {
		t.Fatalf("bob sees %d of alice's documents", n)
	}

	resp := r.do("GET", "/api/me/documents/cv.pdf", "alice", "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "%PDF-1.4 alice" || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("download = %d %q %v", resp.StatusCode, body, resp.Header)
	}
	if got := r.do("GET", "/api/me/documents/cv.pdf", "bob", "").StatusCode; got != 404 {
		t.Fatalf("bob downloads alice's file: %d", got)
	}
	if got := r.do("DELETE", "/api/me/documents/cv.pdf", "alice", "").StatusCode; got != 204 {
		t.Fatalf("delete = %d", got)
	}
	if n := len(list("alice")); n != 0 {
		t.Fatalf("still %d documents", n)
	}
}

func TestDocumentLibraryIsAbsentWithoutADirectory(t *testing.T) {
	r := newRig(t, rigOpts{team: true})
	if got := r.do("GET", "/api/me/documents", "alice", "").StatusCode; got != 404 {
		t.Fatalf("status = %d, want 404", got)
	}
}
