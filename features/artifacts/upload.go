package artifacts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/GhaziBenDahmane/harness/core/sandbox"
	"github.com/GhaziBenDahmane/harness/core/store"
)

var controlChars = regexp.MustCompile(`[\x00-\x1f\x7f]+`)

// SanitizeFilename keeps a safe single path component.
func SanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base("/" + name)
	name = controlChars.ReplaceAllString(name, "_")
	name = strings.Trim(name, " .")
	name = strings.ReplaceAll(name, `"`, "'")
	if !utf8.ValidString(name) {
		name = strings.ToValidUTF8(name, "_")
	}
	if name == "" || name == "/" {
		name = "upload"
	}
	if len(name) > 200 {
		ext := path.Ext(name)
		if len(ext) > 20 {
			ext = ""
		}
		base := strings.TrimSuffix(name, ext)
		for len(base)+len(ext) > 200 {
			_, size := utf8.DecodeLastRuneInString(base)
			base = base[:len(base)-size]
		}
		name = base + ext
	}
	return name
}

type magic struct {
	mime   string
	prefix [][]byte
	check  func([]byte) bool
}

var knownTypes = map[string]magic{
	".png":  {mime: "image/png", prefix: [][]byte{[]byte("\x89PNG\r\n\x1a\n")}},
	".jpg":  {mime: "image/jpeg", prefix: [][]byte{{0xff, 0xd8, 0xff}}},
	".jpeg": {mime: "image/jpeg", prefix: [][]byte{{0xff, 0xd8, 0xff}}},
	".gif":  {mime: "image/gif", prefix: [][]byte{[]byte("GIF87a"), []byte("GIF89a")}},
	".webp": {mime: "image/webp", check: func(b []byte) bool {
		return len(b) >= 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP"))
	}},
	".zip": {mime: "application/zip", prefix: [][]byte{[]byte("PK\x03\x04"), []byte("PK\x05\x06")}},
}

var mimeAliases = map[string]string{"image/jpg": "image/jpeg", "application/x-zip-compressed": "application/zip"}

func normalizeMIME(ct string) string {
	ct, _, _ = strings.Cut(ct, ";")
	ct = strings.ToLower(strings.TrimSpace(ct))
	if a, ok := mimeAliases[ct]; ok {
		return a
	}
	return ct
}

// checkType enforces extension / declared MIME / magic-byte agreement for
// known types: PDF and OOXML with the ported detection, images and zip by signature.
func checkType(name, declared string, data []byte) error {
	if _, ok := detectFormat(name, declared, data); !ok {
		return ErrTypeMismatch
	}
	ext := strings.ToLower(path.Ext(name))
	mt := normalizeMIME(declared)
	generic := mt == "" || mt == "application/octet-stream"
	if k, ok := knownTypes[ext]; ok {
		if !generic && mt != k.mime {
			return ErrTypeMismatch
		}
		matched := k.check != nil && k.check(data)
		for _, p := range k.prefix {
			matched = matched || bytes.HasPrefix(data, p)
		}
		if !matched {
			return ErrTypeMismatch
		}
		return nil
	}
	if !generic {
		for e, k := range knownTypes {
			if k.mime == mt && e != ext {
				return ErrTypeMismatch // e.g. declared image/png on a .txt
			}
		}
	}
	return nil
}

// UploadDir is where attachments land in the sandbox by default (the
// workspace root, e.g. /workspace/<name>, "name (n).ext" when taken).
const UploadDir = sandbox.Workspace

// ErrInvalidTargetPath means an upload's targetPath is not a file path under /workspace.
var ErrInvalidTargetPath = errors.New("targetPath must be a file path under /workspace")

