package artifacts_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/GhaziBenDahmane/harness/artifacts"
	"github.com/GhaziBenDahmane/harness/store"
	"github.com/GhaziBenDahmane/harness/store/memstore"
)

// streamBlobs records how content reaches it: PutStream (streamed) or Put (in memory).
type streamBlobs struct {
	*hookBlobs
	mu       sync.Mutex
	streamed []string
	buffered []string
	maxRead  int
}

func newStreamBlobs() *streamBlobs { return &streamBlobs{hookBlobs: newHookBlobs()} }

func (b *streamBlobs) Put(ctx context.Context, key string, data []byte) error {
	b.mu.Lock()
	b.buffered = append(b.buffered, key)
	b.mu.Unlock()
	return b.hookBlobs.Put(ctx, key, data)
}

func (b *streamBlobs) PutStream(ctx context.Context, key string, r io.Reader, size int64) error {
	var buf bytes.Buffer
	chunk := make([]byte, 32<<10)
	for {
		n, err := r.Read(chunk)
		b.mu.Lock()
		b.maxRead = max(b.maxRead, n)
		b.mu.Unlock()
		buf.Write(chunk[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if int64(buf.Len()) != size {
		return fmt.Errorf("size %d, want %d", buf.Len(), size)
	}
	b.mu.Lock()
	b.streamed = append(b.streamed, key)
	b.mu.Unlock()
	return b.hookBlobs.Put(ctx, key, buf.Bytes())
}

// zeros is a large ReaderAt that allocates nothing.
type zeros int64

func (z zeros) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(z) {
		return 0, io.EOF
	}
	n := min(int64(len(p)), int64(z)-off)
	clear(p[:n])
	if off+n == int64(z) {
		return int(n), io.EOF
	}
	return int(n), nil
}

func sum(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}

func TestPlainUploadsAreStreamedToTheBlobStore(t *testing.T) {
	// A plain file read from the request's temporary file goes to the blob store through PutStream; zip archives (entries scan) are read into memory.
	// Arrange
	st := memstore.New()
	th, _ := st.CreateThread(context.Background(), "alice", "")
	blobs := newStreamBlobs()
	svc := artifacts.New(st, blobs, nil, nil, artifacts.Options{})
	text := bytes.Repeat([]byte("line of text\n"), 20000) // 260 kB
	zipData := zipBytes(t, map[string]string{"a.txt": "a"})

	// Act
	plain, plainErr := svc.Upload(context.Background(), artifacts.UploadInput{ThreadID: th.ID, Login: "alice", Filename: "notes.txt",
		DeclaredMIME: "text/plain", Body: bytes.NewReader(text), Size: int64(len(text))})
	archive, zipErr := svc.Upload(context.Background(), artifacts.UploadInput{ThreadID: th.ID, Login: "alice", Filename: "bundle.zip",
		Body: bytes.NewReader(zipData), Size: int64(len(zipData))})

	// Assert
	if plainErr != nil || zipErr != nil {
		t.Fatalf("plain %v zip %v", plainErr, zipErr)
	}
	if plain.Path != "/workspace/notes.txt" || plain.SHA256 != sum(text) || plain.SizeBytes != int64(len(text)) || archive.Path != "/workspace/bundle.zip" {
		t.Fatalf("plain %+v archive %+v", plain, archive)
	}
	if len(blobs.streamed) != 1 || blobs.streamed[0] != sum(text) || len(blobs.buffered) != 1 || blobs.buffered[0] != sum(zipData) || blobs.maxRead > 32<<10 {
		t.Fatalf("streamed %v buffered %v max read %d", blobs.streamed, blobs.buffered, blobs.maxRead)
	}
}

func TestStreamedUploadAboveTheDatabaseCapIsRefusedWithoutReadingIt(t *testing.T) {
	// The Postgres backend keeps its 50 MiB cap: a larger streamed upload is ErrTooLarge (413 file_too_large) before any content is stored.
	// Arrange
	st := memstore.New()
	th, _ := st.CreateThread(context.Background(), "alice", "")
	svc := artifacts.New(st, artifacts.PGBlobs{S: st}, nil, nil, artifacts.Options{})

	// Act
	_, err := svc.Upload(context.Background(), artifacts.UploadInput{ThreadID: th.ID, Login: "alice", Filename: "video.bin",
		Body: zeros(artifacts.PGBlobMax + 1), Size: artifacts.PGBlobMax + 1})
	list, _ := st.ListArtifacts(context.Background(), th.ID)

	// Assert
	if !errors.Is(err, artifacts.ErrTooLarge) || len(list) != 0 {
		t.Fatalf("err %v artifacts %d", err, len(list))
	}
}

func TestPutReaderOnTheDatabaseBackend(t *testing.T) {
	// PGBlobs.PutStream stores content within the cap and refuses larger content with ErrBlobTooLarge.
	// Arrange
	st := memstore.New()
	b := artifacts.PGBlobs{S: st}

	// Act
	okErr := artifacts.PutReader(context.Background(), b, "k", strings.NewReader("hello"), 5)
	got, _ := b.Get(context.Background(), "k")
	tooBig := artifacts.PutReader(context.Background(), b, "big", io.LimitReader(zeroReader{}, artifacts.PGBlobMax+1), artifacts.PGBlobMax+1)

	// Assert
	if okErr != nil || string(got) != "hello" || !errors.Is(tooBig, artifacts.ErrBlobTooLarge) {
		t.Fatalf("ok %v %q too big %v", okErr, got, tooBig)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// fakeS3 is a minimal S3 endpoint: HEAD/GET/PUT objects and multipart uploads.
type fakeS3 struct {
	mu        sync.Mutex
	objects   map[string][]byte
	parts     map[string]map[int][]byte
	multipart int
	puts      int
}

func newFakeS3() *fakeS3 {
	return &fakeS3{objects: map[string][]byte{}, parts: map[string]map[int][]byte{}}
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.URL.Path
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodHead || r.Method == http.MethodGet:
		data, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, `<Error><Code>NoSuchKey</Code></Error>`)
			}
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		if r.Method == http.MethodGet {
			_, _ = w.Write(data)
		}
	case r.Method == http.MethodPost && q.Has("uploads"):
		id := fmt.Sprintf("upload-%d", len(f.parts)+1)
		f.parts[id] = map[int][]byte{}
		_, _ = fmt.Fprintf(w, `<InitiateMultipartUploadResult><Bucket>b</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, key, id)
	case r.Method == http.MethodPut && q.Has("partNumber"):
		n, _ := strconv.Atoi(q.Get("partNumber"))
		data, _ := io.ReadAll(r.Body)
		f.parts[q.Get("uploadId")][n] = data
		w.Header().Set("ETag", fmt.Sprintf(`"etag-%d"`, n))
	case r.Method == http.MethodPost && q.Has("uploadId"):
		var req struct {
			Parts []struct {
				PartNumber int `xml:"PartNumber"`
			} `xml:"Part"`
		}
		_ = xml.NewDecoder(r.Body).Decode(&req)
		parts := f.parts[q.Get("uploadId")]
		nums := make([]int, 0, len(parts))
		for n := range parts {
			nums = append(nums, n)
		}
		sort.Ints(nums)
		var all []byte
		for _, n := range nums {
			all = append(all, parts[n]...)
		}
		f.objects[key] = all
		f.multipart++
		_, _ = fmt.Fprintf(w, `<CompleteMultipartUploadResult><Bucket>b</Bucket><Key>%s</Key><ETag>"done"</ETag></CompleteMultipartUploadResult>`, key)
	case r.Method == http.MethodPut:
		data, _ := io.ReadAll(r.Body)
		f.objects[key] = data
		f.puts++
		w.Header().Set("ETag", `"etag"`)
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func TestS3PutStreamUsesMultipartUploadsForLargeContent(t *testing.T) {
	// Streamed content above 16 MiB goes up as a multipart upload (8 MiB parts), smaller content as one PutObject; both read back intact.
	// Arrange
	fake := newFakeS3()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	b, err := artifacts.NewS3(artifacts.S3Options{Bucket: "bucket", Endpoint: srv.URL, AccessKey: "k", SecretKey: "s", PathStyle: true, Prefix: "work/"})
	if err != nil {
		t.Fatal(err)
	}
	large := bytes.Repeat([]byte("0123456789abcdef"), (17<<20)/16)
	small := []byte("small content")
	ctx := context.Background()

	// Act
	largeErr := b.PutStream(ctx, "large", bytes.NewReader(large), int64(len(large)))
	smallErr := b.PutStream(ctx, "small", bytes.NewReader(small), int64(len(small)))
	again := b.PutStream(ctx, "small", bytes.NewReader([]byte("other")), 5)
	gotLarge, _ := b.Get(ctx, "large")
	gotSmall, _ := b.Get(ctx, "small")

	// Assert
	if largeErr != nil || smallErr != nil || again != nil {
		t.Fatalf("large %v small %v again %v", largeErr, smallErr, again)
	}
	if fake.multipart != 1 || fake.puts != 1 || !bytes.Equal(gotLarge, large) || !bytes.Equal(gotSmall, small) {
		t.Fatalf("multipart %d puts %d large ok %v small %q", fake.multipart, fake.puts, bytes.Equal(gotLarge, large), gotSmall)
	}
	if _, ok := fake.objects["/bucket/work/large"]; !ok {
		t.Fatalf("keys %v", keys(fake.objects))
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

var _ store.ArtifactStore = (*memstore.Store)(nil)

func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(content))
	}
	_ = zw.Close()
	return buf.Bytes()
}
