package documents_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/features/documents"
)

func TestPutListGetDeleteAreScopedToTheUser(t *testing.T) {
	s := documents.New(t.TempDir())
	if _, err := s.Put("ana", "cv.pdf", []byte("%PDF-1.4 ana")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put("bob", "cv.pdf", []byte("%PDF-1.4 bob")); err != nil {
		t.Fatal(err)
	}
	docs, _ := s.List("ana")
	if len(docs) != 1 || docs[0].Name != "cv.pdf" || docs[0].MimeType != "application/pdf" {
		t.Fatalf("ana's documents = %+v", docs)
	}
	data, _, err := s.Get("bob", "cv.pdf")
	if err != nil || !strings.Contains(string(data), "bob") {
		t.Fatalf("bob got %q, %v", data, err)
	}
	if err := s.Delete("ana", "cv.pdf"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get("ana", "cv.pdf"); !errors.Is(err, documents.ErrNotFound) {
		t.Fatalf("deleted document still there: %v", err)
	}
	if docs, _ := s.List("bob"); len(docs) != 1 {
		t.Fatal("deleting ana's document touched bob's")
	}
}

func TestNamesCannotEscapeTheUserDirectoryOrSmuggleCode(t *testing.T) {
	s := documents.New(t.TempDir())
	for _, bad := range []string{"run.sh", "x.exe", "noext", "..", "", "a.html", "x.svg"} {
		if _, err := s.Put("ana", bad, []byte("x")); !errors.Is(err, documents.ErrInvalid) {
			t.Errorf("Put(%q) = %v, want ErrInvalid", bad, err)
		}
	}
	d, err := s.Put("ana", "../../etc/passwd.txt", []byte("x"))
	if err != nil || d.Name != "passwd.txt" {
		t.Fatalf("traversal = %+v, %v", d, err)
	}
	if _, err := s.Put("ana", "empty.txt", nil); !errors.Is(err, documents.ErrInvalid) {
		t.Errorf("empty file = %v", err)
	}
	if _, err := s.Put("", "a.txt", []byte("x")); !errors.Is(err, documents.ErrInvalid) {
		t.Errorf("no user = %v", err)
	}
}

func TestLimits(t *testing.T) {
	s := documents.New(t.TempDir())
	if _, err := s.Put("ana", "big.pdf", make([]byte, documents.MaxFileBytes+1)); !errors.Is(err, documents.ErrLimit) {
		t.Fatalf("oversized = %v", err)
	}
	for i := 0; i < documents.MaxFiles; i++ {
		if _, err := s.Put("ana", "f"+string(rune('a'+i%26))+string(rune('a'+i/26))+".txt", []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Put("ana", "one-more.txt", []byte("x")); !errors.Is(err, documents.ErrLimit) {
		t.Fatalf("51st document = %v", err)
	}
	if _, err := s.Put("ana", "faa.txt", []byte("replaced")); err != nil {
		t.Fatalf("replacing at the limit must work: %v", err)
	}
}

func TestPromptSectionAndBrowserPath(t *testing.T) {
	s := documents.New(t.TempDir())
	s.BrowserRoot = "/docs"
	in := agent.Input{Run: &store.Run{UserLogin: "ana"}}
	if got := s.PromptSection(context.Background(), in); !strings.Contains(got, "empty") || strings.Contains(got, "cv.pdf") {
		t.Fatalf("no documents, section = %q", got)
	}
	if _, err := s.Put("ana", "cv.pdf", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if got := s.PromptSection(context.Background(), in); !strings.Contains(got, "cv.pdf") {
		t.Fatalf("section = %q", got)
	}
	want := "/docs/" + documents.Tag("ana") + "/cv.pdf"
	if got := s.BrowserPath("ana", "cv.pdf"); got != want {
		t.Fatalf("browser path = %q, want %q", got, want)
	}
	if strings.Contains(want, "ana") {
		t.Fatal("the login must not appear in the path")
	}
}

func TestCopyToPutsTheFilesInTheWorkspace(t *testing.T) {
	s := documents.New(t.TempDir())
	_, _ = s.Put("ana", "cv.pdf", []byte("pdf"))
	_, _ = s.Put("ana", "letter.txt", []byte("hi"))
	sb := sandboxtest.NewHostSandbox("t", t.TempDir())
	names, err := s.CopyTo(context.Background(), "ana", sb, "")
	if err != nil || len(names) != 2 {
		t.Fatalf("copied %v, %v", names, err)
	}
	got, _, err := sb.ReadFile(context.Background(), documents.WorkspaceDir+"/letter.txt", 1<<20)
	if err != nil || string(got) != "hi" {
		t.Fatalf("read back %q, %v", got, err)
	}
	if _, err := s.CopyTo(context.Background(), "ana", sb, "nope.pdf"); !errors.Is(err, documents.ErrNotFound) {
		t.Fatalf("unknown document = %v", err)
	}
}