// UploadTarget validates an upload's optional target path: absolute under
// /workspace or relative to it, no control characters, no "..", not the
// workspace itself and not inside a directory snapshots skip (.git,
// node_modules, .venv, __pycache__, the scratch /workspace/.work). It returns
// the directory and the sanitized file name.
func UploadTarget(target string) (dir, name string, err error) {
	raw := strings.ReplaceAll(target, "\\", "/")
	if controlChars.MatchString(raw) || !utf8.ValidString(raw) {
		return "", "", ErrInvalidTargetPath
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", ErrInvalidTargetPath
	}
	if !strings.HasPrefix(raw, "/") {
		raw = sandbox.Workspace + "/" + raw
	}
	for _, part := range strings.Split(raw, "/") {
		if part == ".." {
			return "", "", ErrInvalidTargetPath
		}
	}
	p := path.Clean(raw)
	if !strings.HasPrefix(p, sandbox.Workspace+"/") || strings.HasSuffix(raw, "/") {
		return "", "", ErrInvalidTargetPath
	}
	for _, part := range strings.Split(strings.TrimPrefix(p, sandbox.Workspace+"/"), "/") {
		if part == ".git" {
			return "", "", ErrInvalidTargetPath
		}
	}
	if InSkippedDir(p) {
		return "", "", ErrInvalidTargetPath
	}
	base := path.Base(p)
	name = SanitizeFilename(base)
	if name != base {
		return "", "", ErrInvalidTargetPath
	}
	return path.Dir(p), name, nil
}

// UploadInput is one validated upload: Data, or Size bytes readable from Body
// (e.g. the request's temporary file; plain files are then streamed to the
// blob store and the sandbox instead of being held in memory).
type UploadInput struct {
	ThreadID     string
	Login        string
	Filename     string
	DeclaredMIME string
	Data         []byte
	Body         io.ReaderAt
	Size         int64
	// TargetPath is the optional client-chosen path (see UploadTarget); it is
	// de-duplicated like the default one.
	TargetPath string
}

// sniffBytes is how much of a streamed upload the type checks read.
const sniffBytes = 64 << 10

// content is an upload's bytes: in memory, or a section of Body.
type content struct {
	data []byte
	body io.ReaderAt
	size int64
}

func (c content) reader() io.Reader {
	if c.data != nil {
		return bytes.NewReader(c.data)
	}
	return io.NewSectionReader(c.body, 0, c.size)
}

// inMemory reports uploads whose checks need the whole content: PDF / Office
// documents (by name, declared type or content: format detection) and zip
// archives, whatever their name.
func inMemory(name, declared string, head, tail []byte) bool {
	return isDocument(name, declared, head) ||
		needsWholeContent(head, tail) || strings.EqualFold(path.Ext(name), ".zip") ||
		normalizeMIME(declared) == "application/zip"
}

// tailBytes covers a zip end-of-central-directory record with the longest comment.
const tailBytes = 64<<10 + 22

// prepare validates the upload and returns its content and sha256.
func (s *Service) prepare(name string, in UploadInput) (content, string, error) {
	c := content{data: in.Data, body: in.Body, size: in.Size}
	if c.data == nil && c.body == nil {
		c.data = []byte{}
	}
	if c.data != nil {
		c.size = int64(len(c.data))
	}
	head := c.data
	if head == nil {
		buf := make([]byte, min(c.size, sniffBytes))
		n, err := c.body.ReadAt(buf, 0)
		if err != nil && !errors.Is(err, io.EOF) {
			return content{}, "", err
		}
		head = buf[:n]
		tail := make([]byte, min(c.size, tailBytes))
		n, err = c.body.ReadAt(tail, c.size-int64(len(tail)))
		if err != nil && !errors.Is(err, io.EOF) {
			return content{}, "", err
		}
		if inMemory(name, in.DeclaredMIME, head, tail[:n]) {
			data, err := io.ReadAll(io.NewSectionReader(c.body, 0, c.size))
			if err != nil {
				return content{}, "", err
			}
			c = content{data: data, size: int64(len(data))}
			head = data
		}
	}
	if err := checkType(name, in.DeclaredMIME, head); err != nil {
		return content{}, "", err
	}
	if max := s.Blobs.MaxBytes(); max > 0 && c.size > max {
		return content{}, "", ErrTooLarge
	}
	if c.data != nil {
		return c, sha(c.data), nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, c.reader()); err != nil {
		return content{}, "", err
	}
	return c, hex.EncodeToString(h.Sum(nil)), nil
}

