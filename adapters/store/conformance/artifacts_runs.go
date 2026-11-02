package conformance

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/7ar-chat/harness/core/store"
)

func artSuiteSkills(t *testing.T, f Factory) {
	ctx := context.Background()

	t.Run("ReturnsTheSkillsRequestedAtRunCreation", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := artThread(t, s)
		in := store.NewRun{RunID: uuid.NewString(), ThreadID: th.ID, UserLogin: "alice", Model: "m", Skills: []string{"docx", "my-report"},
			Message: store.Message{Role: store.RoleUser, Content: "go"}}

		// Act
		run, _, err := s.CreateRun(ctx, in)
		if err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		skills, skillsErr := s.RunSkills(ctx, run.ID)

		// Assert
		if skillsErr != nil || !slices.Equal(skills, []string{"docx", "my-report"}) {
			t.Errorf("skills %v (%v)", skills, skillsErr)
		}
	})

	t.Run("ARunWithoutSkillsHasNone", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := artThread(t, s)
		run := artRun(t, s, th.ID)

		// Act
		skills, err := s.RunSkills(ctx, run.ID)

		// Assert
		if err != nil || len(skills) != 0 {
			t.Errorf("skills %v (%v)", skills, err)
		}
	})

	t.Run("AnUnknownRunIsNotFound", func(t *testing.T) {
		// Arrange
		s := f(t).Store

		// Act
		_, unknown := s.RunSkills(ctx, uuid.NewString())
		_, malformed := s.RunSkills(ctx, "not-an-id")

		// Assert
		if !errors.Is(unknown, store.ErrNotFound) || !errors.Is(malformed, store.ErrNotFound) {
			t.Errorf("unknown run: %v, malformed id: %v", unknown, malformed)
		}
	})
}

// artArtifactsOf decodes metadata.artifacts of a message into its paths.
func artArtifactPaths(m *store.Message) []string { return store.ArtifactPaths(m.Metadata) }

func artSuiteRunArtifacts(t *testing.T, f Factory) {
	ctx := context.Background()
	item := func(path, change string) map[string]any { return map[string]any{"path": path, "change": change} }

	t.Run("AppendsToTheLastAssistantMessageOfTheRun", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := artThread(t, s)
		run := artRun(t, s, th.ID)
		artMust(t, "AppendMessages", s.AppendMessages(ctx,
			&store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleAssistant, Content: "first"},
			&store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleAssistant, Content: "last", Metadata: map[string]any{"other": "kept", "artifacts": []any{item("a", "created")}}}))

		// Act
		err := s.AppendRunArtifacts(ctx, run.ID, []any{item("b", "created"), item("c", store.ChangeDeleted)})

		// Assert
		msgs, _ := s.ListMessages(ctx, th.ID)
		if err != nil || len(msgs) != 3 {
			t.Fatalf("append %v, messages %+v", err, msgs)
		}
		if msgs[1].Metadata != nil && msgs[1].Metadata["artifacts"] != nil {
			t.Errorf("an earlier assistant message got artifacts: %+v", msgs[1].Metadata)
		}
		if got := artArtifactPaths(msgs[2]); !slices.Equal(got, []string{"a", "b", "c (deleted)"}) || msgs[2].Metadata["other"] != "kept" {
			t.Errorf("last message artifacts %v metadata %+v", got, msgs[2].Metadata)
		}
	})

	t.Run("SkipsSupersededAndOtherRunsMessages", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := artThread(t, s)
		run := artRun(t, s, th.ID)
		artMust(t, "AppendMessages", s.AppendMessages(ctx, &store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleAssistant, Content: "mine"}))
		artMust(t, "FinishRun", func() error {
			if _, err := s.ClaimRun(ctx, run.ID, "rt"); err != nil {
				return err
			}
			return s.FinishRun(ctx, run.ID, "rt", store.StatusCompleted, "", "")
		}())
		next := artRun(t, s, th.ID)
		artMust(t, "AppendMessages", s.AppendMessages(ctx, &store.Message{ThreadID: th.ID, RunID: next.ID, Role: store.RoleAssistant, Content: "next"}))

		// Act
		err := s.AppendRunArtifacts(ctx, run.ID, []any{item("a", "created")})

		// Assert
		msgs, _ := s.ListMessages(ctx, th.ID)
		var mine, other *store.Message
		for _, m := range msgs {
			switch m.Content {
			case "mine":
				mine = m
			case "next":
				other = m
			}
		}
		if err != nil || mine == nil || other == nil || !slices.Equal(artArtifactPaths(mine), []string{"a"}) || len(artArtifactPaths(other)) != 0 {
			t.Errorf("append %v, mine %+v, other %+v", err, mine, other)
		}
	})

	t.Run("ARunWithoutAnAnswerGetsAnEmptyAssistantMessage", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := artThread(t, s)
		run := artRun(t, s, th.ID)

		// Act
		err := s.AppendRunArtifacts(ctx, run.ID, []any{item("/workspace/a.csv", "created")})

		// Assert
		msgs, _ := s.ListMessages(ctx, th.ID)
		if err != nil || len(msgs) != 2 {
			t.Fatalf("append %v, messages %+v", err, msgs)
		}
		last := msgs[1]
		if last.Role != store.RoleAssistant || last.Content != "" || last.RunID != run.ID || last.ThreadID != th.ID ||
			last.Position <= msgs[0].Position || !slices.Equal(artArtifactPaths(last), []string{"/workspace/a.csv"}) {
			t.Errorf("created message %+v", last)
		}
	})

	t.Run("StoresTheItemsAsJSON", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := artThread(t, s)
		run := artRun(t, s, th.ID)
		type change struct {
			Path   string `json:"path"`
			Change string `json:"change"`
		}

		// Act
		err := s.AppendRunArtifacts(ctx, run.ID, []any{change{Path: "a", Change: "created"}})

		// Assert
		msgs, _ := s.ListMessages(ctx, th.ID)
		if err != nil || len(msgs) != 2 {
			t.Fatalf("append %v, messages %+v", err, msgs)
		}
		want := []any{map[string]any{"path": "a", "change": "created"}}
		if got := msgs[1].Metadata["artifacts"]; !reflect.DeepEqual(got, want) {
			t.Errorf("artifacts %#v, want %#v", got, want)
		}
	})

	t.Run("IgnoresNothingToAppendAnUnknownRunAndADeletedThread", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := artThread(t, s)
		run := artRun(t, s, th.ID)
		gone := artThread(t, s)
		goneRun := artRun(t, s, gone.ID)
		artDeleteThread(t, s, gone.ID)

		// Act
		empty := s.AppendRunArtifacts(ctx, run.ID, nil)
		unknown := s.AppendRunArtifacts(ctx, uuid.NewString(), []any{item("a", "created")})
		malformed := s.AppendRunArtifacts(ctx, "not-an-id", []any{item("a", "created")})
		deleted := s.AppendRunArtifacts(ctx, goneRun.ID, []any{item("a", "created")})

		// Assert
		if empty != nil || unknown != nil || malformed != nil || deleted != nil {
			t.Errorf("errors %v %v %v %v", empty, unknown, malformed, deleted)
		}
		if msgs, _ := s.ListMessages(ctx, th.ID); len(msgs) != 1 {
			t.Errorf("messages %+v", msgs)
		}
	})
}

