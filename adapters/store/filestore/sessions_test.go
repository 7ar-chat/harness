package filestore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/store"
)

// turn adds a finished run with a user message and an assistant answer to the thread.
func turn(t *testing.T, s *Store, threadID, user, answer string) {
	t.Helper()
	ctx := context.Background()
	run, _, err := s.CreateRun(ctx, store.NewRun{RunID: fmt.Sprint("run-", user), ThreadID: threadID, UserLogin: "alice", Model: "m", Message: store.Message{Role: store.RoleUser, Content: user}})
	ok(t, err)
	_, err = s.ClaimRun(ctx, run.ID, "rt")
	ok(t, err)
	ok(t, s.AppendMessages(ctx, &store.Message{ThreadID: threadID, RunID: run.ID, Role: store.RoleAssistant, Content: answer}))
	ok(t, s.FinishRun(ctx, run.ID, "rt", store.StatusCompleted, "", ""))
}

func TestSessionsListIsLimitedAndNewestFirst(t *testing.T) {
	// Arrange
	s := open(t, t.TempDir())
	defer s.Close()
	ctx := context.Background()
	older := must(s.CreateThread(ctx, "alice", "older"))
	newer := must(s.CreateThread(ctx, "alice", "newer"))
	turn(t, s, older.ID, "touch", "done") // the latest activity makes "older" the most recently updated

	// Act
	all := must(s.Sessions().List(ctx, "alice", 0))
	one := must(s.Sessions().List(ctx, "alice", 1))
	none := must(s.Sessions().List(ctx, "carol", 0))

	// Assert
	if len(all) != 2 || all[0].ID != older.ID || all[1].ID != newer.ID {
		t.Fatalf("all = %+v", all)
	}
	if len(one) != 1 || one[0].ID != older.ID {
		t.Fatalf("limit 1 = %+v", one)
	}
	if len(none) != 0 {
		t.Fatalf("another user's sessions: %+v", none)
	}
}

func TestSessionsListPreviewsTheLastVisibleUserOrAssistantTextOnOneLine(t *testing.T) {
	// Arrange
	s := open(t, t.TempDir())
	defer s.Close()
	ctx := context.Background()
	th := must(s.CreateThread(ctx, "alice", "t"))
	long := strings.Repeat("word ", 60) // 300 characters, more than the 120 of a preview
	turn(t, s, th.ID, "question", "line one\n\n   line   two")
	run := must(s.GetRun(ctx, "run-question"))
	ok(t, s.AppendMessages(ctx,
		&store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleTool, ToolCallID: "c", Content: "tool output is never the preview"},
		&store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleAssistant, Content: "  \n "}))

	// Act
	first := must(s.Sessions().List(ctx, "alice", 0))[0]
	ok(t, s.AppendMessages(ctx, &store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleAssistant, Content: long}))
	second := must(s.Sessions().List(ctx, "alice", 0))[0]

	// Assert
	if first.Preview != "line one line two" || first.Messages != 4 {
		t.Errorf("preview %q over %d messages", first.Preview, first.Messages)
	}
	if want := strings.Repeat("word ", 24) + "…"; second.Preview != want {
		t.Errorf("long preview %q, want %q", second.Preview, want)
	}
	if second.LastStatus != store.StatusCompleted || second.ActiveRun != nil {
		t.Errorf("status %q active %+v", second.LastStatus, second.ActiveRun)
	}
}

func TestForkCopiesTheHistoryUpToAMessageWithoutTheirIdentity(t *testing.T) {
	// Arrange
	s := open(t, t.TempDir())
	defer s.Close()
	ctx := context.Background()
	th := must(s.CreateThread(ctx, "alice", ""))
	turn(t, s, th.ID, "first", "re: first")
	turn(t, s, th.ID, "second", "re: second")
	src := must(s.ListMessages(ctx, th.ID))

	// Act
	fork, err := s.Sessions().Fork(ctx, "alice", th.ID, src[1].ID)

	// Assert
	ok(t, err)
	got := must(s.ListMessages(ctx, fork.ID))
	if len(got) != 2 || got[0].Content != "first" || got[1].Content != "re: first" {
		t.Fatalf("fork messages %+v", got)
	}
	for i, m := range got {
		if m.ID == src[i].ID || m.ThreadID != fork.ID || m.RunID != "" || m.Position <= src[len(src)-1].Position {
			t.Errorf("copy %d keeps the source identity: %+v", i, m)
		}
	}
	if fork.Title != "" || fork.UserLogin != "alice" || fork.ID == th.ID {
		t.Errorf("an untitled thread forks untitled: %+v", fork)
	}
	if len(must(s.ListMessages(ctx, th.ID))) != 4 {
		t.Error("the source thread changed")
	}
}

