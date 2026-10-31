package artifacts_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/features/artifacts"
)

var (
	pngBytes  = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{1}, 32)...)
	jpegBytes = append([]byte{0xff, 0xd8, 0xff, 0xe0}, bytes.Repeat([]byte{2}, 32)...)
)

func TestUploadRefusesContentThatDisagreesWithItsNameOrDeclaredType(t *testing.T) {
	// Extension, declared MIME type and signature must agree for known types: mismatches are ErrTypeMismatch and nothing is stored.
	webp := append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), bytes.Repeat([]byte{3}, 16)...)
	pdf := testdata(t, "memo.pdf")
	cases := []struct {
		name     string
		filename string
		declared string
		data     []byte
		ok       bool
	}{
		{"png matches", "a.png", "image/png", pngBytes, true},
		{"png without declared type", "a.png", "", pngBytes, true},
		{"png with generic type", "a.png", "application/octet-stream", pngBytes, true},
		{"jpg alias type", "a.jpg", "image/jpg", jpegBytes, true},
		{"jpeg with parameters", "a.jpeg", "IMAGE/JPEG; charset=binary", jpegBytes, true},
		{"gif87", "a.gif", "image/gif", []byte("GIF87a....."), true},
		{"gif89", "a.gif", "image/gif", []byte("GIF89a....."), true},
		{"webp", "a.webp", "image/webp", webp, true},
		{"zip declared with the Windows type", "a.zip", "application/x-zip-compressed", zipBytes(t, map[string]string{"a": "b"}), true},
		{"png extension on jpeg bytes", "a.png", "image/png", jpegBytes, false},
		{"png declared as jpeg", "a.png", "image/jpeg", pngBytes, false},
		{"txt declared as png", "a.txt", "image/png", []byte("text"), false},
		{"txt on png bytes is fine undeclared", "a.txt", "", pngBytes, true},
		{"webp extension on RIFF without WEBP", "a.webp", "", []byte("RIFF\x00\x00\x00\x00WAVEfmt "), false},
		{"webp extension on short data", "a.webp", "", []byte("RIFF"), false},
		{"pdf name on text", "a.pdf", "application/pdf", []byte("plain"), false},
		{"pdf bytes with a text name", "a.txt", "text/plain", pdf, false},
		{"pdf name with a docx type", "a.pdf", mimeDOCX, pdf, false},
		{"pdf matches", "a.pdf", "application/pdf", pdf, true},
		{"docx matches", "a.docx", mimeDOCX, testdata(t, "report.docx"), true},
		{"docx name on pptx bytes", "a.docx", mimeDOCX, testdata(t, "deck.pptx"), false},
		{"xlsx name on zip without content types", "a.xlsx", "", zipBytes(t, map[string]string{"a": "b"}), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			r := newSBRig(t, artifacts.Options{})

			// Act
			a, err := r.svc.Upload(r.ctx, artifacts.UploadInput{ThreadID: r.th.ID, Login: "alice", Filename: c.filename, DeclaredMIME: c.declared, Data: c.data})

			// Assert
			if c.ok && (err != nil || a == nil) {
				t.Fatalf("upload refused: %v", err)
			}
			if !c.ok && (!errors.Is(err, artifacts.ErrTypeMismatch) || len(r.all()) != 0) {
				t.Fatalf("err %v artifacts %d, want ErrTypeMismatch and nothing stored", err, len(r.all()))
			}
		})
	}
}

func TestUploadStoresAtTheTargetPathAndNumbersCollisions(t *testing.T) {
	// A target path is honoured; a taken path (artifact, or a file the agent wrote and nobody snapshotted yet) gets "name (2).ext", "(3)"...
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	sb := r.sandbox()
	sb.Put("/workspace/outputs/report.txt", "written by the agent")

	// Act
	up := func(target, filename string) result {
		a, err := r.svc.Upload(r.ctx, artifacts.UploadInput{ThreadID: r.th.ID, Login: "alice", Filename: filename, TargetPath: target, Data: []byte("data " + target)})
		if err != nil {
			return result{err: err}
		}
		return result{path: a.Path}
	}
	first := up("outputs/report.txt", "ignored.txt")
	second := up("outputs/report.txt", "ignored.txt")
	rootA := up("", "notes.md")
	rootB := up("", "notes.md")
	rootC := up("", "notes.md")
	bad := up("../escape.txt", "x.txt")

	// Assert
	want := []struct {
		got  result
		path string
	}{
		{first, "/workspace/outputs/report (2).txt"}, // the agent's own file already holds report.txt
		{second, "/workspace/outputs/report (3).txt"},
		{rootA, "/workspace/notes.md"},
		{rootB, "/workspace/notes (2).md"},
		{rootC, "/workspace/notes (3).md"},
	}
	for _, w := range want {
		if w.got.err != nil || w.got.path != w.path {
			t.Errorf("path %q err %v, want %q", w.got.path, w.got.err, w.path)
		}
	}
	if !errors.Is(bad.err, artifacts.ErrInvalidTargetPath) {
		t.Errorf("escaping target: %v", bad.err)
	}
	if got, ok := sb.File("/workspace/outputs/report (2).txt"); !ok || got != "data outputs/report.txt" {
		t.Errorf("upload not written into the sandbox: %q %v", got, ok)
	}
	if got, _ := sb.File("/workspace/outputs/report.txt"); got != "written by the agent" {
		t.Errorf("the agent's file was overwritten: %q", got)
	}
}

