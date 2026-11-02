package filestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/store"
)

// sameJSON compares by JSON: time.Time values differ in their Location pointer after a decode.
func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func open(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func ok(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// populate writes one of every persisted entity and returns the ids.
func populate(t *testing.T, s *Store) (threadID, runID string) {
	t.Helper()
	ctx := context.Background()
	th := must(s.CreateThread(ctx, "alice", ""))
	run, msg, err := s.CreateRun(ctx, store.NewRun{RunID: "run-1", ThreadID: th.ID, UserLogin: "alice", Model: "m", AutoTitle: "Hello there",
		Message: store.Message{Role: store.RoleUser, Content: "Hello there"}, Skills: []string{"docx"}, InteractionMode: store.InteractionInteractive})
	ok(t, err)
	_, err = s.ClaimRun(ctx, run.ID, "rt")
	ok(t, err)
	ok(t, s.AppendMessages(ctx,
		&store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleAssistant, Content: "calling",
			ToolCalls: []store.ToolCall{{ID: "c1", Type: "function", Function: store.ToolCallFunction{Name: "update_plan", Arguments: `{}`}}},
			Metadata:  map[string]any{"k": "v"}, ReasoningItems: json.RawMessage(`[{"id":"r"}]`)},
		&store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleTool, ToolCallID: "c1", Content: "result"},
		&store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleAssistant, Content: "final answer"}))
	ok(t, s.AppendEvents(ctx, run.ID, []store.Event{
		{Seq: 1, Type: agui.RunStarted, Data: json.RawMessage(`{"type":"RUN_STARTED"}`)},
		{Seq: 2, Type: agui.RunFinished, Data: json.RawMessage(`{"type":"RUN_FINISHED"}`)}}))
	ok(t, s.SetThreadPlan(ctx, th.ID, []store.PlanItem{{ID: "1", Title: "step", Status: store.PlanPending}}))
	ok(t, s.SetThreadSummary(ctx, th.ID, store.ThreadSummary{Text: "sum", ThroughPosition: 1}))
	ok(t, s.RecordCompaction(ctx, store.Compaction{ThreadID: th.ID, RunID: run.ID, Trigger: "auto", TokensBefore: 10, TokensAfter: 5}))
	_, err = s.PutGoal(ctx, "alice", th.ID, "ship it", 3)
	ok(t, err)
	ok(t, s.SetFeedback(ctx, "alice", must(s.ListMessages(ctx, th.ID))[3].ID, store.Feedback{Sentiment: "like", Comment: "nice"}))
	ok(t, s.CreateApproval(ctx, &store.Approval{ID: "ap1", Kind: store.ApprovalKindRiskyTool, ThreadID: th.ID, RunID: run.ID, ToolCallID: "c9", ToolName: "bash",
		ToolInputPreview: "rm", Status: store.ApprovalPending}))
	ok(t, s.FinishRun(ctx, run.ID, "rt", store.StatusCompleted, "", ""))
	pin := true
	_, err = s.UpdateThread(ctx, "alice", th.ID, store.ThreadPatch{Pinned: &pin})
	ok(t, err)
	_ = msg
	return th.ID, run.ID
}

type snapshot struct {
	Thread   *store.Thread
	Messages []*store.Message
	Run      *store.Run
	Events   []store.Event
	Summary  store.ThreadSummary
	Goal     *store.Goal
	Feedback map[string]store.Feedback
	Comps    []store.Compaction
	Approval *store.Approval
	Skills   []string
}

func snap(t *testing.T, s *Store, threadID, runID string) snapshot {
	t.Helper()
	ctx := context.Background()
	var sn snapshot
	sn.Thread = must(s.GetThread(ctx, "alice", threadID))
	sn.Messages = must(s.ListMessages(ctx, threadID))
	sn.Run = must(s.GetRun(ctx, runID))
	sn.Events = must(s.ListEvents(ctx, runID, 0, 0))
	sn.Summary = must(s.GetThreadSummary(ctx, threadID))
	sn.Goal = must(s.GetGoal(ctx, "alice", threadID))
	sn.Feedback = must(s.ListFeedback(ctx, threadID))
	sn.Comps = must(s.ListCompactions(ctx, threadID))
	sn.Approval = must(s.GetApproval(ctx, "ap1"))
	sn.Skills = must(s.RunSkills(ctx, runID))
	return sn
}

