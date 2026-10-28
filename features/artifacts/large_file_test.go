package artifacts_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/GhaziBenDahmane/harness/engine/agui"
	"github.com/GhaziBenDahmane/harness/features/artifacts"
	"github.com/GhaziBenDahmane/harness/sandbox"
	"github.com/GhaziBenDahmane/harness/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/store"
	"github.com/GhaziBenDahmane/harness/store/memstore"
)

func TestSnapshotSaysOnceWhenAFileIsTooLargeToSave(t *testing.T) {
	// A workspace file above the artifact size limit is not versioned and the run is told so once (work.artifact_skipped), not silently dropped (5.8).
	// Arrange
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	st := memstore.New()
	backend := sandboxtest.NewBackend()
	mgr := sandbox.NewManager(backend, st, rdb, sandbox.ManagerOptions{MaxPerUser: 5, Timeout: time.Hour})
	svc := artifacts.New(st, artifacts.PGBlobs{S: st}, mgr, artifacts.Options{MaxFileBytes: 1024})
	th, _ := st.CreateThread(ctx, "alice", "t")
	if _, err := mgr.Acquire(ctx, th.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	sb := backend.Created()[0]
	sb.SetExec(sandboxtest.FindExec(sb, nil))
	sb.Put("/workspace/outputs/big.csv", strings.Repeat("x", 2048))
	sb.Put("/workspace/outputs/small.csv", "a,b")
	run := &store.Run{ID: "r1", ThreadID: th.ID, UserLogin: "alice"}
	var events []agui.Event
	emit := func(ev agui.Event) error { events = append(events, ev); return nil }

	// Act
	first := svc.SnapshotRun(ctx, run, "call-1", emit)
	again := svc.SnapshotRun(ctx, run, "call-2", emit)

	// Assert
	var skipped []map[string]any
	for _, ev := range events {
		if ev["name"] == artifacts.CustomArtifactSkipped {
			skipped = append(skipped, ev["value"].(map[string]any))
		}
	}
	if len(first) != 1 || first[0].Artifact.Path != "/workspace/outputs/small.csv" || len(again) != 0 {
		t.Fatalf("changes %+v then %+v", first, again)
	}
	if len(skipped) != 1 || skipped[0]["path"] != "/workspace/outputs/big.csv" || skipped[0]["reason"] != "too_large" ||
		skipped[0]["sizeBytes"] != int64(2048) || skipped[0]["maxBytes"] != int64(1024) || skipped[0]["toolCallId"] != "call-1" {
		t.Fatalf("skipped events %+v", skipped)
	}
}
