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

// killCopy returns a new state directory holding the journal of dir as it is on disk right now: what a kill -9
// leaves behind (the original store stays open and is closed by its test).
func killCopy(t *testing.T, dir string) string {
	t.Helper()
	killed := t.TempDir()
	ok(t, os.WriteFile(filepath.Join(killed, journalName), must(os.ReadFile(filepath.Join(dir, journalName))), 0o600))
	return killed
}

func journalSize(t *testing.T, dir string) int64 {
	t.Helper()
	return must(os.Stat(filepath.Join(dir, journalName))).Size()
}

func writeJournal(t *testing.T, dir string, lines ...string) {
	t.Helper()
	ok(t, os.WriteFile(filepath.Join(dir, journalName), []byte(strings.Join(lines, "\n")), 0o600))
}

func eventTypes(evs []store.Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, fmt.Sprintf("%d:%s", e.Seq, e.Type))
	}
	return out
}

func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permission checks do not apply to root")
	}
}

// ---- Open ----

func TestOpenRequiresAStateDirectory(t *testing.T) {
	// Act
	s, err := Open("")

	// Assert
	if err == nil || s != nil {
		t.Fatalf("Open(\"\") = %v, %v", s, err)
	}
}

func TestOpenFailsWhenTheStateDirectoryCannotBeCreated(t *testing.T) {
	// Arrange: a regular file where a directory is needed.
	file := filepath.Join(t.TempDir(), "state")
	ok(t, os.WriteFile(file, []byte("x"), 0o600))

	// Act
	_, err := Open(filepath.Join(file, "sub"))

	// Assert
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestOpenCreatesTheDirectoryAndReportsIt(t *testing.T) {
	// Arrange
	dir := filepath.Join(t.TempDir(), "a", "b")

	// Act
	s := open(t, dir)
	defer s.Close()

	// Assert
	if s.Dir() != dir {
		t.Fatalf("Dir() = %q, want %q", s.Dir(), dir)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("state directory: %v", err)
	}
}

func TestOpenFailsInAnUnwritableDirectoryAndLeavesNothingLocked(t *testing.T) {
	skipIfRoot(t)
	for name, prepare := range map[string]func(dir string){
		"no lock file yet":         func(string) {},
		"lock file but no journal": func(dir string) { ok(t, os.WriteFile(filepath.Join(dir, "LOCK"), nil, 0o600)) },
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			dir := t.TempDir()
			prepare(dir)
			ok(t, os.Chmod(dir, 0o500))
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

			// Act
			_, err := Open(dir)

			// Assert
			if err == nil {
				t.Fatal("expected an error")
			}
			ok(t, os.Chmod(dir, 0o700))
			s := open(t, dir) // the failed Open did not keep the directory locked
			ok(t, s.Close())
		})
	}
}

func TestSecondOpenIsRefusedUntilTheFirstStoreIsClosed(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	s := open(t, dir)
	populate(t, s)

	// Act
	_, err := Open(dir)
	ok(t, s.Close())
	s2, err2 := Open(dir)

	// Assert
	if err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("second Open while open: %v", err)
	}
	if err2 != nil {
		t.Fatalf("Open after Close: %v", err2)
	}
	ok(t, s2.Close())
}

func TestOpenReportsAnUnreadableJournalAndReleasesTheDirectory(t *testing.T) {
	// Arrange: a directory where the journal file should be.
	dir := t.TempDir()
	ok(t, os.Mkdir(filepath.Join(dir, journalName), 0o700))

	// Act
	_, err := Open(dir)

	// Assert
	if err == nil {
		t.Fatal("expected an error")
	}
	ok(t, os.Remove(filepath.Join(dir, journalName)))
	ok(t, open(t, dir).Close()) // not locked any more
}

func TestOpenDeletesTheLeftoverOfAnInterruptedCompaction(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	s := open(t, dir)
	populate(t, s)
	ok(t, s.Close())
	tmp := filepath.Join(dir, journalName+".tmp")
	ok(t, os.WriteFile(tmp, []byte("half a compacted journal"), 0o600))

	// Act
	s2 := open(t, dir)
	defer s2.Close()

	// Assert
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("temp journal still there: %v", err)
	}
	if n := len(must(s2.Sessions().List(context.Background(), "alice", 0))); n != 1 {
		t.Fatalf("sessions after the leftover was removed: %d", n)
	}
}

