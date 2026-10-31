package redisx_test

import (
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/redisx"
)

func TestLockIsExclusiveAndOnlyItsHolderCanRenewOrUnlockIt(t *testing.T) {
	// SetNX grants a key to one token; only that token renews or releases it (a stale holder's Unlock/Renew must not touch the new holder's lock); the ttl is honoured.
	// Arrange
	mr, rdb := newRedis(t)
	c := redisx.NewCoordinator(rdb)

	// Act
	won, err := c.SetNX(bg, "lock", "a", time.Minute)
	lost, _ := c.SetNX(bg, "lock", "b", time.Minute)
	foreignRenew, _ := c.Renew(bg, "lock", "b", time.Hour)
	foreignUnlock := c.Unlock(bg, "lock", "b")
	stillHeld, _ := c.Exists(bg, "lock")
	mr.FastForward(40 * time.Second)
	renewed, _ := c.Renew(bg, "lock", "a", time.Minute)
	mr.FastForward(40 * time.Second)
	aliveAfterRenew, _ := c.Exists(bg, "lock")
	unlockErr := c.Unlock(bg, "lock", "a")
	released, _ := c.Exists(bg, "lock")
	renewReleased, _ := c.Renew(bg, "lock", "a", time.Minute)
	reacquired, _ := c.SetNX(bg, "lock", "b", time.Minute)

	// Assert
	if err != nil || !won || lost {
		t.Fatalf("won %v lost %v (%v)", won, lost, err)
	}
	if foreignRenew || foreignUnlock != nil || !stillHeld {
		t.Fatalf("foreign renew %v unlock %v still held %v", foreignRenew, foreignUnlock, stillHeld)
	}
	if !renewed || !aliveAfterRenew || unlockErr != nil || released || renewReleased || !reacquired {
		t.Fatalf("renewed %v alive %v unlock %v released-exists %v renew-after-release %v reacquired %v", renewed, aliveAfterRenew, unlockErr, released, renewReleased, reacquired)
	}
}

func TestSetDelAndTakeBehaveAsOneShotMarks(t *testing.T) {
	// Set stores a value with a ttl, Del reports whether something was removed, and Take returns the value exactly once (two takers never both get it).
	// Arrange
	mr, rdb := newRedis(t)
	c := redisx.NewCoordinator(rdb)
	_ = c.Set(bg, "mark", "payload", time.Minute)
	ttl := mr.TTL("mark")

	// Act
	first, err := c.Take(bg, "mark")
	second, secondErr := c.Take(bg, "mark")
	_ = c.Set(bg, "other", "x", 0)
	deleted, _ := c.Del(bg, "other")
	deletedAgain, _ := c.Del(bg, "other")

	// Assert
	if ttl != time.Minute || err != nil || first != "payload" || secondErr != nil || second != "" {
		t.Fatalf("ttl %v first %q (%v) second %q (%v)", ttl, first, err, second, secondErr)
	}
	if !deleted || deletedAgain {
		t.Fatalf("deleted %v then %v", deleted, deletedAgain)
	}
}

func TestUnhealthyCountsChecksFromTheFirstFailure(t *testing.T) {
	// Each report increments the check count while the first-failure time stays that of the first report; the record expires after ttl, restarting the count.
	// Arrange
	mr, rdb := newRedis(t)
	c := redisx.NewCoordinator(rdb)

	// Act
	first1, checks1, err := c.Unhealthy(bg, "sbx", 1000, time.Minute)
	first2, checks2, _ := c.Unhealthy(bg, "sbx", 5000, time.Minute)
	otherFirst, otherChecks, _ := c.Unhealthy(bg, "other", 7000, time.Minute)
	mr.FastForward(2 * time.Minute)
	first3, checks3, _ := c.Unhealthy(bg, "sbx", 9000, time.Minute)

	// Assert
	if err != nil || first1 != 1000 || checks1 != 1 || first2 != 1000 || checks2 != 2 {
		t.Fatalf("first reports: %d/%d then %d/%d (%v)", first1, checks1, first2, checks2, err)
	}
	if otherFirst != 7000 || otherChecks != 1 || first3 != 9000 || checks3 != 1 {
		t.Fatalf("other key %d/%d, after expiry %d/%d", otherFirst, otherChecks, first3, checks3)
	}
}

func TestCoordinatorOperationsReportAnUnavailableRedis(t *testing.T) {
	// With Redis down, lock acquisition never reads as success and every operation returns the error.
	// Arrange
	mr, rdb := newRedis(t)
	c := redisx.NewCoordinator(rdb)
	mr.SetError("ERR LOADING Redis is loading the dataset in memory")

	// Act
	exists, existsErr := c.Exists(bg, "k")
	won, setnxErr := c.SetNX(bg, "k", "t", time.Minute)
	renewed, renewErr := c.Renew(bg, "k", "t", time.Minute)
	unlockErr := c.Unlock(bg, "k", "t")
	setErr := c.Set(bg, "k", "v", time.Minute)
	deleted, delErr := c.Del(bg, "k")
	taken, takeErr := c.Take(bg, "k")
	first, checks, unhealthyErr := c.Unhealthy(bg, "k", 1, time.Minute)

	// Assert
	if exists || won || renewed || deleted || taken != "" || first != 0 || checks != 0 {
		t.Fatalf("a stopped Redis answered: %v %v %v %v %q %d %d", exists, won, renewed, deleted, taken, first, checks)
	}
	for name, err := range map[string]error{"Exists": existsErr, "SetNX": setnxErr, "Renew": renewErr, "Unlock": unlockErr,
		"Set": setErr, "Del": delErr, "Take": takeErr, "Unhealthy": unhealthyErr} {
		if err == nil {
			t.Errorf("%s returned no error", name)
		}
	}
}
