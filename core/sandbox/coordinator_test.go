package sandbox_test

import (
	"context"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/sandbox"
)

func TestLocalCoordinatorLocksExpireAndOnlyTheHolderReleases(t *testing.T) {
	// Arrange
	ctx := context.Background()
	c := sandbox.NewLocalCoordinator()

	// Act
	first, _ := c.SetNX(ctx, "k", "a", time.Hour)
	second, _ := c.SetNX(ctx, "k", "b", time.Hour)
	renewedByOther, _ := c.Renew(ctx, "k", "b", time.Hour)
	_ = c.Unlock(ctx, "k", "b") // not the holder: no effect
	stillHeld, _ := c.SetNX(ctx, "k", "c", time.Hour)
	_ = c.Unlock(ctx, "k", "a")
	free, _ := c.SetNX(ctx, "k", "c", time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	expired, _ := c.SetNX(ctx, "k", "d", time.Hour)

	// Assert
	if !first || second || renewedByOther || stillHeld || !free || !expired {
		t.Fatalf("first=%v second=%v renewedByOther=%v stillHeld=%v free=%v expired=%v", first, second, renewedByOther, stillHeld, free, expired)
	}
}

func TestLocalCoordinatorCountsAnUnhealthyStreakAndTakesOnce(t *testing.T) {
	// Arrange
	ctx := context.Background()
	c := sandbox.NewLocalCoordinator()

	// Act
	f1, n1, _ := c.Unhealthy(ctx, "s", 1000, time.Hour)
	f2, n2, _ := c.Unhealthy(ctx, "s", 2000, time.Hour)
	_ = c.Set(ctx, "notice", "x", time.Hour)
	took, _ := c.Take(ctx, "notice")
	again, _ := c.Take(ctx, "notice")

	// Assert
	if f1 != 1000 || n1 != 1 || f2 != 1000 || n2 != 2 || took != "x" || again != "" {
		t.Fatalf("%d/%d %d/%d %q %q", f1, n1, f2, n2, took, again)
	}
}
