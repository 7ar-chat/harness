package s3blobs_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/7ar-chat/harness/adapters/blobs/s3blobs"
	"github.com/7ar-chat/harness/core/blobs"
)

// fakeS3 is a minimal path-style S3 endpoint for bucket "bucket": bucket
// HEAD/PUT and object HEAD/GET/PUT/DELETE. fail maps "METHOD /path" to an
// error response (status, S3 error code) that replaces the normal handling.
type fakeS3 struct {
	mu      sync.Mutex
	bucket  bool
	objects map[string][]byte
	fail    map[string]s3Failure
	calls   []string
	auth    string
	onCall  func(call string)
}

type s3Failure struct {
	status int
	code   string
}

func newFakeS3() *fakeS3 {
	return &fakeS3{objects: map[string][]byte{}, fail: map[string]s3Failure{}}
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	call := r.Method + " " + strings.TrimSuffix(r.URL.Path, "/")
	f.calls = append(f.calls, call)
	f.auth = r.Header.Get("Authorization")
	hook := f.onCall
	failure, failing := f.fail[call]
	f.mu.Unlock()
	if hook != nil {
		hook(call)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if failing {
		w.WriteHeader(failure.status)
		if r.Method != http.MethodHead {
			_, _ = fmt.Fprintf(w, `<Error><Code>%s</Code><Message>injected</Message></Error>`, failure.code)
		}
		return
	}
	if r.URL.Path == "/bucket" || r.URL.Path == "/bucket/" {
		switch r.Method {
		case http.MethodHead:
			if !f.bucket {
				w.WriteHeader(http.StatusNotFound)
			}
		case http.MethodPut:
			f.bucket = true
		default:
			w.WriteHeader(http.StatusNotImplemented)
		}
		return
	}
	switch r.Method {
	case http.MethodHead, http.MethodGet:
		data, ok := f.objects[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, `<Error><Code>NoSuchKey</Code></Error>`)
			}
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		if r.Method == http.MethodGet {
			_, _ = w.Write(data)
		}
	case http.MethodPut:
		data, _ := io.ReadAll(r.Body)
		f.objects[r.URL.Path] = data
		w.Header().Set("ETag", `"etag"`)
	case http.MethodDelete:
		delete(f.objects, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func (f *fakeS3) count(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == call {
			n++
		}
	}
	return n
}

func (f *fakeS3) stored(path string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.objects[path]
	return d, ok
}

func rig(t *testing.T, prefix string) (*s3blobs.Blobs, *fakeS3) {
	t.Helper()
	fake := newFakeS3()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	b, err := s3blobs.New(s3blobs.Options{Bucket: "bucket", Endpoint: srv.URL, AccessKey: "k", SecretKey: "s", PathStyle: true, Prefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	return b, fake
}

func TestNewRequiresABucket(t *testing.T) {
	// A backend without a bucket is a configuration error, reported at construction.
	// Arrange / Act
	b, err := s3blobs.New(s3blobs.Options{Endpoint: "http://127.0.0.1:1"})

	// Assert
	if err == nil || b != nil {
		t.Fatalf("blobs %v err %v", b, err)
	}
}

func TestNewSignsRequestsInTheConfiguredRegionAndAnonymouslyWithoutKeys(t *testing.T) {
	// The region defaults to us-east-1; without an access key requests carry no signature at all.
	// Arrange
	fake := newFakeS3()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	signed, _ := s3blobs.New(s3blobs.Options{Bucket: "bucket", Endpoint: srv.URL + "/", AccessKey: "k", SecretKey: "s", PathStyle: true})
	regional, _ := s3blobs.New(s3blobs.Options{Bucket: "bucket", Endpoint: srv.URL, AccessKey: "k", SecretKey: "s", PathStyle: true, Region: "eu-west-3"})
	anonymous, _ := s3blobs.New(s3blobs.Options{Bucket: "bucket", Endpoint: srv.URL, PathStyle: true})
	ctx := context.Background()

	// Act
	_, _ = signed.Has(ctx, "a")
	defaultAuth := fake.auth
	_, _ = regional.Has(ctx, "a")
	regionalAuth := fake.auth
	_, _ = anonymous.Has(ctx, "a")
	anonymousAuth := fake.auth

	// Assert
	if !strings.Contains(defaultAuth, "/us-east-1/s3/") {
		t.Fatalf("default region auth %q", defaultAuth)
	}
	if !strings.Contains(regionalAuth, "/eu-west-3/s3/") {
		t.Fatalf("explicit region auth %q", regionalAuth)
	}
	if anonymousAuth != "" {
		t.Fatalf("anonymous request carries %q", anonymousAuth)
	}
	if fake.count("HEAD /bucket/a") != 3 {
		t.Fatalf("trailing slash on the endpoint must not change the path: %v", fake.calls)
	}
}

func TestBlobsRoundTripUnderThePrefixAndKeepTheFirstContent(t *testing.T) {
	// Objects live under prefix+key; a key keeps its first content (content-addressed) and Has/Get agree with what was stored.
	// Arrange
	b, fake := rig(t, "harness/")
	ctx := context.Background()

	// Act
	putErr := b.Put(ctx, "abc", []byte("first"))
	secondErr := b.Put(ctx, "abc", []byte("second"))
	got, getErr := b.Get(ctx, "abc")
	has, hasErr := b.Has(ctx, "abc")
	missing, missingErr := b.Has(ctx, "nope")

	// Assert
	if putErr != nil || secondErr != nil || getErr != nil || hasErr != nil || missingErr != nil {
		t.Fatalf("errors %v %v %v %v %v", putErr, secondErr, getErr, hasErr, missingErr)
	}
	if string(got) != "first" || !has || missing {
		t.Fatalf("got %q has %v missing %v", got, has, missing)
	}
	if stored, ok := fake.stored("/bucket/harness/abc"); !ok || string(stored) != "first" {
		t.Fatalf("stored %q %v", stored, ok)
	}
	if fake.count("PUT /bucket/harness/abc") != 1 {
		t.Fatalf("an existing key must not be uploaded again: %v", fake.calls)
	}
}

func TestPutStoresEmptyContent(t *testing.T) {
	// An empty blob is a valid blob: it is stored and read back as empty, not as missing.
	// Arrange
	b, _ := rig(t, "")
	ctx := context.Background()

	// Act
	putErr := b.Put(ctx, "empty", nil)
	got, getErr := b.Get(ctx, "empty")
	has, _ := b.Has(ctx, "empty")

	// Assert
	if putErr != nil || getErr != nil || len(got) != 0 || !has {
		t.Fatalf("put %v get %q %v has %v", putErr, got, getErr, has)
	}
}

func TestGetMapsMissingObjectsToErrNotFound(t *testing.T) {
	// A missing key surfaces as blobs.ErrNotFound whichever not-found code the store answers with; other failures stay distinct.
	// Arrange
	b, fake := rig(t, "")
	fake.fail["GET /bucket/legacy"] = s3Failure{http.StatusNotFound, "NotFound"}
	fake.fail["GET /bucket/denied"] = s3Failure{http.StatusForbidden, "AccessDenied"}
	ctx := context.Background()

	// Act
	_, noSuchKey := b.Get(ctx, "absent")
	_, genericNotFound := b.Get(ctx, "legacy")
	_, denied := b.Get(ctx, "denied")

	// Assert
	if !errors.Is(noSuchKey, blobs.ErrNotFound) || !errors.Is(genericNotFound, blobs.ErrNotFound) {
		t.Fatalf("NoSuchKey %v NotFound %v", noSuchKey, genericNotFound)
	}
	if denied == nil || errors.Is(denied, blobs.ErrNotFound) || !strings.Contains(denied.Error(), "s3 get denied") {
		t.Fatalf("access denied must not read as not found: %v", denied)
	}
}

func TestHasDistinguishesAbsentFromFailing(t *testing.T) {
	// A forbidden HEAD is an error, not "absent": callers must not treat an unreachable store as an empty one.
	// Arrange
	b, fake := rig(t, "")
	fake.fail["HEAD /bucket/denied"] = s3Failure{http.StatusForbidden, "AccessDenied"}
	ctx := context.Background()

	// Act
	has, err := b.Has(ctx, "denied")

	// Assert
	if err == nil || has || !strings.Contains(err.Error(), "s3 head denied") {
		t.Fatalf("has %v err %v", has, err)
	}
}

func TestPutStillUploadsWhenTheExistenceCheckFails(t *testing.T) {
	// The HEAD before an upload is an optimisation: when it fails the blob is uploaded anyway.
	// Arrange
	b, fake := rig(t, "")
	fake.fail["HEAD /bucket/k"] = s3Failure{http.StatusForbidden, "AccessDenied"}

	// Act
	err := b.Put(context.Background(), "k", []byte("data"))

	// Assert
	if stored, ok := fake.stored("/bucket/k"); err != nil || !ok || string(stored) != "data" {
		t.Fatalf("err %v stored %q %v", err, stored, ok)
	}
}

func TestPutReportsUploadFailures(t *testing.T) {
	// A rejected upload is returned to the caller with the key, and nothing is stored.
	// Arrange
	b, fake := rig(t, "")
	fake.fail["PUT /bucket/k"] = s3Failure{http.StatusForbidden, "AccessDenied"}

	// Act
	err := b.Put(context.Background(), "k", []byte("data"))

	// Assert
	if _, ok := fake.stored("/bucket/k"); err == nil || !strings.Contains(err.Error(), "s3 put k") || ok {
		t.Fatalf("err %v stored %v", err, ok)
	}
}

func TestPutStreamStoresSmallContentAndSkipsExistingKeys(t *testing.T) {
	// Streamed content is stored as given; streaming a key that exists is a no-op that does not even read the source.
	// Arrange
	b, fake := rig(t, "p/")
	ctx := context.Background()
	content := []byte("streamed content")

	// Act
	err := b.PutStream(ctx, "k", bytes.NewReader(content), int64(len(content)))
	again := b.PutStream(ctx, "k", failingReader{}, 99)

	// Assert
	if err != nil || again != nil {
		t.Fatalf("stream %v again %v", err, again)
	}
	if stored, ok := fake.stored("/bucket/p/k"); !ok || !bytes.Equal(stored, content) {
		t.Fatalf("stored %q %v", stored, ok)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("source must not be read") }

func TestPutStreamReportsFailures(t *testing.T) {
	// An upload rejected by the store, and a source that fails mid-read, are both returned and leave no object.
	// Arrange
	b, fake := rig(t, "")
	fake.fail["PUT /bucket/rejected"] = s3Failure{http.StatusForbidden, "AccessDenied"}
	ctx := context.Background()

	// Act
	rejected := b.PutStream(ctx, "rejected", strings.NewReader("data"), 4)
	broken := b.PutStream(ctx, "broken", failingReader{}, 4)

	// Assert
	if rejected == nil || !strings.Contains(rejected.Error(), "s3 upload rejected") {
		t.Fatalf("rejected: %v", rejected)
	}
	if broken == nil || !strings.Contains(broken.Error(), "s3 upload broken") {
		t.Fatalf("broken source: %v", broken)
	}
	if _, ok := fake.stored("/bucket/broken"); ok {
		t.Fatal("a failed stream must not leave an object behind")
	}
}

func TestDeleteRemovesObjectsAndToleratesMissingOnes(t *testing.T) {
	// Delete removes the object; deleting a key that is already gone (including a store that answers NoSuchKey) succeeds.
	// Arrange
	b, fake := rig(t, "")
	fake.fail["DELETE /bucket/gone"] = s3Failure{http.StatusNotFound, "NoSuchKey"}
	ctx := context.Background()
	_ = b.Put(ctx, "k", []byte("data"))

	// Act
	deleteErr := b.Delete(ctx, "k")
	has, _ := b.Has(ctx, "k")
	_, getErr := b.Get(ctx, "k")
	repeat := b.Delete(ctx, "k")
	gone := b.Delete(ctx, "gone")

	// Assert
	if deleteErr != nil || repeat != nil || gone != nil {
		t.Fatalf("delete %v repeat %v gone %v", deleteErr, repeat, gone)
	}
	if has || !errors.Is(getErr, blobs.ErrNotFound) {
		t.Fatalf("has %v get %v", has, getErr)
	}
}

func TestDeleteReportsRealFailures(t *testing.T) {
	// Only not-found is swallowed: a forbidden delete is an error, and the object stays.
	// Arrange
	b, fake := rig(t, "")
	ctx := context.Background()
	_ = b.Put(ctx, "k", []byte("data"))
	fake.fail["DELETE /bucket/k"] = s3Failure{http.StatusForbidden, "AccessDenied"}

	// Act
	err := b.Delete(ctx, "k")

	// Assert
	if _, ok := fake.stored("/bucket/k"); err == nil || !strings.Contains(err.Error(), "s3 delete k") || !ok {
		t.Fatalf("err %v stored %v", err, ok)
	}
}

func TestEnsureBucketCreatesAMissingBucketOnce(t *testing.T) {
	// A fresh store has no bucket: EnsureBucket creates it, and a second call finds it and creates nothing.
	// Arrange
	b, fake := rig(t, "")
	ctx := context.Background()

	// Act
	first := b.EnsureBucket(ctx)
	second := b.EnsureBucket(ctx)

	// Assert
	if first != nil || second != nil {
		t.Fatalf("first %v second %v", first, second)
	}
	if fake.count("PUT /bucket") != 1 || fake.count("HEAD /bucket") != 2 {
		t.Fatalf("calls %v", fake.calls)
	}
}

func TestEnsureBucketAcceptsABucketCreatedMeanwhile(t *testing.T) {
	// Losing the creation race to another process is fine, whether the store reports the bucket as owned by us or by someone else.
	for _, code := range []string{"BucketAlreadyOwnedByYou", "BucketAlreadyExists"} {
		t.Run(code, func(t *testing.T) {
			// Arrange
			b, fake := rig(t, "")
			fake.fail["PUT /bucket"] = s3Failure{http.StatusConflict, code}

			// Act
			err := b.EnsureBucket(context.Background())

			// Assert
			if err != nil {
				t.Fatalf("%s: %v", code, err)
			}
		})
	}
}

func TestEnsureBucketReportsRealFailures(t *testing.T) {
	// A forbidden HEAD or a rejected create is an error naming the bucket; neither is mistaken for "already there".
	// Arrange
	headDenied, headFake := rig(t, "")
	headFake.fail["HEAD /bucket"] = s3Failure{http.StatusForbidden, "AccessDenied"}
	createDenied, createFake := rig(t, "")
	createFake.fail["PUT /bucket"] = s3Failure{http.StatusForbidden, "AccessDenied"}
	ctx := context.Background()

	// Act
	headErr := headDenied.EnsureBucket(ctx)
	createErr := createDenied.EnsureBucket(ctx)

	// Assert
	if headErr == nil || !strings.Contains(headErr.Error(), "s3 head bucket bucket") || headFake.count("PUT /bucket") != 0 {
		t.Fatalf("head: %v calls %v", headErr, headFake.calls)
	}
	if createErr == nil || !strings.Contains(createErr.Error(), "s3 create bucket bucket") {
		t.Fatalf("create: %v", createErr)
	}
}

func TestCancelledContextAbortsRequests(t *testing.T) {
	// A context cancelled while the store is answering aborts the call with the context error and does not retry.
	// Arrange
	b, fake := rig(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	fake.onCall = func(string) { cancel() }
	fake.fail["GET /bucket/k"] = s3Failure{http.StatusInternalServerError, "InternalError"}

	// Act
	_, err := b.Get(ctx, "k")

	// Assert
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if n := fake.count("GET /bucket/k"); n != 1 {
		t.Fatalf("cancelled request attempted %d times", n)
	}
}

func TestMaxBytesIsUnbounded(t *testing.T) {
	// S3 imposes no per-blob cap, which callers read as 0.
	// Arrange
	b, _ := rig(t, "")

	// Act / Assert
	if b.MaxBytes() != 0 {
		t.Fatalf("max %d", b.MaxBytes())
	}
}