// ---- damaged journals ----

func TestOpenKeepsNothingFromAJournalWhoseOnlyLineIsTorn(t *testing.T) {
	for name, content := range map[string]string{
		"half a record":               `{"t":"thread","k":"x","v":{"Thread`,
		"a complete line, no newline": `{"t":"thread","k":"x","v":{}}`,
		"garbage":                     "not json at all\n",
		"a record without a type":     `{"k":"x","v":{}}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			dir := t.TempDir()
			ok(t, os.WriteFile(filepath.Join(dir, journalName), []byte(content), 0o600))

			// Act
			s, err := Open(dir)

			// Assert
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if !s.Truncated || journalSize(t, dir) != 0 || len(s.ThreadIDs()) != 0 {
				t.Fatalf("truncated=%v size=%d threads=%v", s.Truncated, journalSize(t, dir), s.ThreadIDs())
			}
		})
	}
}

func TestOpenRefusesAnInvalidLineFollowedByAValidOneAndReleasesTheDirectory(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	good := `{"t":"summary","k":"t1","v":null}`
	writeJournal(t, dir, good, "{broken", good, "")

	// Act
	_, err := Open(dir)

	// Assert
	if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "offset") {
		t.Fatalf("err = %v", err)
	}
	writeJournal(t, dir, good, good, "")
	ok(t, open(t, dir).Close())
}

func TestOpenRefusesARecordWhoseValueDoesNotMatchItsType(t *testing.T) {
	for _, typ := range []string{"thread", "run", "approval", "skills", "summary", "goal", "feedback", "superseded", "compactions", "msg", "events", "mem", "memoff"} {
		t.Run(typ, func(t *testing.T) {
			// Arrange: a well-formed line whose value is a string, which none of the types accepts.
			dir := t.TempDir()
			writeJournal(t, dir, fmt.Sprintf(`{"t":%q,"k":"k1","v":"not the right shape"}`, typ), "")

			// Act
			_, err := Open(dir)

			// Assert
			if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), typ+"/k1") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// ---- crash recovery ----

func TestRecoveryAddsATerminalEventToEachInterruptedRunAndKeepsItsLog(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	s := open(t, dir)
	defer s.Close()
	ctx := context.Background()
	mk := func(id, claimed string) *store.Run {
		th := must(s.CreateThread(ctx, "alice", id))
		r, _, err := s.CreateRun(ctx, store.NewRun{RunID: id, ThreadID: th.ID, UserLogin: "alice", Model: "m", Message: store.Message{Role: store.RoleUser, Content: "go"}})
		ok(t, err)
		if claimed != "" {
			_, err = s.ClaimRun(ctx, r.ID, claimed)
			ok(t, err)
		}
		return r
	}
	queued := mk("queued", "")
	silent := mk("silent", "rt") // running, no event yet
	mid := mk("mid", "rt")       // running, two events
	ok(t, s.AppendEvents(ctx, mid.ID, []store.Event{
		{Seq: 1, Type: agui.RunStarted, Data: json.RawMessage(`{"type":"RUN_STARTED"}`)},
		{Seq: 2, Type: agui.TextMessageContent, Data: json.RawMessage(`{"type":"TEXT_MESSAGE_CONTENT"}`)}}))
	finishedLog := mk("finished", "rt") // the log is complete, the process died before the status was written
	ok(t, s.AppendEvents(ctx, finishedLog.ID, []store.Event{
		{Seq: 1, Type: agui.RunStarted, Data: json.RawMessage(`{"type":"RUN_STARTED"}`)},
		{Seq: 2, Type: agui.RunFinished, Data: json.RawMessage(`{"type":"RUN_FINISHED"}`)}}))
	done := mk("done", "rt")
	ok(t, s.FinishRun(ctx, done.ID, "rt", store.StatusCompleted, "", ""))
	killed := killCopy(t, dir)

	// Act
	s2 := open(t, killed)
	defer s2.Close()

	// Assert
	if s2.Recovered != 4 {
		t.Fatalf("Recovered = %d, want 4", s2.Recovered)
	}
	want := map[string][]string{
		queued.ID:      {"1:RUN_STARTED", "2:RUN_ERROR"},
		silent.ID:      {"1:RUN_STARTED", "2:RUN_ERROR"},
		mid.ID:         {"1:RUN_STARTED", "2:TEXT_MESSAGE_CONTENT", "3:RUN_ERROR"},
		finishedLog.ID: {"1:RUN_STARTED", "2:RUN_FINISHED"},
		done.ID:        nil,
	}
	for id, evs := range want {
		got := eventTypes(must(s2.ListEvents(ctx, id, 0, 0)))
		if strings.Join(got, ",") != strings.Join(evs, ",") {
			t.Errorf("run %s events %v, want %v", id, got, evs)
		}
	}
	for _, id := range []string{queued.ID, silent.ID, mid.ID, finishedLog.ID} {
		r := must(s2.GetRun(ctx, id))
		if r.Status != store.StatusInterrupted || r.ErrorCode != "interrupted" || r.Error != InterruptedMessage || r.FinishedAt == nil {
			t.Errorf("run %s = %+v", id, r)
		}
	}
	if r := must(s2.GetRun(ctx, done.ID)); r.Status != store.StatusCompleted {
		t.Errorf("a finished run is left alone: %+v", r)
	}
	var ev struct{ Code, Message string }
	last, _, _ := s2.LastEvent(ctx, mid.ID)
	ok(t, json.Unmarshal(last.Data, &ev))
	if ev.Code != agui.CodeInterrupted || ev.Message != InterruptedMessage {
		t.Errorf("RUN_ERROR payload %+v", ev)
	}
}

func TestRecoveryDoesNotLeaveSandboxIdsOfDeadProcesses(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	s := open(t, dir)
	ctx := context.Background()
	th := must(s.CreateThread(ctx, "alice", "t"))
	ok(t, s.SetThreadSandbox(ctx, th.ID, "", "sbx-1"))
	populate(t, s) // journals every thread, the sandbox id included if it were persisted
	ok(t, s.Close())

	// Act
	s2 := open(t, dir)
	defer s2.Close()

	// Assert
	id, err := s2.ThreadSandbox(ctx, th.ID)
	if err != nil || id != "" {
		t.Fatalf("sandbox of the thread after a restart = %q, %v", id, err)
	}
}

func TestAClaimOfAQueuedRunOnADeletedThreadIsJournaledAsCancelled(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	s := open(t, dir)
	defer s.Close()
	ctx := context.Background()
	th := must(s.CreateThread(ctx, "alice", "t"))
	run, _, err := s.CreateRun(ctx, store.NewRun{RunID: "r1", ThreadID: th.ID, UserLogin: "alice", Model: "m", Message: store.Message{Role: store.RoleUser, Content: "go"}})
	ok(t, err)
	_, err = s.DeleteThread(ctx, "alice", th.ID)
	ok(t, err)

	// Act
	_, claimErr := s.ClaimRun(ctx, run.ID, "rt")
	killed := killCopy(t, dir)
	s2 := open(t, killed)
	defer s2.Close()

	// Assert
	if !errors.Is(claimErr, store.ErrNotClaimable) {
		t.Fatalf("claim: %v", claimErr)
	}
	if s2.Recovered != 0 {
		t.Fatalf("the run was cancelled by the claim, nothing to recover; Recovered = %d", s2.Recovered)
	}
}

// ---- compaction ----

func TestCompactKeepsEveryPersistedEntityAndLaterWritesStillReachTheJournal(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	s := open(t, dir)
	ctx := context.Background()
	threadID, runID := populate(t, s)
	before := snap(t, s, threadID, runID)

	// Act
	ok(t, s.compact())
	afterCompact := snap(t, s, threadID, runID)
	ok(t, s.AppendMessages(ctx, &store.Message{ThreadID: threadID, Role: store.RoleUser, Content: "after the compaction"}))
	pin := false
	_, err := s.UpdateThread(ctx, "alice", threadID, store.ThreadPatch{Pinned: &pin})
	ok(t, err)
	ok(t, s.Close())
	s2 := open(t, dir)
	defer s2.Close()

	// Assert
	if !sameJSON(before, afterCompact) {
		t.Fatalf("compact changed the live state\nbefore %+v\nafter  %+v", before, afterCompact)
	}
	msgs := must(s2.ListMessages(ctx, threadID))
	if len(msgs) != 5 || msgs[4].Content != "after the compaction" || must(s2.GetThread(ctx, "alice", threadID)).PinnedAt != nil {
		t.Fatalf("writes after the compaction: %d messages, thread %+v", len(msgs), must(s2.GetThread(ctx, "alice", threadID)))
	}
	if !sameJSON(before.Events, must(s2.ListEvents(ctx, runID, 0, 0))) || !sameJSON(before.Approval, must(s2.GetApproval(ctx, "ap1"))) {
		t.Fatal("events or approval lost")
	}
	if _, err := os.Stat(filepath.Join(dir, journalName+".tmp")); !os.IsNotExist(err) {
		t.Fatalf("temp journal left behind: %v", err)
	}
}

func TestCompactKeepsAllEventsOfALongRunInOrder(t *testing.T) {
	// Arrange: more events than one compaction page (500) and one journal line.
	dir := t.TempDir()
	s := open(t, dir)
	ctx := context.Background()
	th := must(s.CreateThread(ctx, "alice", "t"))
	run, _, err := s.CreateRun(ctx, store.NewRun{RunID: "long", ThreadID: th.ID, UserLogin: "alice", Model: "m", Message: store.Message{Role: store.RoleUser, Content: "go"}})
	ok(t, err)
	const n = 1234
	evs := make([]store.Event, n)
	for i := range evs {
		evs[i] = store.Event{Seq: int64(i + 1), Type: agui.TextMessageContent, Data: json.RawMessage(fmt.Sprintf(`{"i":%d}`, i+1))}
	}
	ok(t, s.AppendEvents(ctx, run.ID, evs))
	_, err = s.ClaimRun(ctx, run.ID, "rt")
	ok(t, err)
	ok(t, s.FinishRun(ctx, run.ID, "rt", store.StatusCompleted, "", ""))

	// Act
	ok(t, s.compact())
	ok(t, s.Close())
	s2 := open(t, dir)
	defer s2.Close()

	// Assert
	got := must(s2.ListEvents(ctx, run.ID, 0, 5000))
	if len(got) != n || must(s2.GetRun(ctx, run.ID)).LastSeq != n {
		t.Fatalf("%d events, run %+v", len(got), must(s2.GetRun(ctx, run.ID)))
	}
	for i, e := range got {
		if e.Seq != int64(i+1) || string(e.Data) != fmt.Sprintf(`{"i":%d}`, i+1) {
			t.Fatalf("event %d = %+v", i, e)
		}
	}
}

func TestCompactDropsDeletedThreadsOnTheNextOpenOnly(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	s := open(t, dir)
	ctx := context.Background()
	doomed := must(s.CreateThread(ctx, "alice", "doomed title"))
	must(s.CreateThread(ctx, "alice", "kept title"))
	_, err := s.DeleteThread(ctx, "alice", doomed.ID)
	ok(t, err)
	ok(t, s.Close())
	if raw := string(must(os.ReadFile(filepath.Join(dir, journalName)))); !strings.Contains(raw, doomed.ID) {
		t.Fatal("a deletion is a record, the thread is dropped at compaction")
	}

	// Act
	s2 := open(t, dir)
	defer s2.Close()

	// Assert
	raw := string(must(os.ReadFile(filepath.Join(dir, journalName))))
	if strings.Contains(raw, doomed.ID) || !strings.Contains(raw, "kept title") {
		t.Fatalf("journal after compaction:\n%s", raw)
	}
}

func TestCompactFailsCleanlyWhenTheDirectoryIsReadOnly(t *testing.T) {
	skipIfRoot(t)
	// Arrange
	dir := t.TempDir()
	s := open(t, dir)
	defer s.Close()
	threadID, _ := populate(t, s)
	ok(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	// Act
	err := s.compact()
	ok(t, os.Chmod(dir, 0o700))

	// Assert: the live journal is still the one writes go to.
	if err == nil {
		t.Fatal("expected an error")
	}
	ok(t, s.AppendMessages(context.Background(), &store.Message{ThreadID: threadID, Role: store.RoleUser, Content: "still journaled"}))
	ok(t, s.Close())
	s2 := open(t, dir)
	defer s2.Close()
	if msgs := must(s2.ListMessages(context.Background(), threadID)); msgs[len(msgs)-1].Content != "still journaled" {
		t.Fatalf("messages %v", msgs)
	}
}

// ---- journaling rules ----

func TestMessagesAddedBesideTheWrappersAreJournaledWhenTheRunEnds(t *testing.T) {
	// Arrange: AppendRunArtifacts creates an assistant message without going through AppendMessages.
	dir := t.TempDir()
	s := open(t, dir)
	defer s.Close()
	ctx := context.Background()
	th := must(s.CreateThread(ctx, "alice", "t"))
	run, _, err := s.CreateRun(ctx, store.NewRun{RunID: "r1", ThreadID: th.ID, UserLogin: "alice", Model: "m", Message: store.Message{Role: store.RoleUser, Content: "make a file"}})
	ok(t, err)
	_, err = s.ClaimRun(ctx, run.ID, "rt")
	ok(t, err)
	ok(t, s.AppendRunArtifacts(ctx, run.ID, []any{map[string]any{"path": "a.txt"}}))

	// Act
	ok(t, s.FinishRun(ctx, run.ID, "rt", store.StatusCompleted, "", ""))
	killed := killCopy(t, dir)
	s2 := open(t, killed)
	defer s2.Close()

	// Assert
	msgs := must(s2.ListMessages(ctx, th.ID))
	if len(msgs) != 2 || msgs[1].Role != store.RoleAssistant || msgs[1].RunID != run.ID {
		t.Fatalf("messages %+v", msgs)
	}
	if arts, _ := msgs[1].Metadata["artifacts"].([]any); len(arts) != 1 {
		t.Fatalf("metadata %v", msgs[1].Metadata)
	}
}

func TestFailedMutationsWriteNothingToTheJournal(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	s := open(t, dir)
	defer s.Close()
	ctx := context.Background()
	threadID, runID := populate(t, s)
	msgs := must(s.ListMessages(ctx, threadID))
	size := journalSize(t, dir)
	title := "x"
	unknown := "00000000-0000-0000-0000-000000000000"
	calls := map[string]func() error{
		"duplicate message id": func() error {
			return s.AppendMessages(ctx, &store.Message{ID: msgs[0].ID, ThreadID: threadID, Role: store.RoleUser, Content: "again"})
		},
		"feedback on an unknown message": func() error {
			return s.SetFeedback(ctx, "alice", unknown, store.Feedback{Sentiment: "like"})
		},
		"deleting unknown feedback": func() error { return s.DeleteFeedback(ctx, "alice", unknown) },
		"update of another user's thread": func() error {
			_, err := s.UpdateThread(ctx, "bob", threadID, store.ThreadPatch{Title: &title})
			return err
		},
		"delete of another user's thread": func() error { _, err := s.DeleteThread(ctx, "bob", threadID); return err },
		"run on an unknown thread": func() error {
			_, _, err := s.CreateRun(ctx, store.NewRun{RunID: "r9", ThreadID: unknown, UserLogin: "alice", Model: "m", Message: store.Message{Role: store.RoleUser, Content: "x"}})
			return err
		},
		"finish by the wrong runtime":      func() error { return s.FinishRun(ctx, runID, "other", store.StatusCompleted, "", "") },
		"claim of a finished run":          func() error { _, err := s.ClaimRun(ctx, runID, "rt"); return err },
		"reopening an unknown approval":    func() error { _, err := s.ReopenApproval(ctx, unknown); return err },
		"cancelling an unknown approval":   func() error { _, err := s.CancelPendingApproval(ctx, unknown, "r"); return err },
		"goal of another user's thread":    func() error { _, err := s.PutGoal(ctx, "bob", threadID, "g", 3); return err },
		"clearing a missing goal":          func() error { return s.ClearGoal(ctx, "bob", threadID) },
		"interaction mode of another user": func() error { return s.SetThreadInteractionMode(ctx, "bob", threadID, store.InteractionInteractive) },
		"seen by another user":             func() error { return s.MarkThreadSeen(ctx, "bob", threadID) },
		"cancel of an unknown run":         func() error { _, err := s.RequestCancel(ctx, unknown); return err },
	}

	for name, call := range calls {
		// Act
		err := call()

		// Assert
		if err == nil {
			t.Errorf("%s: no error", name)
		}
		if got := journalSize(t, dir); got != size {
			t.Errorf("%s: the journal grew from %d to %d bytes", name, size, got)
		}
	}
	if n := len(must(s.ListMessages(ctx, threadID))); n != len(msgs) {
		t.Errorf("messages: %d, want %d", n, len(msgs))
	}
}

func TestCallsThatChangeNothingAreNotJournaledAgain(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	s := open(t, dir)
	defer s.Close()
	ctx := context.Background()
	threadID, _ := populate(t, s)
	th := must(s.GetThread(ctx, "alice", threadID))
	size := journalSize(t, dir)

	// Act: the same state written again.
	ok(t, s.SetThreadPlan(ctx, threadID, th.Plan))
	ok(t, s.SetThreadSummary(ctx, threadID, store.ThreadSummary{Text: "sum", ThroughPosition: 1}))
	ok(t, s.SetThreadInteractionMode(ctx, "alice", threadID, th.InteractionMode))
	ok(t, s.MarkThreadSeen(ctx, "alice", threadID))
	grownBySeen := journalSize(t, dir) - size
	ok(t, s.SetThreadInteractionMode(ctx, "alice", threadID, th.InteractionMode))
	ok(t, s.SetThreadPlan(ctx, threadID, th.Plan))

	// Assert: only the first MarkThreadSeen changed a record (last seen time); everything else was identical.
	if grown := journalSize(t, dir) - size; grown != grownBySeen {
		t.Fatalf("journal grew by %d after the identical calls, %d of which were the seen mark", grown, grownBySeen)
	}
	ok(t, s.SetThreadSummary(ctx, threadID, store.ThreadSummary{Text: "sum", ThroughPosition: 1}))
	if journalSize(t, dir)-size != grownBySeen {
		t.Fatal("an identical summary was journaled again")
	}
}

// ---- close and sync policies ----

func TestEverySyncPolicyKeepsTheStateAcrossAClose(t *testing.T) {
	for name, opts := range map[string]Options{
		"always":          {Sync: SyncAlways},
		"never":           {Sync: SyncNever},
		"interval":        {Sync: SyncInterval},
		"fast interval":   {Sync: SyncInterval, SyncEvery: time.Millisecond},
		"negative period": {Sync: SyncInterval, SyncEvery: -time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			dir := t.TempDir()
			s, err := Open(dir, opts)
			ok(t, err)
			threadID, runID := populate(t, s)
			before := snap(t, s, threadID, runID)

			// Act
			ok(t, s.Close())
			s2 := open(t, dir)
			defer s2.Close()

			// Assert
			if !sameJSON(before, snap(t, s2, threadID, runID)) {
				t.Fatal("state differs after reopen")
			}
		})
	}
}

func TestSyncAlwaysWritesEachCallThroughToTheFile(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	s, err := Open(dir, Options{Sync: SyncAlways})
	ok(t, err)
	defer s.Close()
	size := journalSize(t, dir)

	// Act
	th := must(s.CreateThread(context.Background(), "alice", "t"))

	// Assert: a copy of the file taken while the store is still open already holds the thread.
	if journalSize(t, dir) <= size || !strings.Contains(string(must(os.ReadFile(filepath.Join(killCopy(t, dir), journalName)))), th.ID) {
		t.Fatal("the thread is not in the journal")
	}
}

func TestMutationsAfterCloseFailWithErrClosedAndCloseIsIdempotent(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	s := open(t, dir)
	ctx := context.Background()
	threadID, _ := populate(t, s)
	ok(t, s.Close())

	// Act
	closeAgain := s.Close()
	_, createErr := s.CreateThread(ctx, "alice", "late")
	appendErr := s.AppendMessages(ctx, &store.Message{ThreadID: threadID, Role: store.RoleUser, Content: "late"})

	// Assert
	if closeAgain != nil {
		t.Errorf("second Close: %v", closeAgain)
	}
	if !errors.Is(createErr, ErrClosed) || !errors.Is(appendErr, ErrClosed) {
		t.Errorf("create %v, append %v; want ErrClosed", createErr, appendErr)
	}
	s2 := open(t, dir)
	defer s2.Close()
	if got := must(s2.ListMessages(ctx, threadID)); len(got) != 4 {
		t.Errorf("the rejected message reached the disk: %d messages", len(got))
	}
}

func TestCloseJournalsStateThatNoWrapperSawYet(t *testing.T) {
	// Arrange: an assistant message created by AppendRunArtifacts, and no FinishRun before the process exits cleanly.
	dir := t.TempDir()
	s := open(t, dir)
	ctx := context.Background()
	th := must(s.CreateThread(ctx, "alice", "t"))
	run, _, err := s.CreateRun(ctx, store.NewRun{RunID: "r1", ThreadID: th.ID, UserLogin: "alice", Model: "m", Message: store.Message{Role: store.RoleUser, Content: "go"}})
	ok(t, err)
	_, err = s.ClaimRun(ctx, run.ID, "rt")
	ok(t, err)
	ok(t, s.AppendRunArtifacts(ctx, run.ID, []any{map[string]any{"path": "a.txt"}}))

	// Act
	ok(t, s.Close())
	s2 := open(t, dir)
	defer s2.Close()

	// Assert
	if msgs := must(s2.ListMessages(ctx, th.ID)); len(msgs) != 2 {
		t.Fatalf("messages %+v", msgs)
	}
}

// ---- concurrency ----

func TestConcurrentWritersAndReadersWithAFastSyncLoop(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	s, err := Open(dir, Options{Sync: SyncInterval, SyncEvery: time.Millisecond})
	ok(t, err)
	ctx := context.Background()
	const workers, rounds = 6, 20
	var ids [workers]string
	for i := range ids {
		ids[i] = must(s.CreateThread(ctx, "alice", fmt.Sprint("t", i))).ID
	}

	// Act
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for k := range rounds {
				run, _, err := s.CreateRun(ctx, store.NewRun{RunID: fmt.Sprint("w", i, "-", k), ThreadID: ids[i], UserLogin: "alice", Model: "m",
					Message: store.Message{Role: store.RoleUser, Content: fmt.Sprint("q", k)}})
				if err != nil {
					t.Error(err)
					return
				}
				if _, err := s.ClaimRun(ctx, run.ID, "rt"); err != nil {
					t.Error(err)
				}
				if err := s.FinishRun(ctx, run.ID, "rt", store.StatusCompleted, "", ""); err != nil {
					t.Error(err)
				}
				if err := s.SetThreadPlan(ctx, ids[i], []store.PlanItem{{ID: "1", Title: fmt.Sprint("step", k), Status: store.PlanPending}}); err != nil {
					t.Error(err)
				}
			}
		}()
		go func() { // readers never see a half-written state
			defer wg.Done()
			for range rounds {
				if _, err := s.Sessions().List(ctx, "alice", 0); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	ok(t, s.Close())
	s2 := open(t, dir)
	defer s2.Close()

	// Assert
	for i := range ids {
		th := must(s2.GetThread(ctx, "alice", ids[i]))
		if n := len(must(s2.ListMessages(ctx, ids[i]))); n != rounds || len(th.Plan) != 1 || th.Plan[0].Title != fmt.Sprint("step", rounds-1) {
			t.Fatalf("thread %d: %d messages, plan %+v", i, n, th.Plan)
		}
	}
	if s2.Recovered != 0 {
		t.Fatalf("Recovered = %d", s2.Recovered)
	}
}