func TestRoundTripEveryPersistedEntityAcrossARestart(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	s := open(t, dir)
	threadID, runID := populate(t, s)
	before := snap(t, s, threadID, runID)

	// Act
	ok(t, s.Close())
	s2 := open(t, dir)
	defer s2.Close()
	after := snap(t, s2, threadID, runID)

	// Assert
	if len(before.Messages) != 4 || before.Thread.PinnedAt == nil || before.Goal == nil || len(before.Comps) != 1 || before.Approval == nil {
		t.Fatalf("test setup incomplete: %+v", before)
	}
	// Approvals pending on a completed run show on the thread view.
	if !sameJSON(before, after) {
		b, _ := json.MarshalIndent(before, "", " ")
		a, _ := json.MarshalIndent(after, "", " ")
		t.Fatalf("state differs after reopen\nbefore %s\nafter %s", b, a)
	}
	if s2.Recovered != 0 || s2.Truncated {
		t.Fatalf("recovered %d truncated %v", s2.Recovered, s2.Truncated)
	}
}

func TestMessagePositionsContinueAfterARestart(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	threadID, _ := populate(t, s)
	ok(t, s.Close())
	s2 := open(t, dir)
	defer s2.Close()

	ok(t, s2.AppendMessages(context.Background(), &store.Message{ThreadID: threadID, Role: store.RoleUser, Content: "again"}))

	msgs := must(s2.ListMessages(context.Background(), threadID))
	if len(msgs) != 5 || msgs[4].Position <= msgs[3].Position || msgs[4].Content != "again" {
		t.Fatalf("positions %v", msgs)
	}
}

func TestOpenCutsATornLastRecordAndKeepsEverythingBefore(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	s := open(t, dir)
	threadID, runID := populate(t, s)
	before := snap(t, s, threadID, runID)
	ok(t, s.AppendMessages(context.Background(), &store.Message{ThreadID: threadID, Role: store.RoleUser, Content: "the last message that will be torn"}))
	ok(t, s.Close())
	path := filepath.Join(dir, journalName)
	data := must(os.ReadFile(path))
	lastNL := strings.LastIndexByte(string(data[:len(data)-1]), '\n')

	for _, cut := range []int{len(data) - 1, len(data) - 20, lastNL + 1 + 10, lastNL + 2} {
		// Act: kill -9 in the middle of the last record.
		d2 := t.TempDir()
		ok(t, os.WriteFile(filepath.Join(d2, journalName), data[:cut], 0o600))
		s2, err := Open(d2)
		if err != nil {
			t.Fatalf("cut %d: %v", cut, err)
		}

		// Assert
		if !s2.Truncated {
			t.Fatalf("cut %d: torn tail not reported", cut)
		}
		after := snap(t, s2, threadID, runID)
		if !sameJSON(before.Messages, after.Messages) || !sameJSON(before.Events, after.Events) {
			t.Fatalf("cut %d: history changed", cut)
		}
		// The journal accepts new writes after the cut (no garbage glued to the next record).
		ok(t, s2.AppendMessages(context.Background(), &store.Message{ThreadID: threadID, Role: store.RoleUser, Content: "next"}))
		ok(t, s2.Close())
		s3 := open(t, d2)
		if got := must(s3.ListMessages(context.Background(), threadID)); got[len(got)-1].Content != "next" || s3.Truncated {
			t.Fatalf("cut %d: after append %v truncated=%v", cut, got[len(got)-1].Content, s3.Truncated)
		}
		s3.Close()
	}
}

func TestOpenRefusesAJournalDamagedBeforeItsTail(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	populate(t, s)
	ok(t, s.Close())
	path := filepath.Join(dir, journalName)
	lines := strings.Split(string(must(os.ReadFile(path))), "\n")
	lines[2] = lines[2][:len(lines[2])/2]
	ok(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600))

	_, err := Open(dir)

	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v", err)
	}
}