func artSuiteToolVerification(t *testing.T, f Factory) {
	ctx := context.Background()

	t.Run("GoesToTheToolMessageOfTheCall", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := artThread(t, s)
		run := artRun(t, s, th.ID)
		artMust(t, "AppendMessages", s.AppendMessages(ctx,
			&store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleTool, ToolCallID: "c0", Content: "other"},
			&store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleTool, ToolCallID: "c1", Content: "five files",
				Metadata: map[string]any{"verification": []any{map[string]any{"path": "a.docx"}}, "verificationPending": []any{"d.docx"}}}))

		// Act
		err := s.AppendToolVerification(ctx, th.ID, "c1", []any{map[string]any{"path": "d.docx", "ok": true}})

		// Assert
		msgs, _ := s.ListMessages(ctx, th.ID)
		if err != nil || len(msgs) != 3 {
			t.Fatalf("append %v, messages %+v", err, msgs)
		}
		want := []any{map[string]any{"path": "a.docx"}, map[string]any{"path": "d.docx", "ok": true}}
		if got := msgs[2].Metadata["verification"]; !reflect.DeepEqual(got, want) {
			t.Errorf("verification %#v, want %#v", got, want)
		}
		if pending, _ := msgs[2].Metadata["verificationPending"].([]any); len(pending) != 1 {
			t.Errorf("the other metadata was lost: %+v", msgs[2].Metadata)
		}
		if msgs[1].Metadata != nil {
			t.Errorf("another call's message changed: %+v", msgs[1].Metadata)
		}
	})

	t.Run("AppendsToTheLatestMessageOfTheCall", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := artThread(t, s)
		run := artRun(t, s, th.ID)
		artMust(t, "AppendMessages", s.AppendMessages(ctx,
			&store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleTool, ToolCallID: "c1", Content: "first result"},
			&store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleTool, ToolCallID: "c1", Content: "retried result"}))

		// Act
		err := s.AppendToolVerification(ctx, th.ID, "c1", []any{map[string]any{"path": "x"}})

		// Assert
		msgs, _ := s.ListMessages(ctx, th.ID)
		if err != nil || len(msgs) != 3 || msgs[1].Metadata != nil || msgs[2].Metadata["verification"] == nil {
			t.Errorf("append %v, messages %+v %+v", err, msgs[1].Metadata, msgs[2].Metadata)
		}
	})

	t.Run("DoesNothingWithoutAStoredResult", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := artThread(t, s)
		run := artRun(t, s, th.ID)
		artMust(t, "AppendMessages", s.AppendMessages(ctx, &store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleTool, ToolCallID: "c1", Content: "x"}))
		items := []any{map[string]any{"path": "x"}}

		// Act
		unknownCall := s.AppendToolVerification(ctx, th.ID, "nope", items)
		noCall := s.AppendToolVerification(ctx, th.ID, "", items)
		noItems := s.AppendToolVerification(ctx, th.ID, "c1", nil)
		unknownThread := s.AppendToolVerification(ctx, uuid.NewString(), "c1", items)
		malformedThread := s.AppendToolVerification(ctx, "not-an-id", "c1", items)

		// Assert
		if unknownCall != nil || noCall != nil || noItems != nil || unknownThread != nil || malformedThread != nil {
			t.Errorf("errors %v %v %v %v %v", unknownCall, noCall, noItems, unknownThread, malformedThread)
		}
		if msgs, _ := s.ListMessages(ctx, th.ID); msgs[1].Metadata != nil {
			t.Errorf("metadata %+v", msgs[1].Metadata)
		}
	})
}

