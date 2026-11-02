package filestore

import (
	"context"
	"testing"
)

func TestSupersedeFromSurvivesARestart(t *testing.T) {
	// Arrange
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)
	threadID, _ := populate(t, s)
	first := must(s.ListMessages(ctx, threadID))[0]

	// Act
	err := s.SupersedeFrom(ctx, "alice", threadID, first.ID)
	ok(t, s.Close())
	s2 := open(t, dir)
	defer s2.Close()

	// Assert
	if msgs := must(s2.ListMessages(ctx, threadID)); err != nil || len(msgs) != 0 {
		t.Fatalf("err %v, %d messages left after a restart", err, len(msgs))
	}
}
