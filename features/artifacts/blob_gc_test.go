package artifacts_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/features/artifacts"
	"github.com/GhaziBenDahmane/harness/store"
	"github.com/GhaziBenDahmane/harness/store/memstore"
)

// hookBlobs is an external (S3-like) blob backend whose Has and Delete can
// interleave other work: onHas runs after the existence check, before it is
// returned; onDelete runs before the object is removed.
type hookBlobs struct {
	mu       sync.Mutex
	m        map[string][]byte
	onHas    func(key string)
	onDelete func(key string)
}

func newHookBlobs() *hookBlobs { return &hookBlobs{m: map[string][]byte{}} }

func (b *hookBlobs) MaxBytes() int64 { return 0 }

func (b *hookBlobs) Put(_ context.Context, key string, data []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.m[key]; !ok {
		b.m[key] = append([]byte(nil), data...)
	}
	return nil
}

func (b *hookBlobs) Get(_ context.Context, key string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, ok := b.m[key]
	if !ok {
		return nil, artifacts.ErrBlobNotFound
	}
	return data, nil
}

func (b *hookBlobs) Has(_ context.Context, key string) (bool, error) {
	b.mu.Lock()
	_, ok := b.m[key]
	hook := b.onHas
	b.mu.Unlock()
	if hook != nil {
		hook(key)
	}
	return ok, nil
}

func (b *hookBlobs) Delete(_ context.Context, key string) error {
	b.mu.Lock()
	hook := b.onDelete
	b.mu.Unlock()
	if hook != nil {
		hook(key)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.m, key)
	return nil
}

func (b *hookBlobs) has(key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.m[key]
	return ok
}

func sumOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// gcRig is a service on a memstore with a fake clock.
type gcRig struct {
	t     *testing.T
	ctx   context.Context
	st    *memstore.Store
	svc   *artifacts.Service
	mu    sync.Mutex
	clock time.Time
}

func newGCRig(t *testing.T, blobs artifacts.Blobs) *gcRig {
	r := &gcRig{t: t, ctx: context.Background(), st: memstore.New(), clock: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)}
	r.st.Now = func() time.Time {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.clock
	}
	if blobs == nil {
		blobs = artifacts.PGBlobs{S: r.st}
	}
	r.svc = artifacts.New(r.st, blobs, nil, artifacts.Options{BlobGCGrace: 24 * time.Hour})
	return r
}

func (r *gcRig) advance(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clock = r.clock.Add(d)
}

func (r *gcRig) upload(threadID, content string) *store.Artifact {
	r.t.Helper()
	a, err := r.svc.Upload(r.ctx, artifacts.UploadInput{ThreadID: threadID, Login: "alice", Filename: "notes.txt",
		DeclaredMIME: "text/plain", Data: []byte(content)})
	if err != nil {
		r.t.Fatal(err)
	}
	return a
}

// unreferenced uploads content in a thread, then deletes and purges that
// thread: the content's last reference is dropped now.
func (r *gcRig) unreferenced(content string) {
	r.t.Helper()
	th, _ := r.st.CreateThread(r.ctx, "alice", "old")
	r.upload(th.ID, content)
	_, _ = r.st.DeleteThread(r.ctx, "alice", th.ID)
	r.st.SetThreadDeletedAt(th.ID, r.st.Now().Add(-48*time.Hour))
	if threads, _, err := r.svc.Purge(r.ctx, 24*time.Hour); err != nil || threads != 1 {
		r.t.Fatalf("purge of the old thread: %d %v", threads, err)
	}
}

func (r *gcRig) purgeBlobs() int {
	r.t.Helper()
	_, blobs, err := r.svc.Purge(r.ctx, 24*time.Hour)
	if err != nil {
		r.t.Fatal(err)
	}
	return blobs
}

func TestBlobGCKeepsAnUnreferencedBlobForTheGracePeriod(t *testing.T) {
	// A blob is only deleted once it has had no reference for the grace period (with its cached preview).
	// Arrange
	blobs := newHookBlobs()
	r := newGCRig(t, blobs)
	key := sumOf("quarterly report")
	r.unreferenced("quarterly report")
	_ = blobs.Put(r.ctx, "preview/"+key+".pdf", []byte("%PDF"))

	// Act
	r.advance(23 * time.Hour)
	early := r.purgeBlobs()
	keptEarly := blobs.has(key)
	r.advance(2 * time.Hour)
	late := r.purgeBlobs()

	// Assert
	if early != 0 || !keptEarly {
		t.Fatalf("collected within the grace period: %d kept %v", early, keptEarly)
	}
	if late != 1 || blobs.has(key) || blobs.has("preview/"+key+".pdf") {
		t.Fatalf("not collected after the grace period: %d blob %v preview %v", late, blobs.has(key), blobs.has("preview/"+key+".pdf"))
	}
}