func artSuiteWorkspaceGit(t *testing.T, f Factory) {
	ctx := context.Background()
	archive := func(threadID, key, head string, size int64) store.WorkspaceGitArchive {
		return store.WorkspaceGitArchive{ThreadID: threadID, BlobKey: key, Head: head, SizeBytes: size}
	}

	t.Run("HasNoArchiveUntilOneIsPut", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := artThread(t, s)

		// Act
		_, err := s.WorkspaceGit(ctx, th.ID)

		// Assert
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("WorkspaceGit: %v, want ErrNotFound", err)
		}
	})

	t.Run("RecordsTheArchive", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := artThread(t, s)

		// Act
		putErr := s.PutWorkspaceGit(ctx, archive(th.ID, "workspace-git/a", "h1", 3))
		got, err := s.WorkspaceGit(ctx, th.ID)

		// Assert
		if putErr != nil || err != nil {
			t.Fatalf("put %v get %v", putErr, err)
		}
		if got.ThreadID != th.ID || got.BlobKey != "workspace-git/a" || got.Head != "h1" || got.SizeBytes != 3 || got.UpdatedAt.IsZero() {
			t.Errorf("archive %+v", got)
		}
	})

	t.Run("ReplacingQueuesThePreviousBlobUnlessTheKeyIsTheSame", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		th := artThread(t, s)
		artMust(t, "PutWorkspaceGit", s.PutWorkspaceGit(ctx, archive(th.ID, "workspace-git/a", "h1", 3)))
		b.Advance(t, artGrace+time.Hour)

		// Act
		sameErr := s.PutWorkspaceGit(ctx, archive(th.ID, "workspace-git/a", "h2", 5))
		afterSame := artCandidates(t, s, artGrace, 10)
		nextErr := s.PutWorkspaceGit(ctx, archive(th.ID, "workspace-git/b", "h3", 7))
		b.Advance(t, artGrace+time.Hour)
		afterReplace := artCandidates(t, s, artGrace, 10)
		got, err := s.WorkspaceGit(ctx, th.ID)

		// Assert
		if sameErr != nil || nextErr != nil || err != nil {
			t.Fatalf("errors %v %v %v", sameErr, nextErr, err)
		}
		if len(afterSame) != 0 {
			t.Errorf("replacing an archive by the same key queued it: %v", afterSame)
		}
		if !slices.Equal(afterReplace, []string{"workspace-git/a"}) {
			t.Errorf("candidates after the replacement %v, want [workspace-git/a]", afterReplace)
		}
		if got.BlobKey != "workspace-git/b" || got.Head != "h3" || got.SizeBytes != 7 {
			t.Errorf("archive %+v", got)
		}
	})

	t.Run("AReusedKeyLeavesTheQueue", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		th := artThread(t, s)
		artMust(t, "QueueBlob", s.QueueBlob(ctx, "workspace-git/a"))
		b.Advance(t, artGrace+time.Hour)

		// Act
		err := s.PutWorkspaceGit(ctx, archive(th.ID, "workspace-git/a", "h1", 3))
		candidates := artCandidates(t, s, artGrace, 10)

		// Assert
		if err != nil || len(candidates) != 0 {
			t.Errorf("put %v, candidates %v", err, candidates)
		}
	})

	t.Run("AnUnknownThreadIsNotFound", func(t *testing.T) {
		// Arrange
		s := f(t).Store

		// Act
		err := s.PutWorkspaceGit(ctx, archive(uuid.NewString(), "workspace-git/a", "h1", 3))

		// Assert
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("PutWorkspaceGit: %v, want ErrNotFound", err)
		}
	})
}

