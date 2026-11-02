package redisx_test

import (
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/redisx"
)

func TestSkillInstallsRecordRevisionsPerSandboxAndExpire(t *testing.T) {
	// Recorded revisions are read back per sandbox and skill, a later record updates only the skills it names, unknown sandboxes/skills read as "" and the whole record expires after ttl.
	// Arrange
	mr, rdb := newRedis(t)
	s := redisx.NewSkillInstalls(rdb)

	// Act
	err := s.Record(bg, "sbx-1", map[string]string{"pdf": "r1", "xlsx": "r1"}, time.Hour)
	_ = s.Record(bg, "sbx-1", map[string]string{"pdf": "r2"}, time.Hour)
	pdf, _ := s.Revision(bg, "sbx-1", "pdf")
	xlsx, _ := s.Revision(bg, "sbx-1", "xlsx")
	missingSkill, missingErr := s.Revision(bg, "sbx-1", "docx")
	otherSandbox, _ := s.Revision(bg, "sbx-2", "pdf")
	ttl := mr.TTL("work:skills:installed:sbx-1")
	mr.FastForward(2 * time.Hour)
	expired, _ := s.Revision(bg, "sbx-1", "pdf")

	// Assert
	if err != nil || pdf != "r2" || xlsx != "r1" || missingSkill != "" || missingErr != nil || otherSandbox != "" {
		t.Fatalf("pdf %q xlsx %q missing %q (%v) other %q (%v)", pdf, xlsx, missingSkill, missingErr, otherSandbox, err)
	}
	if ttl != time.Hour || expired != "" {
		t.Fatalf("ttl %v, after expiry %q", ttl, expired)
	}
}

func TestSkillInstallLocksAreExclusiveAndReleasedByTheirTokenOnly(t *testing.T) {
	// A lock is held by one token; a stranger's Unlock leaves it in place, the holder's frees it, and it frees itself after its ttl.
	// Arrange
	mr, rdb := newRedis(t)
	s := redisx.NewSkillInstalls(rdb)

	// Act
	a, err := s.TryLock(bg, "lock:sbx", "tok-a", time.Minute)
	b, _ := s.TryLock(bg, "lock:sbx", "tok-b", time.Minute)
	foreign := s.Unlock(bg, "lock:sbx", "tok-b")
	stillLocked, _ := s.TryLock(bg, "lock:sbx", "tok-b", time.Minute)
	own := s.Unlock(bg, "lock:sbx", "tok-a")
	afterOwn, _ := s.TryLock(bg, "lock:sbx", "tok-b", time.Minute)
	mr.FastForward(2 * time.Minute)
	afterExpiry, _ := s.TryLock(bg, "lock:sbx", "tok-c", time.Minute)

	// Assert
	if err != nil || !a || b || foreign != nil || stillLocked || own != nil || !afterOwn || !afterExpiry {
		t.Fatalf("a %v b %v foreign %v stillLocked %v own %v afterOwn %v afterExpiry %v (%v)", a, b, foreign, stillLocked, own, afterOwn, afterExpiry, err)
	}
}

func TestSkillInstallsReportAnUnavailableRedis(t *testing.T) {
	// A Redis outage is an error everywhere: a missing revision must not be confused with "not installed", nor a failed lock with a held one.
	// Arrange
	mr, rdb := newRedis(t)
	s := redisx.NewSkillInstalls(rdb)
	mr.SetError("ERR LOADING Redis is loading the dataset in memory")

	// Act
	rev, revErr := s.Revision(bg, "sbx", "pdf")
	recErr := s.Record(bg, "sbx", map[string]string{"pdf": "r1"}, time.Hour)
	got, lockErr := s.TryLock(bg, "k", "t", time.Minute)
	unlockErr := s.Unlock(bg, "k", "t")

	// Assert
	if revErr == nil || rev != "" || recErr == nil || lockErr == nil || got || unlockErr == nil {
		t.Fatalf("revision %v, record %v, lock %v/%v, unlock %v", revErr, recErr, got, lockErr, unlockErr)
	}
}