func TestForkRecomputesThePlanFromTheCopiedMessages(t *testing.T) {
	// Arrange
	s := open(t, t.TempDir())
	defer s.Close()
	ctx := context.Background()
	th := must(s.CreateThread(ctx, "alice", "planned"))
	run, _, err := s.CreateRun(ctx, store.NewRun{RunID: "r", ThreadID: th.ID, UserLogin: "alice", Model: "m", Message: store.Message{Role: store.RoleUser, Content: "plan it"}})
	ok(t, err)
	_, err = s.ClaimRun(ctx, run.ID, "rt")
	ok(t, err)
	call := store.ToolCall{ID: "p1", Type: "function", Function: store.ToolCallFunction{Name: "update_plan", Arguments: `{"items":[{"id":"1","title":"Do it","status":"in_progress"}]}`}}
	ok(t, s.AppendMessages(ctx,
		&store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleAssistant, ToolCalls: []store.ToolCall{call}},
		&store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleTool, ToolCallID: "p1", Content: "Plan updated"},
		&store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleAssistant, Content: "planned"}))
	ok(t, s.FinishRun(ctx, run.ID, "rt", store.StatusCompleted, "", ""))
	msgs := must(s.ListMessages(ctx, th.ID))

	// Act
	whole, err := s.Sessions().Fork(ctx, "alice", th.ID, "")
	ok(t, err)
	beforeCall, err2 := s.Sessions().Fork(ctx, "alice", th.ID, msgs[1].ID) // cut inside the tool-call exchange
	ok(t, err2)

	// Assert
	if len(whole.Plan) != 1 || whole.Plan[0].Title != "Do it" {
		t.Errorf("full fork plan %+v", whole.Plan)
	}
	if got := must(s.ListMessages(ctx, beforeCall.ID)); len(got) != 1 || got[0].Role != store.RoleUser || len(beforeCall.Plan) != 0 {
		t.Errorf("a fork never stops between a tool call and its result: %+v plan %+v", got, beforeCall.Plan)
	}
}

func TestForkOfAThreadWithoutAnswerableMessagesIsEmpty(t *testing.T) {
	// Arrange
	s := open(t, t.TempDir())
	defer s.Close()
	ctx := context.Background()
	th := must(s.CreateThread(ctx, "alice", "empty"))

	// Act
	fork, err := s.Sessions().Fork(ctx, "alice", th.ID, "")

	// Assert
	ok(t, err)
	if got := must(s.ListMessages(ctx, fork.ID)); len(got) != 0 || fork.Title != "Fork of empty" {
		t.Fatalf("fork %+v messages %v", fork, got)
	}
}

func TestForkAndDeleteOfAnUnknownOrForeignThreadReportNotFound(t *testing.T) {
	// Arrange
	s := open(t, t.TempDir())
	defer s.Close()
	ctx := context.Background()
	th := must(s.CreateThread(ctx, "alice", "mine"))

	// Act
	_, forkErr := s.Sessions().Fork(ctx, "alice", "nope", "")
	deleteErr := s.Sessions().Delete(ctx, "bob", th.ID)

	// Assert
	if !errors.Is(forkErr, store.ErrNotFound) || !errors.Is(deleteErr, store.ErrNotFound) {
		t.Fatalf("fork %v delete %v", forkErr, deleteErr)
	}
	if _, err := s.GetThread(ctx, "alice", th.ID); err != nil {
		t.Fatalf("a refused delete removed the thread: %v", err)
	}
}
