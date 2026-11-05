package documents

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/agent"
)

func TestRenameMovesAndNeverReplaces(t *testing.T) {
	svc := New(t.TempDir())
	if _, err := svc.Put("alice", "cv.pdf", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Put("alice", "old.pdf", []byte("b")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Rename("alice", "cv.pdf", "old.pdf"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("renaming onto another document: %v", err)
	}
	if _, err := svc.Rename("alice", "cv.pdf", "run.sh"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("renaming to a script: %v", err)
	}
	if _, err := svc.Rename("alice", "nope.pdf", "x.pdf"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("renaming a missing document: %v", err)
	}
	if _, err := svc.Rename("bob", "cv.pdf", "x.pdf"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("renaming another user's document: %v", err)
	}
	d, err := svc.Rename("alice", "cv.pdf", "resume.pdf")
	if err != nil || d.Name != "resume.pdf" {
		t.Fatalf("rename = %+v, %v", d, err)
	}
	if _, _, err := svc.Get("alice", "cv.pdf"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the old name is still there: %v", err)
	}
}

func TestFetchSavesWhatTheWebsiteServes(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/files/resume.pdf":
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write([]byte("%PDF-1.4 resume"))
		case "/cv":
			w.Header().Set("Content-Type", "application/pdf")
			w.Header().Set("Content-Disposition", `attachment; filename="Jane Doe CV.pdf"`)
			_, _ = w.Write([]byte("%PDF-1.4 cv"))
		case "/page":
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write([]byte("%PDF-1.4"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer site.Close()
	svc := New(t.TempDir())
	if _, _, err := svc.Fetch(context.Background(), site.URL+"/files/resume.pdf", ""); err == nil {
		t.Fatal("a service without a client must not download")
	}
	svc.HTTP = site.Client()
	for _, tc := range []struct{ path, name, want string }{
		{"/files/resume.pdf", "", "resume.pdf"},
		{"/cv", "", "Jane Doe CV.pdf"},
		{"/page", "", "page.pdf"}, // no extension: the media type gives one
		{"/files/resume.pdf", "mine.pdf", "mine.pdf"},
	} {
		data, name, err := svc.Fetch(context.Background(), site.URL+tc.path, tc.name)
		if err != nil || name != tc.want || !strings.HasPrefix(string(data), "%PDF") {
			t.Errorf("%s: %q %q %v, want %q", tc.path, name, data, err, tc.want)
		}
	}
	if _, _, err := svc.Fetch(context.Background(), site.URL+"/missing", ""); err == nil {
		t.Error("a 404 must fail")
	}
	for _, bad := range []string{"file:///etc/passwd", "ftp://x/y", "/relative", ""} {
		if _, _, err := svc.Fetch(context.Background(), bad, ""); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: %v, want ErrInvalid", bad, err)
		}
	}
}

func TestPublicClientRefusesPrivateAddresses(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("secret")) }))
	defer site.Close() // listens on 127.0.0.1
	svc := &Service{Dir: t.TempDir(), HTTP: PublicClient()}
	if _, _, err := svc.Fetch(context.Background(), site.URL+"/x.txt", ""); err == nil || !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("a loopback address was reachable: %v", err)
	}
	for _, a := range []string{"127.0.0.1", "10.0.0.5", "192.168.1.1", "169.254.169.254", "100.64.0.1", "::1", "fd00::1", "::ffff:10.0.0.1", "0.0.0.0"} {
		if publicAddr(netip.MustParseAddr(a)) {
			t.Errorf("%s counted as public", a)
		}
	}
	if !publicAddr(netip.MustParseAddr("93.184.216.34")) {
		t.Error("a public address was refused")
	}
}

func TestEmptyLibraryPromptTeachesSaving(t *testing.T) {
	svc := New(t.TempDir())
	got := svc.PromptSection(context.Background(), agent.Input{Run: &store.Run{UserLogin: "alice"}})
	if !strings.Contains(got, "documents_save_from_url") || !strings.Contains(got, "empty") {
		t.Fatalf("section = %q", got)
	}
}
