// Package artifacts makes the files the agent produces under /workspace
// durable: snapshots after file-changing tools (and at run end), versioned and
// sha256-deduplicated content in a blob store (S3-compatible or Postgres),
// workspace restore on sandbox (re)creation, classified downloads, Office → PDF
// previews and user uploads.
package artifacts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

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

// S3Options configures the S3-compatible backend (Dell ECS, RustFS, AWS).
type S3Options struct {
	Bucket    string
	Endpoint  string // empty: AWS default endpoint
	Region    string
	AccessKey string
	SecretKey string
	PathStyle bool
	// Prefix is prepended to every key (e.g. "work/").
	Prefix string
}

// S3Blobs stores blobs as S3 objects (streamed content goes through the
// transfer manager: multipart uploads of 8 MiB parts above 16 MiB).
type S3Blobs struct {
	client   *s3.Client
	transfer *transfermanager.Client
	bucket   string
	prefix   string
}

// NewS3 builds the S3 backend with static credentials.
func NewS3(o S3Options) (*S3Blobs, error) {
	if o.Bucket == "" {
		return nil, errors.New("ARTIFACTS_S3_BUCKET is empty")
	}
	region := o.Region
	if region == "" {
		region = "us-east-1"
	}
	opts := s3.Options{
		Region:       region,
		UsePathStyle: o.PathStyle,
		// ECS and some S3-compatible stores do not support the newer default integrity checksums.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}
	if o.AccessKey != "" {
		opts.Credentials = credentials.NewStaticCredentialsProvider(o.AccessKey, o.SecretKey, "")
	}
	if o.Endpoint != "" {
		opts.BaseEndpoint = aws.String(strings.TrimRight(o.Endpoint, "/"))
	}
	client := s3.New(opts)
	transfer := transfermanager.New(client, func(t *transfermanager.Options) {
		t.Concurrency = 3 // ≤ (3+1) × 8 MiB part buffers per upload
		// Same as the client: ECS and some S3-compatible stores do not support the default CRC32 checksums.
		t.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
	})
	return &S3Blobs{client: client, transfer: transfer, bucket: o.Bucket, prefix: o.Prefix}, nil
}

func (b *S3Blobs) MaxBytes() int64 { return 0 }

// EnsureBucket creates the bucket when it does not exist (local S3 stores such
// as RustFS start empty); a bucket created meanwhile by another process is fine.
func (b *S3Blobs) EnsureBucket(ctx context.Context) error {
	_, err := b.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(b.bucket)})
	if err == nil {
		return nil
	}
	var notFound *types.NotFound
	var noBucket *types.NoSuchBucket
	if !errors.As(err, &notFound) && !errors.As(err, &noBucket) {
		return fmt.Errorf("s3 head bucket %s: %w", b.bucket, err)
	}
	_, err = b.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(b.bucket)})
	var owned *types.BucketAlreadyOwnedByYou
	var exists *types.BucketAlreadyExists
	if err != nil && !errors.As(err, &owned) && !errors.As(err, &exists) {
		return fmt.Errorf("s3 create bucket %s: %w", b.bucket, err)
	}
	return nil
}

func (b *S3Blobs) key(k string) *string { return aws.String(b.prefix + k) }

func (b *S3Blobs) Put(ctx context.Context, key string, data []byte) error {
	if ok, err := b.Has(ctx, key); err == nil && ok {
		return nil // content-addressed: already stored
	}
	_, err := b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(b.bucket), Key: b.key(key), Body: bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))), ContentType: aws.String("application/octet-stream"),
	})
	if err != nil {
		return fmt.Errorf("s3 put %s: %w", key, err)
	}
	return nil
}

// PutStream implements StreamPutter with the S3 transfer manager (one
// PutObject below 16 MiB, a multipart upload above; parts are buffered, the
// whole content never is).
func (b *S3Blobs) PutStream(ctx context.Context, key string, r io.Reader, size int64) error {
	if ok, err := b.Has(ctx, key); err == nil && ok {
		return nil // content-addressed: already stored
	}
	_, err := b.transfer.UploadObject(ctx, &transfermanager.UploadObjectInput{
		Bucket: aws.String(b.bucket), Key: b.key(key), Body: r,
		ContentLength: aws.Int64(size), ContentType: aws.String("application/octet-stream"),
	})
	if err != nil {
		return fmt.Errorf("s3 upload %s: %w", key, err)
	}
	return nil
}

func (b *S3Blobs) Get(ctx context.Context, key string) ([]byte, error) {
	out, err := b.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(b.bucket), Key: b.key(key)})
	if err != nil {
		if isS3NotFound(err) {
			return nil, ErrBlobNotFound
		}
		return nil, fmt.Errorf("s3 get %s: %w", key, err)
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

func (b *S3Blobs) Has(ctx context.Context, key string) (bool, error) {
	_, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(b.bucket), Key: b.key(key)})
	if err == nil {
		return true, nil
	}
	if isS3NotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("s3 head %s: %w", key, err)
}

func (b *S3Blobs) Delete(ctx context.Context, key string) error {
	_, err := b.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(b.bucket), Key: b.key(key)})
	if err != nil && !isS3NotFound(err) {
		return fmt.Errorf("s3 delete %s: %w", key, err)
	}
	return nil
}

func isS3NotFound(err error) bool {
	var nsk *types.NoSuchKey
	var nf *types.NotFound
	if errors.As(err, &nsk) || errors.As(err, &nf) {
		return true
	}
	var api smithy.APIError
	return errors.As(err, &api) && (api.ErrorCode() == "NotFound" || api.ErrorCode() == "NoSuchKey")
}
