package redisx_test

import (
	"sort"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/redisx"
)

func TestJobsKeepTheFirstRecordedStartAndListWhatIsRunning(t *testing.T) {
	// Registering a job twice keeps its first start (a second process must not reset it); listing returns every running job with its thread, and removed jobs disappear.
	// Arrange
	_, rdb := newRedis(t)
	jobs := redisx.NewJobs(rdb)
	first, err := jobs.Add(bg, "t1", "build")
	time.Sleep(2 * time.Millisecond) // the second Add must be later than the first by at least one millisecond
	second, _ := jobs.Add(bg, "t1", "build")
	_, _ = jobs.Add(bg, "t2", "serve")

	// Act
	listed, listErr := jobs.List(bg)
	removeErr := jobs.Remove(bg, "t1", "build")
	left, _ := jobs.List(bg)

	// Assert
	if err != nil || !second.Equal(first) {
		t.Fatalf("first %v second %v err %v", first, second, err)
	}
	var ids []string
	for _, j := range listed {
		ids = append(ids, j.ThreadID+"/"+j.JobID)
		if j.ThreadID == "t1" && !j.Started.Equal(first) {
			t.Errorf("listed start %v, want %v", j.Started, first)
		}
	}
	sort.Strings(ids)
	if listErr != nil || len(ids) != 2 || ids[0] != "t1/build" || ids[1] != "t2/serve" {
		t.Fatalf("listed %v (%v)", ids, listErr)
	}
	if removeErr != nil || len(left) != 1 || left[0].JobID != "serve" {
		t.Fatalf("after remove: %v (%v)", left, removeErr)
	}
}

func TestJobsReadStartsWrittenInSecondsByThePreviousRelease(t *testing.T) {
	// Entries of the previous release hold unix seconds; they are read as seconds, not as a 1970 millisecond date. Unparseable or non-positive values and keys without a thread are not trusted blindly.
	// Arrange
	_, rdb := newRedis(t)
	jobs := redisx.NewJobs(rdb)
	rdb.HSet(bg, "work:sbx:jobs", "old/job", "1700000000", "new/job", "1700000000123", "junk/job", "soon", "zero/job", "0", "nothread", "1700000000000")
	before := time.Now()

	// Act
	listed, err := jobs.List(bg)
	again, _ := jobs.Add(bg, "old", "job")

	// Assert
	starts := map[string]time.Time{}
	for _, j := range listed {
		starts[j.ThreadID+"/"+j.JobID] = j.Started
	}
	if err != nil || len(starts) != 4 {
		t.Fatalf("listed %v (%v)", starts, err)
	}
	if !starts["old/job"].Equal(time.Unix(1700000000, 0)) || !again.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("seconds entry read as %v / %v", starts["old/job"], again)
	}
	if !starts["new/job"].Equal(time.UnixMilli(1700000000123)) {
		t.Errorf("milliseconds entry read as %v", starts["new/job"])
	}
	for _, k := range []string{"junk/job", "zero/job"} {
		if starts[k].Before(before.Add(-time.Second)) || starts[k].After(time.Now().Add(time.Second)) {
			t.Errorf("%s: unreadable start must fall back to now, got %v", k, starts[k])
		}
	}
}

func TestJobLeasesAreExclusiveRenewableAndReleasableByTheirOwnerOnly(t *testing.T) {
	// One process holds a job's lease at a time; its owner renews it, others are refused until it is released by the owner (a release by anyone else does nothing) or expires.
	// Arrange
	mr, rdb := newRedis(t)
	jobs := redisx.NewJobs(rdb)

	// Act
	a, errA := jobs.Claim(bg, "t", "j", "proc-a", time.Minute)
	b, errB := jobs.Claim(bg, "t", "j", "proc-b", time.Minute)
	mr.FastForward(40 * time.Second)
	renewed, _ := jobs.Claim(bg, "t", "j", "proc-a", time.Minute)
	mr.FastForward(40 * time.Second) // 80 s after the first claim: alive only because of the renewal
	stillA, _ := jobs.Claim(bg, "t", "j", "proc-b", time.Minute)
	foreignRelease := jobs.Release(bg, "t", "j", "proc-b")
	afterForeign, _ := jobs.Claim(bg, "t", "j", "proc-b", time.Minute)
	ownRelease := jobs.Release(bg, "t", "j", "proc-a")
	afterOwn, _ := jobs.Claim(bg, "t", "j", "proc-b", time.Minute)
	mr.FastForward(2 * time.Minute)
	afterExpiry, _ := jobs.Claim(bg, "t", "j", "proc-c", time.Minute)
	otherJob, _ := jobs.Claim(bg, "t", "other", "proc-d", time.Minute)
	missingRelease := jobs.Release(bg, "t", "never-claimed", "proc-a")

	// Assert
	if errA != nil || errB != nil || !a || b {
		t.Fatalf("first claims: a=%v b=%v (%v %v)", a, b, errA, errB)
	}
	if !renewed || stillA || afterForeign {
		t.Fatalf("renewed %v, rival while renewed %v, after a foreign release %v", renewed, stillA, afterForeign)
	}
	if foreignRelease != nil || ownRelease != nil || !afterOwn {
		t.Fatalf("release errors %v %v, claim after the owner's release %v", foreignRelease, ownRelease, afterOwn)
	}
	if !afterExpiry || !otherJob || missingRelease != nil {
		t.Fatalf("after expiry %v, other job %v, release of an unclaimed job %v", afterExpiry, otherJob, missingRelease)
	}
}

func TestJobLeaseWithATinyTTLStillExpires(t *testing.T) {
	// A sub-millisecond ttl is rounded up to 1 ms instead of being rejected by Redis or becoming a lease that never expires.
	// Arrange
	mr, rdb := newRedis(t)
	jobs := redisx.NewJobs(rdb)

	// Act
	ok, err := jobs.Claim(bg, "t", "j", "proc-a", time.Microsecond)
	ttl := mr.TTL("work:sbx:job-lease:t/j")

	// Assert
	if err != nil || !ok || ttl <= 0 || ttl > time.Second {
		t.Fatalf("claim %v (%v) ttl %v", ok, err, ttl)
	}
}

func TestJobsReportAnUnavailableRedis(t *testing.T) {
	// Every job operation returns the connection error instead of an empty answer: a restarted runtime must not conclude "no jobs".
	// Arrange
	mr, rdb := newRedis(t)
	jobs := redisx.NewJobs(rdb)
	mr.SetError("ERR LOADING Redis is loading the dataset in memory")

	// Act
	_, addErr := jobs.Add(bg, "t", "j")
	listed, listErr := jobs.List(bg)
	removeErr := jobs.Remove(bg, "t", "j")
	claimed, claimErr := jobs.Claim(bg, "t", "j", "o", time.Minute)
	releaseErr := jobs.Release(bg, "t", "j", "o")

	// Assert
	if addErr == nil || listErr == nil || listed != nil || removeErr == nil || claimErr == nil || claimed || releaseErr == nil {
		t.Fatalf("add %v list %v/%v remove %v claim %v/%v release %v", addErr, listed, listErr, removeErr, claimed, claimErr, releaseErr)
	}
}
