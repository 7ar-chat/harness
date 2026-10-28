package pgstore_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/GhaziBenDahmane/harness/store"
)

func TestPGRendersMergePagesAndStayInTheirThread(t *testing.T) {
	// A render recorded twice keeps the union of its pages and the latest label; it is only found in its own thread (review round 1 P2 #2).
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "")
	other, _ := s.CreateThread(ctx, "alice", "")
	_ = s.PutRender(ctx, &store.Render{ThreadID: th.ID, RenderID: "memo-0123456789", SourcePath: "/workspace/memo.pdf", Label: "C2", Allowed: true, Pages: []int{3, 1}})

	// Act
	err := s.PutRender(ctx, &store.Render{ThreadID: th.ID, RenderID: "memo-0123456789", SourcePath: "/workspace/memo.pdf", Label: "C4 - Secret", Allowed: false, Pages: []int{2, 3}})

	// Assert
	got, getErr := s.GetRender(ctx, th.ID, "memo-0123456789")
	_, otherErr := s.GetRender(ctx, other.ID, "memo-0123456789")
	if err != nil || getErr != nil || !slices.Equal(got.Pages, []int{1, 2, 3}) || got.Label != "C4 - Secret" || got.Allowed ||
		got.SourcePath != "/workspace/memo.pdf" || got.CreatedAt.IsZero() || !errors.Is(otherErr, store.ErrNotFound) {
		t.Fatalf("err %v get %v %+v other %v", err, getErr, got, otherErr)
	}
}