type result struct {
	path string
	err  error
}

func TestUploadRefusesContentAboveTheBackendCap(t *testing.T) {
	// Data above the blob backend's cap is ErrTooLarge before anything is stored.
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	svc := artifacts.New(r.mem, capBlobs{hookBlobs: newHookBlobs(), max: 10}, nil, artifacts.Options{})

	// Act
	_, err := svc.Upload(r.ctx, artifacts.UploadInput{ThreadID: r.th.ID, Login: "alice", Filename: "big.txt", Data: []byte(strings.Repeat("x", 11))})
	_, okErr := svc.Upload(r.ctx, artifacts.UploadInput{ThreadID: r.th.ID, Login: "alice", Filename: "ok.txt", Data: []byte(strings.Repeat("x", 10))})

	// Assert
	if !errors.Is(err, artifacts.ErrTooLarge) || okErr != nil || len(r.all()) != 1 {
		t.Fatalf("over cap: %v; at cap: %v; artifacts %d", err, okErr, len(r.all()))
	}
}

// failingReader is a ReaderAt that always fails.
type failingReader struct{ err error }

func (f failingReader) ReadAt([]byte, int64) (int, error) { return 0, f.err }

func TestUploadFromABodyReportsReadErrorsAndStreamsLargeFiles(t *testing.T) {
	// A Body that fails to read fails the upload without storing anything; a plain Body is hashed while streamed and a zip Body is read whole to check it.
	t.Run("read error while sniffing", func(t *testing.T) {
		// Arrange
		r := newSBRig(t, artifacts.Options{})
		boom := errors.New("disk gone")

		// Act
		_, err := r.svc.Upload(r.ctx, artifacts.UploadInput{ThreadID: r.th.ID, Login: "alice", Filename: "a.txt", Body: failingReader{boom}, Size: 10})

		// Assert
		if !errors.Is(err, boom) || len(r.all()) != 0 {
			t.Fatalf("err %v artifacts %d", err, len(r.all()))
		}
	})
	t.Run("a prefixed zip is read whole to be checked", func(t *testing.T) {
		// Arrange: a zip whose central directory sits in the tail of a file that does not start with PK
		r := newSBRig(t, artifacts.Options{})
		z := zipBytes(t, map[string]string{"a.txt": "x"})
		prefixed := append([]byte("junk-prefix"), z...)

		// Act
		a, err := r.svc.Upload(r.ctx, artifacts.UploadInput{ThreadID: r.th.ID, Login: "alice", Filename: "bundle.dat", Body: bytes.NewReader(prefixed), Size: int64(len(prefixed))})

		// Assert
		if err != nil || a.SHA256 != sum(prefixed) {
			t.Fatalf("upload %v", err)
		}
	})
	t.Run("a zip body unreadable in full fails", func(t *testing.T) {
		// Arrange
		r := newSBRig(t, artifacts.Options{})
		z := zipBytes(t, map[string]string{"a.txt": "x"})
		boom := errors.New("short read")

		// Act: reads for the head and tail succeed, the full read fails
		_, err := r.svc.Upload(r.ctx, artifacts.UploadInput{ThreadID: r.th.ID, Login: "alice", Filename: "a.zip", Body: &failSecondFull{data: z, err: boom}, Size: int64(len(z))})

		// Assert
		if !errors.Is(err, boom) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("an empty upload is stored", func(t *testing.T) {
		// Arrange
		r := newSBRig(t, artifacts.Options{})

		// Act
		a, err := r.svc.Upload(r.ctx, artifacts.UploadInput{ThreadID: r.th.ID, Login: "alice", Filename: "empty.txt"})

		// Assert
		if err != nil || a.SizeBytes != 0 || a.SHA256 != sum(nil) {
			t.Fatalf("upload %+v %v", a, err)
		}
	})
}

// failSecondFull serves the sniff reads, then fails the whole-content read.
type failSecondFull struct {
	data  []byte
	err   error
	reads int
}

func (f *failSecondFull) ReadAt(p []byte, off int64) (int, error) {
	f.reads++
	if f.reads > 2 {
		return 0, f.err
	}
	n := copy(p, f.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func TestUploadOfAStreamedFileReusesStoredContentAndFailsCleanly(t *testing.T) {
	// A streamed upload whose content is already stored is not sent again; backend failures surface and a backend cap on a stream is ErrTooLarge.
	t.Run("already stored content is not streamed again", func(t *testing.T) {
		// Arrange
		st := newSBRig(t, artifacts.Options{})
		blobs := newStreamBlobs()
		svc := artifacts.New(st.mem, blobs, nil, artifacts.Options{})
		text := bytes.Repeat([]byte("same\n"), 1000)
		in := artifacts.UploadInput{ThreadID: st.th.ID, Login: "alice", Filename: "a.txt", Body: bytes.NewReader(text), Size: int64(len(text))}

		// Act
		_, err1 := svc.Upload(st.ctx, in)
		in.Filename = "b.txt"
		_, err2 := svc.Upload(st.ctx, in)

		// Assert
		if err1 != nil || err2 != nil || len(blobs.streamed) != 1 {
			t.Fatalf("errors %v %v streamed %d", err1, err2, len(blobs.streamed))
		}
	})
	t.Run("queue failure aborts before storing", func(t *testing.T) {
		// Arrange
		r := newSBRig(t, artifacts.Options{})
		boom := errors.New("queue down")
		fs := &faultStore{Store: r.mem, queueErr: boom}
		blobs := newStreamBlobs()
		svc := artifacts.New(fs, blobs, nil, artifacts.Options{})
		text := []byte("hello streamed")

		// Act
		_, err := svc.Upload(r.ctx, artifacts.UploadInput{ThreadID: r.th.ID, Login: "alice", Filename: "a.txt", Body: bytes.NewReader(text), Size: int64(len(text))})

		// Assert
		if !errors.Is(err, boom) || len(blobs.streamed) != 0 {
			t.Fatalf("err %v streamed %v", err, blobs.streamed)
		}
	})
	t.Run("touch failure aborts", func(t *testing.T) {
		// Arrange
		r := newSBRig(t, artifacts.Options{})
		boom := errors.New("touch down")
		svc := artifacts.New(&faultStore{Store: r.mem, touchErr: boom}, newStreamBlobs(), nil, artifacts.Options{})
		text := []byte("hello streamed")

		// Act
		_, err := svc.Upload(r.ctx, artifacts.UploadInput{ThreadID: r.th.ID, Login: "alice", Filename: "a.txt", Body: bytes.NewReader(text), Size: int64(len(text))})

		// Assert
		if !errors.Is(err, boom) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("a streamed blob above the backend cap is too large", func(t *testing.T) {
		// Arrange: the backend accepts the declared size but refuses the stream
		r := newSBRig(t, artifacts.Options{})
		svc := artifacts.New(r.mem, tooLargeStream{newStreamBlobs()}, nil, artifacts.Options{})
		text := []byte("hello streamed")

		// Act
		_, err := svc.Upload(r.ctx, artifacts.UploadInput{ThreadID: r.th.ID, Login: "alice", Filename: "a.txt", Body: bytes.NewReader(text), Size: int64(len(text))})

		// Assert
		if !errors.Is(err, artifacts.ErrTooLarge) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("a data upload surfaces a blob write failure", func(t *testing.T) {
		// Arrange
		r := newSBRig(t, artifacts.Options{})
		boom := errors.New("bucket down")
		svc := artifacts.New(r.mem, &failBlobs{hookBlobs: newHookBlobs(), putErr: boom}, nil, artifacts.Options{})

		// Act
		_, err := svc.Upload(r.ctx, artifacts.UploadInput{ThreadID: r.th.ID, Login: "alice", Filename: "a.txt", Data: []byte("x")})

		// Assert
		if !errors.Is(err, boom) || len(r.all()) != 0 {
			t.Fatalf("err %v artifacts %d", err, len(r.all()))
		}
	})
}

type tooLargeStream struct{ *streamBlobs }

func (tooLargeStream) PutStream(context.Context, string, io.Reader, int64) error {
	return artifacts.ErrBlobTooLarge
}

func TestUploadKeepsTheArtifactWhenTheSandboxWriteFails(t *testing.T) {
	// The artifact is recorded even if the live sandbox cannot take the file; it is then not marked as present there (the next restore brings it).
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	sb := r.sandbox()
	sb.SetWriteErr(func(string) error { return errors.New("disk full") })

	// Act
	a, err := r.svc.Upload(r.ctx, artifacts.UploadInput{ThreadID: r.th.ID, Login: "alice", Filename: "a.txt", Data: []byte("x")})

	// Assert
	if err != nil || a.SandboxID != "" {
		t.Fatalf("upload %+v %v", a, err)
	}
	if _, ok := sb.File("/workspace/a.txt"); ok {
		t.Fatal("file written despite the failure")
	}
	if stored := r.art("/workspace/a.txt"); stored == nil || stored.SandboxID != "" {
		t.Fatalf("stored %+v", stored)
	}
}

func TestUploadRecordsTheSandboxItWasWrittenTo(t *testing.T) {
	// An upload into a live sandbox is marked present there, so the next snapshot does not treat it as new or deleted.
	// Arrange
	r := newSBRig(t, artifacts.Options{})
	sb := r.sandbox()

	// Act
	a, err := r.svc.Upload(r.ctx, artifacts.UploadInput{ThreadID: r.th.ID, Login: "alice", Filename: "a.txt", Data: []byte("x")})

	// Assert
	if err != nil || a.SandboxID != sb.ID() || r.art("/workspace/a.txt").SandboxID != sb.ID() {
		t.Fatalf("upload %+v %v", a, err)
	}
	if got, _ := sb.File("/workspace/a.txt"); got != "x" {
		t.Fatalf("sandbox content %q", got)
	}
}
