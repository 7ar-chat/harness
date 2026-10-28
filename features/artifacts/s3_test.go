package artifacts_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"

	"github.com/GhaziBenDahmane/harness/features/artifacts"
	"github.com/GhaziBenDahmane/harness/features/artifacts/s3blobs"
	"github.com/GhaziBenDahmane/harness/store"
	"github.com/GhaziBenDahmane/harness/store/memstore"
)

// s3Blobs returns the S3 backend on a fresh bucket of HARNESS_TEST_S3_ENDPOINT (e.g. local RustFS); skipped when unset.
func s3Blobs(t *testing.T) *s3blobs.Blobs {
	t.Helper()
	endpoint := os.Getenv("HARNESS_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("HARNESS_TEST_S3_ENDPOINT not set")
	}
	key, secret := os.Getenv("HARNESS_TEST_S3_ACCESS_KEY"), os.Getenv("HARNESS_TEST_S3_SECRET_KEY")
	bucket := "work-test-" + uuid.NewString()[:8]
	admin := s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String(endpoint), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(key, secret, "")})
	if _, err := admin.CreateBucket(context.Background(), &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatal(err)
	}
	b, err := s3blobs.New(s3blobs.Options{Bucket: bucket, Endpoint: endpoint, AccessKey: key, SecretKey: secret, PathStyle: true, Prefix: "work/"})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestS3BlobsStoreContentAddressedObjects(t *testing.T) {
	// Blobs round-trip through an S3-compatible bucket; a key keeps its first content and unknown keys are not found.
	// Arrange
	b := s3Blobs(t)
	ctx := context.Background()

	// Act
	putErr := b.Put(ctx, "abc", []byte("first"))
	_ = b.Put(ctx, "abc", []byte("second"))
	got, getErr := b.Get(ctx, "abc")
	has, _ := b.Has(ctx, "abc")
	missingHas, _ := b.Has(ctx, "nope")
	_, missing := b.Get(ctx, "nope")

	// Assert
	if putErr != nil || getErr != nil || string(got) != "first" || !has || missingHas || !errors.Is(missing, artifacts.ErrBlobNotFound) {
		t.Fatalf("put %v get %q %v has %v %v missing %v", putErr, got, getErr, has, missingHas, missing)
	}
	if b.MaxBytes() != 0 {
		t.Fatal("S3 has no per-blob cap")
	}
}

func TestS3PurgeDeletesTheBlobsOfPurgedThreadsOnly(t *testing.T) {
	// The retention purge deletes from the bucket the content (and cached preview) of a purged thread's artifacts after the grace period, keeping content still referenced.
	// Arrange
	b := s3Blobs(t)
	ctx := context.Background()
	st := memstore.New()
	clock := time.Now()
	st.Now = func() time.Time { return clock }
	svc := artifacts.New(st, b, nil, artifacts.Options{BlobGCGrace: 24 * time.Hour})
	put := func(th *store.Thread, path, content string) string {
		sum := sha256.Sum256([]byte(content))
		key := hex.EncodeToString(sum[:])
		if err := b.Put(ctx, key, []byte(content)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.PutArtifactVersion(ctx, store.PutArtifact{ThreadID: th.ID, UserLogin: "alice", Path: path, Name: "f",
			MimeType: "text/plain", Kind: store.ArtifactOutput, SizeBytes: int64(len(content)), SHA256: key}); err != nil {
			t.Fatal(err)
		}
		return key
	}
	old, _ := st.CreateThread(ctx, "alice", "old")
	alive, _ := st.CreateThread(ctx, "alice", "alive")
	gone := put(old, "/workspace/a.txt", "only old")
	shared := put(old, "/workspace/b.txt", "shared")
	put(alive, "/workspace/b.txt", "shared")
	_ = b.Put(ctx, "preview/"+gone+".pdf", []byte("%PDF"))
	_, _ = st.DeleteThread(ctx, "alice", old.ID)
	st.SetThreadDeletedAt(old.ID, clock.Add(-48*time.Hour))

	// Act
	threads, early, err := svc.Purge(ctx, 24*time.Hour)
	clock = clock.Add(25 * time.Hour)
	_, blobs, lateErr := svc.Purge(ctx, 24*time.Hour)

	// Assert
	hasGone, _ := b.Has(ctx, gone)
	hasPreview, _ := b.Has(ctx, "preview/"+gone+".pdf")
	hasShared, _ := b.Has(ctx, shared)
	if err != nil || lateErr != nil || threads != 1 || early != 0 || blobs != 1 || hasGone || hasPreview || !hasShared {
		t.Fatalf("err %v %v threads %d blobs %d/%d gone %v preview %v shared %v", err, lateErr, threads, early, blobs, hasGone, hasPreview, hasShared)
	}
}

func TestS3BlobsEnsureBucketCreatesAMissingBucketOnce(t *testing.T) {
	// EnsureBucket creates a missing bucket (local RustFS starts empty) and is a no-op once it exists.
	// Arrange
	endpoint := os.Getenv("HARNESS_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("HARNESS_TEST_S3_ENDPOINT not set")
	}
	key, secret := os.Getenv("HARNESS_TEST_S3_ACCESS_KEY"), os.Getenv("HARNESS_TEST_S3_SECRET_KEY")
	b, err := s3blobs.New(s3blobs.Options{Bucket: "work-test-" + uuid.NewString()[:8], Endpoint: endpoint, AccessKey: key, SecretKey: secret,
		PathStyle: true, Prefix: "work/"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Act
	first := b.EnsureBucket(ctx)
	second := b.EnsureBucket(ctx)
	putErr := b.Put(ctx, "k", []byte("v"))

	// Assert
	if first != nil || second != nil || putErr != nil {
		t.Fatalf("ensure %v then %v, put %v", first, second, putErr)
	}
}
