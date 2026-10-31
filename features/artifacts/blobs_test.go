package artifacts_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/features/artifacts"
)

func TestPGBlobsRefuseContentAboveTheCap(t *testing.T) {
	// The database backend reports its cap and refuses a larger blob, declared (size) or actual (a stream longer than declared), without storing it.
	// Arrange
	ctx := context.Background()
	st := memstore.New()
	b := artifacts.PGBlobs{S: st}
	big := bytes.Repeat([]byte("x"), artifacts.PGBlobMax+1)

	// Act
	putErr := b.Put(ctx, "big", big)
	declared := b.PutStream(ctx, "declared", strings.NewReader("tiny"), artifacts.PGBlobMax+1)
	lying := b.PutStream(ctx, "lying", bytes.NewReader(big), 10)
	has, _ := b.Has(ctx, "big")

	// Assert
	if b.MaxBytes() != artifacts.PGBlobMax {
		t.Fatalf("cap %d", b.MaxBytes())
	}
	if !errors.Is(putErr, artifacts.ErrBlobTooLarge) || !errors.Is(declared, artifacts.ErrBlobTooLarge) || !errors.Is(lying, artifacts.ErrBlobTooLarge) || has {
		t.Fatalf("put %v declared %v lying %v stored %v", putErr, declared, lying, has)
	}
}

// brokenReader fails after its data.
type brokenReader struct{ err error }

func (b brokenReader) Read([]byte) (int, error) { return 0, b.err }

func TestPGBlobsStreamReadFailureIsReportedAndNothingStored(t *testing.T) {
	// A stream that fails midway leaves no blob behind.
	// Arrange
	ctx := context.Background()
	b := artifacts.PGBlobs{S: memstore.New()}
	boom := errors.New("connection reset")

	// Act
	err := b.PutStream(ctx, "k", io.MultiReader(strings.NewReader("part"), brokenReader{boom}), 100)
	has, _ := b.Has(ctx, "k")

	// Assert
	if !errors.Is(err, boom) || has {
		t.Fatalf("err %v stored %v", err, has)
	}
}

func TestPGBlobsMissingKeyIsErrBlobNotFoundAndDeleteRemoves(t *testing.T) {
	// Get maps the store's not-found to ErrBlobNotFound; Delete removes content.
	// Arrange
	ctx := context.Background()
	b := artifacts.PGBlobs{S: memstore.New()}
	_ = b.Put(ctx, "k", []byte("v"))

	// Act
	_, missing := b.Get(ctx, "nope")
	delErr := b.Delete(ctx, "k")
	_, afterDelete := b.Get(ctx, "k")

	// Assert
	if !errors.Is(missing, artifacts.ErrBlobNotFound) || delErr != nil || !errors.Is(afterDelete, artifacts.ErrBlobNotFound) {
		t.Fatalf("missing %v delete %v after %v", missing, delErr, afterDelete)
	}
}

func TestFallbackBlobsWriteLimitsAndErrorsFollowThePrimary(t *testing.T) {
	// The fallback reports and enforces the primary's cap, a Has miss on both is false, and without a legacy backend a miss stays a miss.
	// Arrange
	ctx := context.Background()
	primary := capBlobs{hookBlobs: newHookBlobs(), max: 5}
	legacy := newHookBlobs()
	_ = legacy.Put(ctx, "old", []byte("legacy"))
	b := artifacts.FallbackBlobs{Primary: primary, Legacy: legacy}
	alone := artifacts.FallbackBlobs{Primary: newHookBlobs()}

	// Act
	hasOld, _ := b.Has(ctx, "old")
	hasNone, _ := b.Has(ctx, "none")
	putErr := b.Put(ctx, "new", []byte("abc"))
	got, getErr := b.Get(ctx, "new")
	_, aloneMiss := alone.Get(ctx, "x")
	aloneHas, _ := alone.Has(ctx, "x")
	aloneDel := alone.Delete(ctx, "x")

	// Assert
	if b.MaxBytes() != 5 || !hasOld || hasNone {
		t.Fatalf("max %d has old %v none %v", b.MaxBytes(), hasOld, hasNone)
	}
	if putErr != nil || getErr != nil || string(got) != "abc" {
		t.Fatalf("put %v get %q %v", putErr, got, getErr)
	}
	if !errors.Is(aloneMiss, artifacts.ErrBlobNotFound) || aloneHas || aloneDel != nil {
		t.Fatalf("alone: get %v has %v delete %v", aloneMiss, aloneHas, aloneDel)
	}
}

func TestFallbackBlobsDeleteReportsAFailureOfEitherBackend(t *testing.T) {
	// A delete that fails on the primary or on the legacy backend is reported (and still attempted on the other).
	// Arrange
	ctx := context.Background()
	boom := errors.New("delete refused")
	legacy := &failBlobs{hookBlobs: newHookBlobs(), deleteErr: boom}
	primary := newHookBlobs()
	_ = primary.Put(ctx, "k", []byte("v"))
	_ = legacy.hookBlobs.Put(ctx, "k", []byte("v"))
	b := artifacts.FallbackBlobs{Primary: primary, Legacy: legacy}

	// Act
	err := b.Delete(ctx, "k")

	// Assert
	if !errors.Is(err, boom) || primary.has("k") {
		t.Fatalf("err %v primary still has the key %v", err, primary.has("k"))
	}

	// Arrange
	primaryFail := &failBlobs{hookBlobs: newHookBlobs(), deleteErr: boom}
	legacyOK := newHookBlobs()
	_ = legacyOK.Put(ctx, "k", []byte("v"))
	b = artifacts.FallbackBlobs{Primary: primaryFail, Legacy: legacyOK}

	// Act
	err = b.Delete(ctx, "k")

	// Assert
	if !errors.Is(err, boom) || legacyOK.has("k") {
		t.Fatalf("primary failure: err %v legacy still has the key %v", err, legacyOK.has("k"))
	}
}

func TestFallbackBlobsHasAndGetSurfaceAPrimaryError(t *testing.T) {
	// A primary that fails (not a miss) is not masked by the legacy backend.
	// Arrange
	ctx := context.Background()
	boom := errors.New("s3 unreachable")
	legacy := newHookBlobs()
	_ = legacy.Put(ctx, "k", []byte("old"))
	b := artifacts.FallbackBlobs{Primary: &failBlobs{hookBlobs: newHookBlobs(), getErr: boom, hasErr: boom}, Legacy: legacy}

	// Act
	_, getErr := b.Get(ctx, "k")
	has, hasErr := b.Has(ctx, "k")

	// Assert
	if !errors.Is(getErr, boom) || !errors.Is(hasErr, boom) || has {
		t.Fatalf("get %v has %v %v", getErr, has, hasErr)
	}
}