func TestAProcessKilledMidRunLeavesTheRunInterruptedWithATerminalEvent(t *testing.T) {
	// Arrange: no Close, the directory is copied as it is on disk (kill -9).
	dir := t.TempDir()
	s := open(t, dir)
	ctx := context.Background()
	th := must(s.CreateThread(ctx, "alice", "t"))
	run, _, err := s.CreateRun(ctx, store.NewRun{RunID: "r1", ThreadID: th.ID, UserLogin: "alice", Model: "m", Message: store.Message{Role: store.RoleUser, Content: "go"}})
	ok(t, err)
	_, err = s.ClaimRun(ctx, run.ID, "rt")
	ok(t, err)
	ok(t, s.AppendEvents(ctx, run.ID, []store.Event{{Seq: 1, Type: agui.RunStarted, Data: json.RawMessage(`{"type":"RUN_STARTED"}`)}}))
	queued := must(s.CreateThread(ctx, "alice", "q"))
	qrun, _, err := s.CreateRun(ctx, store.NewRun{RunID: "r2", ThreadID: queued.ID, UserLogin: "alice", Model: "m", Message: store.Message{Role: store.RoleUser, Content: "go"}})
	ok(t, err)
	killed := t.TempDir()
	ok(t, os.WriteFile(filepath.Join(killed, journalName), must(os.ReadFile(filepath.Join(dir, journalName))), 0o600))
	s.Close()

	// Act
	s2 := open(t, killed)
	defer s2.Close()

	// Assert
	if s2.Recovered != 2 {
		t.Fatalf("recovered %d", s2.Recovered)
	}
	for _, id := range []string{run.ID, qrun.ID} {
		r := must(s2.GetRun(ctx, id))
		if r.Status != store.StatusInterrupted || r.ErrorCode != "interrupted" || r.FinishedAt == nil {
			t.Fatalf("run %s: %+v", id, r)
		}
		last, found, _ := s2.LastEvent(ctx, id)
		if !found || last.Type != agui.RunError {
			t.Fatalf("run %s last event %+v", id, last)
		}
	}
	if th2 := must(s2.GetThread(ctx, "alice", th.ID)); th2.ActiveRun != nil {
		t.Fatalf("thread still has an active run: %+v", th2.ActiveRun)
	}
	// The recovery itself is durable and idempotent.
	ok(t, s2.Close())
	s3 := open(t, killed)
	defer s3.Close()
	if s3.Recovered != 0 || must(s3.GetRun(ctx, run.ID)).Status != store.StatusInterrupted {
		t.Fatalf("second open recovered %d", s3.Recovered)
	}
	if evs := must(s3.ListEvents(ctx, run.ID, 0, 0)); len(evs) != 2 {
		t.Fatalf("events %v", evs)
	}
}

func TestConcurrentWritersKeepAConsistentJournal(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	ctx := context.Background()
	const threads, perThread = 8, 60
	var ids [threads]string
	for i := range ids {
		ids[i] = must(s.CreateThread(ctx, "alice", fmt.Sprint("t", i))).ID
	}

	var wg sync.WaitGroup
	for i := 0; i < threads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			run, _, err := s.CreateRun(ctx, store.NewRun{RunID: fmt.Sprint("run", i), ThreadID: ids[i], UserLogin: "alice", Model: "m",
				Message: store.Message{Role: store.RoleUser, Content: "go"}})
			if err != nil {
				t.Error(err)
				return
			}
			for k := 1; k <= perThread; k++ {
				if err := s.AppendMessages(ctx, &store.Message{ThreadID: ids[i], RunID: run.ID, Role: store.RoleAssistant, Content: fmt.Sprint("m", k)}); err != nil {
					t.Error(err)
				}
				if err := s.AppendEvents(ctx, run.ID, []store.Event{{Seq: int64(k), Type: agui.TextMessageContent, Data: json.RawMessage(`{}`)}}); err != nil {
					t.Error(err)
				}
			}
			if _, err := s.ClaimRun(ctx, run.ID, "rt"); err != nil {
				t.Error(err)
			}
			if err := s.FinishRun(ctx, run.ID, "rt", store.StatusCompleted, "", ""); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	ok(t, s.Close())

	s2 := open(t, dir)
	defer s2.Close()
	for i := range ids {
		msgs := must(s2.ListMessages(ctx, ids[i]))
		evs := must(s2.ListEvents(ctx, fmt.Sprint("run", i), 0, 0))
		if len(msgs) != perThread+1 || len(evs) != perThread {
			t.Fatalf("thread %d: %d messages, %d events", i, len(msgs), len(evs))
		}
		for k := 1; k <= perThread; k++ { // per-thread order is preserved
			if msgs[k].Content != fmt.Sprint("m", k) {
				t.Fatalf("thread %d message %d = %q", i, k, msgs[k].Content)
			}
		}
	}
}

func TestSecondOpenOfTheSameDirectoryIsRefused(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	defer s.Close()

	if _, err := Open(dir); err == nil {
		t.Fatal("expected the lock to refuse a second process")
	}
}