func artSuiteWorkspaceGitIssue(t *testing.T, f Factory) {
	ctx := context.Background()

	t.Run("IsAbsentUntilRecordedAndUpsertedPerThread", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := artThread(t, s)
		_, absent := s.WorkspaceGitIssue(ctx, th.ID)

		// Act
		firstErr := s.SetWorkspaceGitIssue(ctx, store.WorkspaceGitIssue{ThreadID: th.ID, Reason: store.GitIssueFailed})
		secondErr := s.SetWorkspaceGitIssue(ctx, store.WorkspaceGitIssue{ThreadID: th.ID, Reason: store.GitIssueTooLarge, Head: "abc", SizeBytes: 99})
		got, err := s.WorkspaceGitIssue(ctx, th.ID)

		// Assert
		if !errors.Is(absent, store.ErrNotFound) {
			t.Errorf("before any issue: %v", absent)
		}
		if firstErr != nil || secondErr != nil || err != nil {
			t.Fatalf("errors %v %v %v", firstErr, secondErr, err)
		}
		if got.ThreadID != th.ID || got.Reason != store.GitIssueTooLarge || got.Head != "abc" || got.SizeBytes != 99 || got.UpdatedAt.IsZero() {
			t.Errorf("issue %+v", got)
		}
	})

	t.Run("IsClearedAndClearingTwiceIsHarmless", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := artThread(t, s)
		artMust(t, "SetWorkspaceGitIssue", s.SetWorkspaceGitIssue(ctx, store.WorkspaceGitIssue{ThreadID: th.ID, Reason: store.GitIssueFailed}))

		// Act
		clearErr := s.ClearWorkspaceGitIssue(ctx, th.ID)
		_, cleared := s.WorkspaceGitIssue(ctx, th.ID)
		againErr := s.ClearWorkspaceGitIssue(ctx, th.ID)

		// Assert
		if clearErr != nil || againErr != nil || !errors.Is(cleared, store.ErrNotFound) {
			t.Errorf("clear %v then %v, issue %v", clearErr, againErr, cleared)
		}
	})

	t.Run("IsIgnoredForAThreadThatDoesNotExist", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		id := uuid.NewString()

		// Act
		err := s.SetWorkspaceGitIssue(ctx, store.WorkspaceGitIssue{ThreadID: id, Reason: store.GitIssueFailed})
		_, getErr := s.WorkspaceGitIssue(ctx, id)

		// Assert
		if err != nil || !errors.Is(getErr, store.ErrNotFound) {
			t.Errorf("set %v, get %v", err, getErr)
		}
	})
}

func artSuiteSanitize(t *testing.T, f Factory) {
	ctx := context.Background()

	t.Run("SanitizesNULAndInvalidUTF8LikePostgres", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := artThread(t, s)
		run := artRun(t, s, th.ID)
		bad := "a\x00b\xffc"
		const clean = "a\uFFFDb\uFFFDc"
		in := store.PutArtifact{ThreadID: th.ID, UserLogin: "alice", Path: "/workspace/" + bad, Name: bad, MimeType: "text/" + bad,
			Kind: store.ArtifactOutput, SHA256: "sha", SandboxID: "sb-" + bad}

		// Act
		a, change, err := s.PutArtifactVersion(ctx, in)
		again, againChange, againErr := s.PutArtifactVersion(ctx, in) // the cleaned path is the same artifact
		artErr := s.AppendRunArtifacts(ctx, run.ID, []any{map[string]any{"path": bad, "change": "created"}})
		msgs, _ := s.ListMessages(ctx, th.ID)

		// Assert
		if err != nil || change != store.ChangeCreated {
			t.Fatalf("PutArtifactVersion: %q %v", change, err)
		}
		if a.Path != "/workspace/"+clean || a.Name != clean || a.MimeType != "text/"+clean || a.SandboxID != "sb-"+clean {
			t.Errorf("artifact %+v", a)
		}
		if againErr != nil || againChange != "" || again.ID != a.ID {
			t.Errorf("second put: %q %v (%+v)", againChange, againErr, again)
		}
		if artErr != nil || len(msgs) != 2 || !slices.Equal(artArtifactPaths(msgs[1]), []string{clean}) {
			t.Errorf("run artifacts %v: %+v", artErr, msgs)
		}
	})
}
