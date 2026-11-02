// Package blobs is the port for storing content by key outside the database: artifact contents, previews, archives.
// Implement Blobs over S3, a disk or memory; the engine depends on the interface only.
package blobs

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// ErrNotFound means the key is unknown to the blob store.
var ErrNotFound = errors.New("blob not found")

// ErrTooLarge means the content exceeds the backend's per-blob cap.
var ErrTooLarge = errors.New("blob exceeds the storage limit")

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
// it, else read into memory first (ErrTooLarge above the backend's cap).
func PutReader(ctx context.Context, b Blobs, key string, r io.Reader, size int64) error {
	if sp, ok := b.(StreamPutter); ok {
		return sp.PutStream(ctx, key, r, size)
	}
	if max := b.MaxBytes(); max > 0 && size > max {
		return ErrTooLarge
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