func TestSessionsListForkAndDelete(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	s := open(t, dir)
	ctx := context.Background()
	threadID, _ := populate(t, s)
	other := must(s.CreateThread(ctx, "alice", "Newer"))
	time.Sleep(2 * time.Millisecond)
	_, _, err := s.CreateRun(ctx, store.NewRun{RunID: "r-other", ThreadID: other.ID, UserLogin: "alice", Model: "m", Message: store.Message{Role: store.RoleUser, Content: "  hi\n\nthere  "}})
	ok(t, err)
	must(s.CreateThread(ctx, "bob", "not alice"))
	ss := s.Sessions()

	// Act
	list := must(ss.List(ctx, "alice", 0))

	// Assert
	if len(list) != 2 || list[0].ID != other.ID || list[0].Preview != "hi there" || list[0].ActiveRun == nil {
		t.Fatalf("list %+v", list)
	}
	if list[1].ID != threadID || list[1].Preview != "final answer" || list[1].Messages != 4 || !list[1].Pinned || list[1].LastStatus != store.StatusCompleted {
		t.Fatalf("list[1] %+v", list[1])
	}

	// Fork up to the tool result: the cut moves back to the user message (no dangling tool call).
	msgs := must(s.ListMessages(ctx, threadID))
	fork := must(ss.Fork(ctx, "alice", threadID, msgs[2].ID))
	fm := must(s.ListMessages(ctx, fork.ID))
	if len(fm) != 1 || fm[0].Role != store.RoleUser || fm[0].ID == msgs[0].ID || fm[0].ThreadID != fork.ID || fork.Title != "Fork of Hello there" {
		t.Fatalf("fork %+v %+v", fork, fm)
	}
	full := must(ss.Fork(ctx, "alice", threadID, ""))
	if got := must(s.ListMessages(ctx, full.ID)); len(got) != 4 || got[3].Content != "final answer" {
		t.Fatalf("full fork %v", got)
	}
	if _, err := ss.Fork(ctx, "alice", threadID, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown message: %v", err)
	}
	if _, err := ss.Fork(ctx, "bob", threadID, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("other user: %v", err)
	}

	// Forks and deletions survive a restart; deleted threads leave the disk at compaction.
	ok(t, ss.Delete(ctx, "alice", threadID))
	ok(t, s.Close())
	s2 := open(t, dir)
	defer s2.Close()
	l2 := must(s2.Sessions().List(ctx, "alice", 0))
	if len(l2) != 3 {
		t.Fatalf("after restart %+v", l2)
	}
	for _, in := range l2 {
		if in.ID == threadID {
			t.Fatal("deleted thread listed")
		}
	}
	if raw := string(must(os.ReadFile(filepath.Join(dir, journalName)))); strings.Contains(raw, `"final answer"`) && strings.Count(raw, threadID) > 0 {
		// The fork holds a copy of "final answer"; the deleted thread's id must be gone.
		t.Fatal("deleted thread still on disk after compaction")
	}
}

func TestCompactionKeepsTheStateAndShrinksTheJournal(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	ctx := context.Background()
	th := must(s.CreateThread(ctx, "alice", "t"))
	for i := 0; i < 300; i++ { // many superseded run/thread records
		run, _, err := s.CreateRun(ctx, store.NewRun{RunID: fmt.Sprint("r", i), ThreadID: th.ID, UserLogin: "alice", Model: "m", Message: store.Message{Role: store.RoleUser, Content: "q"}})
		ok(t, err)
		_, err = s.ClaimRun(ctx, run.ID, "rt")
		ok(t, err)
		ok(t, s.AppendEvents(ctx, run.ID, []store.Event{{Seq: 1, Type: agui.RunFinished, Data: json.RawMessage(`{}`)}}))
		ok(t, s.FinishRun(ctx, run.ID, "rt", store.StatusCompleted, "", ""))
	}
	ok(t, s.Close())
	path := filepath.Join(dir, journalName)
	big := must(os.Stat(path)).Size()

	s2 := open(t, dir)
	ok(t, s2.Close())

	small := must(os.Stat(path)).Size()
	s3 := open(t, dir)
	defer s3.Close()
	if small >= big || len(must(s3.ListMessages(ctx, th.ID))) != 300 || must(s3.GetRun(ctx, "r299")).Status != store.StatusCompleted ||
		len(must(s3.ListEvents(ctx, "r10", 0, 0))) != 1 {
		t.Fatalf("journal %d -> %d bytes", big, small)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file left behind")
	}
}