func TestBlobGCOnTheDatabaseBackendHonoursTheGracePeriod(t *testing.T) {
	// The Postgres backend (work_blobs) deletes a queued blob and its preview only after the grace period.
	// Arrange
	r := newGCRig(t, nil)
	key := sumOf("database content")
	r.unreferenced("database content")
	_ = r.st.PutBlob(r.ctx, "preview/"+key+".pdf", []byte("%PDF"))

	// Act
	r.advance(23 * time.Hour)
	early := r.purgeBlobs()
	keptEarly := r.st.Blob(key)
	r.advance(2 * time.Hour)
	late := r.purgeBlobs()

	// Assert
	if early != 0 || !keptEarly || late != 1 || r.st.Blob(key) || r.st.Blob("preview/"+key+".pdf") {
		t.Fatalf("early %d kept %v late %d blob %v preview %v", early, keptEarly, late, r.st.Blob(key), r.st.Blob("preview/"+key+".pdf"))
	}
}

func TestBlobGCSparesABlobAConcurrentUploadIsReusing(t *testing.T) {
	// GC running between an upload's "content already stored" check and its version insert does not delete the reused blob.
	// Arrange
	blobs := newHookBlobs()
	r := newGCRig(t, blobs)
	key := sumOf("same bytes")
	r.unreferenced("same bytes")
	r.advance(25 * time.Hour)
	var once sync.Once
	var gcErr error
	blobs.onHas = func(k string) {
		if k == key {
			once.Do(func() { _, _, gcErr = r.svc.Purge(r.ctx, 24*time.Hour) })
		}
	}
	th, _ := r.st.CreateThread(r.ctx, "alice", "new")

	// Act
	a := r.upload(th.ID, "same bytes")
	blobs.onHas = nil
	r.advance(48 * time.Hour)
	later := r.purgeBlobs()

	// Assert
	if gcErr != nil || a.SHA256 != key {
		t.Fatalf("gc %v sha %s", gcErr, a.SHA256)
	}
	if !blobs.has(key) || later != 0 {
		t.Fatalf("the reused blob was collected: present %v later %d", blobs.has(key), later)
	}
}

func TestUploadDuringAnInFlightCollectionStoresTheContentAgain(t *testing.T) {
	// An upload reusing a blob the GC is deleting waits for the collection, then stores the content again.
	// Arrange
	blobs := newHookBlobs()
	r := newGCRig(t, blobs)
	key := sumOf("racing bytes")
	r.unreferenced("racing bytes")
	r.advance(25 * time.Hour)
	th, _ := r.st.CreateThread(r.ctx, "alice", "new")
	done := make(chan error, 1)
	var once sync.Once
	blobs.onDelete = func(k string) {
		if k != key {
			return
		}
		once.Do(func() {
			go func() {
				_, err := r.svc.Upload(r.ctx, artifacts.UploadInput{ThreadID: th.ID, Login: "alice", Filename: "notes.txt",
					DeclaredMIME: "text/plain", Data: []byte("racing bytes")})
				done <- err
			}()
			time.Sleep(100 * time.Millisecond) // the upload checks the blob while its deletion is in flight
		})
	}

	// Act
	collected := r.purgeBlobs()
	var uploadErr error
	select {
	case uploadErr = <-done:
	case <-time.After(5 * time.Second):
		uploadErr = errors.New("upload did not finish")
	}

	// Assert
	if collected != 1 || uploadErr != nil {
		t.Fatalf("collected %d upload %v", collected, uploadErr)
	}
	data, err := r.svc.Blobs.Get(r.ctx, key)
	if err != nil || string(data) != "racing bytes" {
		t.Fatalf("content lost after the collection: %q %v", data, err)
	}
}

func TestBlobGCSparesTheBlobOfAPurgedThreadAConcurrentUploadReuses(t *testing.T) {
	// A purge that drops the last reference of a blob while an upload of identical content is reusing it keeps the blob.
	// Arrange
	blobs := newHookBlobs()
	r := newGCRig(t, blobs)
	key := sumOf("shared bytes")
	old, _ := r.st.CreateThread(r.ctx, "alice", "old")
	r.upload(old.ID, "shared bytes")
	_, _ = r.st.DeleteThread(r.ctx, "alice", old.ID)
	r.st.SetThreadDeletedAt(old.ID, r.st.Now().Add(-48*time.Hour))
	var once sync.Once
	var purged int
	var gcErr error
	blobs.onHas = func(k string) {
		if k == key {
			once.Do(func() { purged, _, gcErr = r.svc.Purge(r.ctx, 24*time.Hour) })
		}
	}
	th, _ := r.st.CreateThread(r.ctx, "alice", "new")

	// Act
	a := r.upload(th.ID, "shared bytes")

	// Assert
	data, err := r.svc.Blobs.Get(r.ctx, a.SHA256)
	if gcErr != nil || purged != 1 || err != nil || string(data) != "shared bytes" {
		t.Fatalf("gc %v purged %d content %q %v", gcErr, purged, data, err)
	}
}
