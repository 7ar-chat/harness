// Package s3blobs is the S3-compatible artifacts.Blobs backend (Dell ECS, RustFS, AWS).
// It lives apart from artifacts so the AWS SDK is linked only by hosts that use it.
package s3blobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/GhaziBenDahmane/harness/artifacts"
)

// Options configures the S3-compatible backend (Dell ECS, RustFS, AWS).
type Options struct {
	Bucket    string
	Endpoint  string // empty: AWS default endpoint
	Region    string
	AccessKey string
	SecretKey string
	PathStyle bool
	// Prefix is prepended to every key (e.g. "work/").
	Prefix string
}

// Blobs stores blobs as S3 objects (streamed content goes through the
// transfer manager: multipart uploads of 8 MiB parts above 16 MiB).
type Blobs struct {
	client   *s3.Client
	transfer *transfermanager.Client
	bucket   string
	prefix   string
}

// New builds the S3 backend with static credentials.
func New(o Options) (*Blobs, error) {
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
	return &Blobs{client: client, transfer: transfer, bucket: o.Bucket, prefix: o.Prefix}, nil
}

func (b *Blobs) MaxBytes() int64 { return 0 }

// EnsureBucket creates the bucket when it does not exist (local S3 stores such
// as RustFS start empty); a bucket created meanwhile by another process is fine.
func (b *Blobs) EnsureBucket(ctx context.Context) error {
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

func (b *Blobs) key(k string) *string { return aws.String(b.prefix + k) }

func (b *Blobs) Put(ctx context.Context, key string, data []byte) error {
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
func (b *Blobs) PutStream(ctx context.Context, key string, r io.Reader, size int64) error {
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

func (b *Blobs) Get(ctx context.Context, key string) ([]byte, error) {
	out, err := b.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(b.bucket), Key: b.key(key)})
	if err != nil {
		if isS3NotFound(err) {
			return nil, artifacts.ErrBlobNotFound
		}
		return nil, fmt.Errorf("s3 get %s: %w", key, err)
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

func (b *Blobs) Has(ctx context.Context, key string) (bool, error) {
	_, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(b.bucket), Key: b.key(key)})
	if err == nil {
		return true, nil
	}
	if isS3NotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("s3 head %s: %w", key, err)
}

func (b *Blobs) Delete(ctx context.Context, key string) error {
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
