// Package artifacts makes the files the agent produces under /workspace
// durable: snapshots after file-changing tools (and at run end), versioned and
// sha256-deduplicated content in a blob store (Postgres or a host-provided Blobs (see artifacts/s3blobs)),
// workspace restore on sandbox (re)creation, classified downloads, Office → PDF
// previews and user uploads.
package artifacts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/GhaziBenDahmane/harness/store"
)

// ErrBlobNotFound means the key is unknown to the blob store.
var ErrBlobNotFound = errors.New("blob not found")

// ErrBlobTooLarge means the content exceeds the backend's per-blob cap.
var ErrBlobTooLarge = errors.New("blob exceeds the storage limit")

// Blobs is the content store; keys are content sha256 (hex) or derived keys
// such as "preview/<owner tag>/<sha256>.pdf".
type Blobs interface {
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	Has(ctx context.Context, key string) (bool, error)
	// Delete removes a blob (no error when it does not exist).
	Delete(ctx context.Context, key string) error
	// MaxBytes is the per-blob cap (0 = no cap).
	MaxBytes() int64
}

// StreamPutter is a blob backend that stores content read from r (size bytes)
// without the caller holding it in memory (uploads, workspace Git archives).
type StreamPutter interface {
	PutStream(ctx context.Context, key string, r io.Reader, size int64) error
}

// PutReader stores size bytes read from r: streamed when the backend supports
// it, else read into memory first (ErrBlobTooLarge above the backend's cap).
func PutReader(ctx context.Context, b Blobs, key string, r io.Reader, size int64) error {
	if sp, ok := b.(StreamPutter); ok {
		return sp.PutStream(ctx, key, r, size)
	}
	if max := b.MaxBytes(); max > 0 && size > max {
		return ErrBlobTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(r, size+1))
	if err != nil {
		return err
	}
	if int64(len(data)) != size {
		return fmt.Errorf("blob %s: read %d bytes, want %d", key, len(data), size)
	}
	return b.Put(ctx, key, data)
}

// PGBlobMax is the Postgres backend cap per blob.
const PGBlobMax = 50 << 20

// PGBlobs stores blobs in work_blobs (default when no bucket is configured).
type PGBlobs struct{ S store.BlobStore }

func (b PGBlobs) MaxBytes() int64 { return PGBlobMax }

func (b PGBlobs) Put(ctx context.Context, key string, data []byte) error {
	if int64(len(data)) > PGBlobMax {
		return ErrBlobTooLarge
	}
	return b.S.PutBlob(ctx, key, data)
}

// PutStream implements StreamPutter: Postgres stores one bytea value, so the
// content is read into memory, within the PGBlobMax cap (ErrBlobTooLarge).
func (b PGBlobs) PutStream(ctx context.Context, key string, r io.Reader, size int64) error {
	if size > PGBlobMax {
		return ErrBlobTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(r, PGBlobMax+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > PGBlobMax {
		return ErrBlobTooLarge
	}
	return b.Put(ctx, key, data)
}

func (b PGBlobs) Get(ctx context.Context, key string) ([]byte, error) {
	data, err := b.S.GetBlob(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrBlobNotFound
	}
	return data, err
}

func (b PGBlobs) Has(ctx context.Context, key string) (bool, error) { return b.S.HasBlob(ctx, key) }

func (b PGBlobs) Delete(ctx context.Context, key string) error { return b.S.DeleteBlob(ctx, key) }

// CollectUnreferenced deletes in one statement the queued keys still
// unreferenced for grace, with their preview (blob GC of the retention purge).
func (b PGBlobs) CollectUnreferenced(ctx context.Context, keys []string, grace time.Duration) ([]string, error) {
	return b.S.CollectBlobs(ctx, keys, grace)
}

// FallbackBlobs writes to Primary and reads a key Primary does not have from
// Legacy, read-only: switching a deployment from Postgres blobs to S3 (e.g.
// the dev RustFS) keeps the content stored before readable. Deletes (blob GC
// of unreferenced keys) go to both, so legacy garbage is collected too.
type FallbackBlobs struct {
	Primary Blobs
	Legacy  Blobs
}

var (
	_ Blobs        = FallbackBlobs{}
	_ StreamPutter = FallbackBlobs{}
)

func (b FallbackBlobs) MaxBytes() int64 { return b.Primary.MaxBytes() }

func (b FallbackBlobs) Put(ctx context.Context, key string, data []byte) error {
	return b.Primary.Put(ctx, key, data)
}

// PutStream implements StreamPutter (streamed when Primary supports it).
func (b FallbackBlobs) PutStream(ctx context.Context, key string, r io.Reader, size int64) error {
	return PutReader(ctx, b.Primary, key, r, size)
}

func (b FallbackBlobs) Get(ctx context.Context, key string) ([]byte, error) {
	data, err := b.Primary.Get(ctx, key)
	if !errors.Is(err, ErrBlobNotFound) || b.Legacy == nil {
		return data, err
	}
	return b.Legacy.Get(ctx, key)
}

func (b FallbackBlobs) Has(ctx context.Context, key string) (bool, error) {
	ok, err := b.Primary.Has(ctx, key)
	if err != nil || ok || b.Legacy == nil {
		return ok, err
	}
	return b.Legacy.Has(ctx, key)
}

func (b FallbackBlobs) Delete(ctx context.Context, key string) error {
	err := b.Primary.Delete(ctx, key)
	if b.Legacy != nil {
		if lerr := b.Legacy.Delete(ctx, key); err == nil {
			err = lerr
		}
	}
	return err
}
