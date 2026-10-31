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

func TestLocalCoordinatorDelAndExistsHonourExpiryAndResetTheStreak(t *testing.T) {
	// Arrange
	ctx := context.Background()
	c := sandbox.NewLocalCoordinator()
	_ = c.Set(ctx, "marker", "1", time.Hour)
	_ = c.Set(ctx, "short", "1", time.Millisecond)
	_, _, _ = c.Unhealthy(ctx, "streak", 1000, time.Hour)
	_, _, _ = c.Unhealthy(ctx, "streak", 2000, time.Hour)
	time.Sleep(5 * time.Millisecond)

	// Act
	has, _ := c.Exists(ctx, "marker")
	expired, _ := c.Exists(ctx, "short")
	missing, _ := c.Exists(ctx, "nope")
	existed, _ := c.Del(ctx, "marker")
	again, _ := c.Del(ctx, "marker")
	expiredDel, _ := c.Del(ctx, "short")
	delStreak, _ := c.Del(ctx, "streak") // the streak is not a value: nothing existed
	first, checks, _ := c.Unhealthy(ctx, "streak", 3000, time.Hour)

	// Assert
	if !has || expired || missing {
		t.Errorf("Exists: marker %v expired %v missing %v", has, expired, missing)
	}
	if !existed || again || expiredDel || delStreak {
		t.Errorf("Del: existed %v again %v expired %v streak %v", existed, again, expiredDel, delStreak)
	}
	if first != 3000 || checks != 1 {
		t.Errorf("a healthy check must end the streak: restarted at %d with %d checks", first, checks)
	}
}

func TestLocalCoordinatorRenewKeepsALockAliveAndAnExpiredStreakRestarts(t *testing.T) {
	// Arrange
	ctx := context.Background()
	c := sandbox.NewLocalCoordinator()
	_, _ = c.SetNX(ctx, "lock", "a", 20*time.Millisecond)

	// Act
	renewed, _ := c.Renew(ctx, "lock", "a", time.Hour)
	time.Sleep(40 * time.Millisecond)
	stillHeld, _ := c.SetNX(ctx, "lock", "b", time.Hour)
	_, _, _ = c.Unhealthy(ctx, "s", 1000, 10*time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	first, checks, _ := c.Unhealthy(ctx, "s", 9000, time.Hour)
	renewExpired, _ := c.Renew(ctx, "gone", "a", time.Hour)

	// Assert
	if !renewed || stillHeld {
		t.Errorf("renewed %v, lock taken by another while renewed: %v", renewed, stillHeld)
	}
	if first != 9000 || checks != 1 {
		t.Errorf("an expired streak must restart: %d/%d", first, checks)
	}
	if renewExpired {
		t.Error("a lock that was never taken was renewed")
	}
}
