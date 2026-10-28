package artifacts_test

import (
	"bytes"
	"context"
	"testing"
)

func TestS3PutStreamRoundTripsOnARealBucket(t *testing.T) {
	// On an S3-compatible bucket (HARNESS_TEST_S3_ENDPOINT), streamed content above the multipart threshold round-trips intact.
	// Arrange
	b := s3Blobs(t)
	ctx := context.Background()
	large := bytes.Repeat([]byte("0123456789abcdef"), (17<<20)/16)

	// Act
	err := b.PutStream(ctx, "large", bytes.NewReader(large), int64(len(large)))
	got, getErr := b.Get(ctx, "large")

	// Assert
	if err != nil || getErr != nil || !bytes.Equal(got, large) {
		t.Fatalf("put %v get %v equal %v", err, getErr, bytes.Equal(got, large))
	}
}
