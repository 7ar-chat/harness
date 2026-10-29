package artifacts_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/adapters/store/memstore"
	"github.com/GhaziBenDahmane/harness/features/artifacts"
)

func TestFallbackBlobsReadPostgresContentAfterTheSwitchToS3(t *testing.T) {
	// Blobs written to Postgres before S3 was configured stay readable (Get/Has fall back, read-only); new writes go to
	// the primary only; a delete removes the key from both; unknown keys are still ErrBlobNotFound.
	// Arrange
	ctx := context.Background()
	legacy := artifacts.PGBlobs{S: memstore.New()}
	primary := artifacts.PGBlobs{S: memstore.New()} // stands in for S3
	_ = legacy.Put(ctx, "old", []byte("before the switch"))
	b := artifacts.FallbackBlobs{Primary: primary, Legacy: legacy}

	// Act
	old, oldErr := b.Get(ctx, "old")
	has, _ := b.Has(ctx, "old")
	putErr := artifacts.PutReader(ctx, b, "new", strings.NewReader("after"), 5)
	_, newInLegacy := legacy.Get(ctx, "new")
	fresh, freshErr := b.Get(ctx, "new")
	_, missing := b.Get(ctx, "missing")
	delErr := b.Delete(ctx, "old")
	_, gone := b.Get(ctx, "old")

	// Assert
	if oldErr != nil || !bytes.Equal(old, []byte("before the switch")) || !has {
		t.Fatalf("legacy read %q %v has %v", old, oldErr, has)
	}
	if putErr != nil || freshErr != nil || string(fresh) != "after" || !errors.Is(newInLegacy, artifacts.ErrBlobNotFound) {
		t.Fatalf("put %v fresh %q %v written to legacy: %v", putErr, fresh, freshErr, newInLegacy)
	}
	if !errors.Is(missing, artifacts.ErrBlobNotFound) || delErr != nil || !errors.Is(gone, artifacts.ErrBlobNotFound) {
		t.Fatalf("missing %v delete %v gone %v", missing, delErr, gone)
	}
}
