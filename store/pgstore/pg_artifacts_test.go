package pgstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/GhaziBenDahmane/harness/store"
)

func TestPGArtifactVersionsAreDeduplicatedAndUndeleted(t *testing.T) {
	// Same content only refreshes the mtime, new content or Force adds a version, a deleted artifact comes back on change.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "")
	run, _, _ := s.CreateRun(ctx, newRun(th.ID, "go"))
	put := func(sha string, mtime float64, force bool) (*store.Artifact, string) {
		a, change, err := s.PutArtifactVersion(ctx, store.PutArtifact{ThreadID: th.ID, UserLogin: "alice", Path: "/workspace/a.txt",
			Name: "a.txt", MimeType: "text/plain", Kind: store.ArtifactOutput, SizeBytes: int64(len(sha)), SHA256: sha, RunID: run.ID,
			Mtime: mtime, Force: force, SandboxID: "sb-1"})
		if err != nil {
			t.Fatal(err)
		}
		return a, change
	}

	// Act
	_, c1 := put("aaa", 1, false)
	same, c2 := put("aaa", 2, false)
	_, c3 := put("bbbb", 3, false)
	deleted, _ := s.MarkArtifactDeleted(ctx, same.ID, "", "sb-1", time.Now().Add(time.Hour))
	back, c4 := put("bbbb", 4, false)
	forced, c5 := put("bbbb", 5, true)

	// Assert
	if c1 != store.ChangeCreated || c2 != "" || same.SourceMtime != 2 || same.Version != 1 || c3 != store.ChangeUpdated {
		t.Fatalf("changes %q %q %q, %+v", c1, c2, c3, same)
	}
	if !deleted.Deleted || c4 != store.ChangeUpdated || back.Deleted || back.Version != 3 || c5 != store.ChangeUpdated || forced.Version != 4 {
		t.Fatalf("deleted %+v back %q %+v forced %q %+v", deleted, c4, back, c5, forced)
	}
	versions, _ := s.ListArtifactVersions(ctx, same.ID)
	v1, _ := s.GetArtifactVersion(ctx, same.ID, 1)
	if len(versions) != 4 || versions[0].Version != 4 || v1.SHA256 != "aaa" || v1.RunID != run.ID {
		t.Fatalf("versions %+v v1 %+v", versions, v1)
	}
	if _, err := s.GetArtifactVersion(ctx, same.ID, 9); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing version: %v", err)
	}
}

func TestPGArtifactsOfDeletedThreadsAreHidden(t *testing.T) {
	// Listing orders live artifacts first; a deleted thread's artifacts are no longer readable by id.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "")
	for _, p := range []string{"/workspace/a", "/workspace/b"} {
		if _, _, err := s.PutArtifactVersion(ctx, store.PutArtifact{ThreadID: th.ID, UserLogin: "alice", Path: p, Name: p[11:],
			MimeType: "text/plain", Kind: store.ArtifactUpload, SHA256: "x", SandboxID: "sb-1"}); err != nil {
			t.Fatal(err)
		}
	}
	list, _ := s.ListArtifacts(ctx, th.ID)
	_, _ = s.MarkArtifactDeleted(ctx, list[1].ID, "", "sb-1", time.Now().Add(time.Hour))
	ordered, _ := s.ListArtifacts(ctx, th.ID)

	// Act
	_, _ = s.DeleteThread(ctx, "alice", th.ID)
	_, err := s.GetArtifact(ctx, list[0].ID)

	// Assert
	if len(ordered) != 2 || ordered[0].Deleted || !ordered[1].Deleted || ordered[0].Kind != store.ArtifactUpload {
		t.Fatalf("ordered %+v", ordered)
	}
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("artifact of deleted thread: %v", err)
	}
}

func TestPGBlobsAreContentAddressed(t *testing.T) {
	// A blob key keeps its first content; unknown keys are not found.
	// Arrange
	s := newPG(t)
	ctx := context.Background()

	// Act
	_ = s.PutBlob(ctx, "k1", []byte("first"))
	_ = s.PutBlob(ctx, "k1", []byte("second"))
	got, err := s.GetBlob(ctx, "k1")
	has, _ := s.HasBlob(ctx, "k1")
	_, missing := s.GetBlob(ctx, "nope")

	// Assert
	if err != nil || string(got) != "first" || !has || !errors.Is(missing, store.ErrNotFound) {
		t.Fatalf("got %q %v %v %v", got, err, has, missing)
	}
}

func TestPGRunSkills(t *testing.T) {
	// The skills requested at run creation persist; an unknown run is not found.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "")
	in := newRun(th.ID, "go")
	in.Skills = []string{"docx", "my-report"}

	// Act
	run, _, err := s.CreateRun(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	runSkills, _ := s.RunSkills(ctx, run.ID)
	_, unknown := s.RunSkills(ctx, uuid.NewString())

	// Assert
	if len(runSkills) != 2 || runSkills[0] != "docx" || runSkills[1] != "my-report" {
		t.Fatalf("run skills %v", runSkills)
	}
	if !errors.Is(unknown, store.ErrNotFound) {
		t.Fatalf("unknown run: %v", unknown)
	}
}

func TestPGAppendRunArtifactsGoesToTheLastAssistantMessage(t *testing.T) {
	// End-of-run artifact changes are appended to metadata.artifacts of the run's last assistant message.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "")
	run, _, _ := s.CreateRun(ctx, newRun(th.ID, "go"))
	_ = s.AppendMessages(ctx,
		&store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleAssistant, Content: "first"},
		&store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleAssistant, Content: "last", Metadata: map[string]any{"artifacts": []any{map[string]any{"path": "a"}}}})

	// Act
	err := s.AppendRunArtifacts(ctx, run.ID, []any{map[string]any{"path": "b", "change": "created"}})

	// Assert
	msgs, _ := s.ListMessages(ctx, th.ID)
	last, _ := msgs[len(msgs)-1].Metadata["artifacts"].([]any)
	if err != nil || len(last) != 2 || last[1].(map[string]any)["path"] != "b" || msgs[1].Metadata != nil {
		t.Fatalf("err %v messages %+v %+v", err, msgs[1].Metadata, msgs[2].Metadata)
	}
}
