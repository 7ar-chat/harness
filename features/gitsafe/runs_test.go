package gitsafe

import (
	"context"
	"reflect"
	"testing"
)

func TestForRunFindsTheSnapshotTakenBeforeEachRun(t *testing.T) {
	// Arrange: two runs; the second starts on the tree the first left (a new snapshot), a third finds nothing new.
	ctx := context.Background()
	dir := newRepo(t)
	s1, _ := Snapshot(ctx, dir, "th")
	if err := RecordRun(ctx, dir, "th", "run-1", s1); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "a.txt", "a2\n")
	s2, _ := Snapshot(ctx, dir, "th")
	if err := RecordRun(ctx, dir, "th", "run-2", s2); err != nil {
		t.Fatal(err)
	}
	s3, _ := Snapshot(ctx, dir, "th")
	if err := RecordRun(ctx, dir, "th", "run-3", s3); err != nil {
		t.Fatal(err)
	}

	// Act
	got1, err1 := ForRun(ctx, dir, "th", "run-1")
	got2, err2 := ForRun(ctx, dir, "th", "run-2")
	got3, err3 := ForRun(ctx, dir, "th", "run-3")
	_, errMissing := ForRun(ctx, dir, "th", "run-9")

	// Assert
	if err1 != nil || got1.Ref != s1.Ref || err2 != nil || got2.Ref != s2.Ref {
		t.Fatalf("run-1 %+v %v, run-2 %+v %v", got1, err1, got2, err2)
	}
	if err3 != nil || got3.Ref != s2.Ref {
		t.Fatalf("run-3 shares the snapshot of run-2: %+v %v", got3, err3)
	}
	if errMissing == nil {
		t.Fatal("an unknown run has no snapshot")
	}
}

func TestChangedListsThePathsThatDiffer(t *testing.T) {
	// Arrange
	ctx := context.Background()
	dir := newRepo(t)
	before, _ := Snapshot(ctx, dir, "th")
	write(t, dir, "a.txt", "a2\n")
	write(t, dir, "new.txt", "n\n")
	after, _ := Snapshot(ctx, dir, "th")

	// Act
	files, err := Changed(ctx, dir, after.Ref, before.Ref)

	// Assert
	if err != nil || !reflect.DeepEqual(files, []string{"a.txt", "new.txt"}) {
		t.Fatalf("Changed = %v, %v", files, err)
	}
}

func TestDropRemovesTheRunRefs(t *testing.T) {
	// Arrange
	ctx := context.Background()
	dir := newRepo(t)
	s, _ := Snapshot(ctx, dir, "th")
	_ = RecordRun(ctx, dir, "th", "run-1", s)

	// Act
	err := Drop(ctx, dir, "th")

	// Assert
	if refs := run(t, dir, "for-each-ref", "refs/harness/"); err != nil || refs != "" {
		t.Fatalf("Drop = %v, refs left: %q", err, refs)
	}
}