// putContent stores an upload's content (streamed for Body uploads), like putBlob.
func (s *Service) putContent(ctx context.Context, key string, c content) error {
	if c.data != nil {
		return s.putBlob(ctx, key, c.data)
	}
	if err := s.Store.TouchBlob(ctx, key); err != nil {
		return err
	}
	if ok, err := s.Blobs.Has(ctx, key); err == nil && ok {
		return nil
	}
	if err := s.Store.QueueBlob(ctx, key); err != nil { // never an orphan (C-25)
		return err
	}
	err := PutReader(ctx, s.Blobs, key, c.reader(), c.size)
	if errors.Is(err, ErrBlobTooLarge) {
		return ErrTooLarge
	}
	return err
}

// Upload validates and stores an attachment as an artifact of kind upload at
// /workspace/<name> or the validated TargetPath (de-duplicated as
// "name (2).ext", "name (3).ext"… ), and writes it into the
// thread's sandbox when one is running.
func (s *Service) Upload(ctx context.Context, in UploadInput) (*store.Artifact, error) {
	dir, name := UploadDir, SanitizeFilename(in.Filename)
	if in.TargetPath != "" {
		var err error
		if dir, name, err = UploadTarget(in.TargetPath); err != nil {
			return nil, err
		}
	}
	c, sum, err := s.prepare(name, in)
	if err != nil {
		return nil, err
	}
	if err := s.putContent(ctx, sum, c); err != nil {
		return nil, err
	}
	unlock, err := s.lockThread(ctx, in.ThreadID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	existing, err := s.Store.ListArtifacts(ctx, in.ThreadID)
	if err != nil {
		return nil, err
	}
	taken := map[string]bool{}
	for _, a := range existing {
		taken[a.Path] = true
	}
	var sb sandbox.Sandbox
	if s.Sandbox != nil && s.Sandbox.Enabled() {
		if got, err := s.Sandbox.Get(ctx, in.ThreadID); err == nil {
			sb = got
		}
	}
	ext := path.Ext(name)
	base := strings.TrimSuffix(name, ext)
	finalName := name
	for n := 2; ; n++ {
		p := dir + "/" + finalName
		free := !taken[p]
		if free && sb != nil {
			if _, err := sb.Stat(ctx, p); err == nil {
				free = false // a file the agent wrote there, not yet snapshotted
			}
		}
		if free {
			break
		}
		finalName = base + " (" + strconv.Itoa(n) + ")" + ext
	}
	p := dir + "/" + finalName
	a, _, err := s.put(ctx, store.PutArtifact{ThreadID: in.ThreadID, UserLogin: in.Login, Path: p, Name: finalName,
		MimeType: MimeType(finalName), Kind: store.ArtifactUpload, SizeBytes: c.size, SHA256: sum})
	if err != nil {
		return nil, err
	}
	if sb != nil {
		// Until it is written, the upload is pending in this sandbox (never taken
		// for a deletion); it arrives with the workspace restore of the next one otherwise.
		if err := sandbox.WriteFrom(ctx, sb, p, c.reader()); err != nil {
			if !errors.Is(err, context.Canceled) {
				slog.Warn("artifacts: writing upload into the sandbox failed", "thread_id", in.ThreadID, "path", p, "error", err)
			}
		} else if err := s.Store.MarkArtifactsMaterialized(ctx, sb.ID(), []string{a.ID}); err != nil {
			slog.Warn("artifacts: recording the written upload failed", "thread_id", in.ThreadID, "error", err)
		} else {
			a.SandboxID = sb.ID()
		}
	}
	return a, nil
}
