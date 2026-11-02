package memstore_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/core/store"
)

func exportRun(t *testing.T, s *memstore.Store, threadID string) *store.Run {
	t.Helper()
	run, _, err := s.CreateRun(context.Background(), store.NewRun{RunID: uuid.NewString(), ThreadID: threadID, UserLogin: "alice",
		Model: "m", Message: store.Message{ID: uuid.NewString(), Role: store.RoleUser, Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestExportRunReturnsACopyOfTheRun(t *testing.T) {
	// Arrange
	ctx := context.Background()
	s := memstore.New()
	th, _ := s.CreateThread(ctx, "alice", "t")
	run := exportRun(t, s, th.ID)

	// Act
	got, ok := s.ExportRun(run.ID)
	got.Model = "changed"
	_, unknown := s.ExportRun(uuid.NewString())

	// Assert
	again, _ := s.ExportRun(run.ID)
	if !ok || again.ID != run.ID || again.ThreadID != th.ID || again.Model != "m" || again.Status != store.StatusQueued {
		t.Errorf("exported run %+v (ok %v)", again, ok)
	}
	if unknown {
		t.Error("an unknown run was exported")
	}
}

func TestExportThreadOfAnUnknownThreadIsNotFound(t *testing.T) {
	// Arrange
	s := memstore.New()

	// Act
	_, msgs, ok := s.ExportThread(uuid.NewString())

	// Assert
	if ok || msgs != nil {
		t.Errorf("exported an unknown thread: %v %v", ok, msgs)
	}
}

func TestExportedThreadCanBeImportedIntoAnotherStore(t *testing.T) {
	// A thread exported with its messages and events restores identically in a fresh store (what a journal replay does).
	// Arrange
	ctx := context.Background()
	src := memstore.New()
	th, _ := src.CreateThread(ctx, "alice", "t")
	in := store.NewRun{RunID: uuid.NewString(), ThreadID: th.ID, UserLogin: "alice", Model: "m", Skills: []string{"docx"},
		Message: store.Message{ID: uuid.NewString(), Role: store.RoleUser, Content: "hi"}}
	run, _, err := src.CreateRun(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := src.AppendMessages(ctx, &store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleAssistant, Content: "answer"}); err != nil {
		t.Fatal(err)
	}
	if err := src.AppendEvents(ctx, run.ID, []store.Event{{Seq: 1, Type: "A", Data: json.RawMessage(`{"n":1}`)}, {Seq: 2, Type: "B", Data: json.RawMessage(`{"n":2}`)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.PutGoal(ctx, "alice", th.ID, "ship it", 3); err != nil {
		t.Fatal(err)
	}
	st, msgs, ok := src.ExportThread(th.ID)
	events, _ := src.ListEvents(ctx, run.ID, 0, 10)
	exportedRun, _ := src.ExportRun(run.ID)
	dst := memstore.New()

	// Act
	dst.ImportThread(st)
	dst.ImportMessages(msgs)
	dst.ImportEvents(run.ID, events)

	// Assert
	if !ok || len(st.Runs) != 1 || st.Runs[0].ID != run.ID || len(msgs) != 2 || st.Goal == nil || st.Goal.Text != "ship it" {
		t.Fatalf("exported state %+v messages %+v", st, msgs)
	}
	if got := st.RunSkills[run.ID]; len(got) != 1 || got[0] != "docx" {
		t.Errorf("exported run skills %v", st.RunSkills)
	}
	gotMsgs, _ := dst.ListMessages(ctx, th.ID)
	if len(gotMsgs) != 2 || gotMsgs[0].Content != "hi" || gotMsgs[1].Content != "answer" || gotMsgs[1].Position != msgs[1].Position {
		t.Errorf("imported messages %+v", gotMsgs)
	}
	gotEvents, _ := dst.ListEvents(ctx, run.ID, 0, 10)
	if len(gotEvents) != 2 || gotEvents[0].Seq != 1 || gotEvents[1].Type != "B" {
		t.Errorf("imported events %+v", gotEvents)
	}
	if got, _ := dst.ExportRun(run.ID); got.ID != exportedRun.ID || got.ThreadID != th.ID {
		t.Errorf("imported run %+v", got)
	}
	if skills, err := dst.RunSkills(ctx, run.ID); err != nil || len(skills) != 1 || skills[0] != "docx" {
		t.Errorf("imported run skills %v (%v)", skills, err)
	}
}

func TestImportMessagesIgnoresPresentMessagesAndAdvancesThePositions(t *testing.T) {
	// Arrange
	ctx := context.Background()
	s := memstore.New()
	th, _ := s.CreateThread(ctx, "alice", "t")
	run := exportRun(t, s, th.ID)
	present, _ := s.ListMessages(ctx, th.ID)
	dup := *present[0]
	dup.Content = "overwritten?"
	fresh := &store.Message{ID: uuid.NewString(), ThreadID: th.ID, RunID: run.ID, Role: store.RoleAssistant, Content: "restored", Position: 40}

	// Act
	s.ImportMessages([]*store.Message{&dup, fresh})
	if err := s.AppendMessages(ctx, &store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleAssistant, Content: "next"}); err != nil {
		t.Fatal(err)
	}

	// Assert
	msgs, _ := s.ListMessages(ctx, th.ID)
	if len(msgs) != 3 || msgs[0].Content != "hi" || msgs[1].Content != "restored" || msgs[1].Position != 40 {
		t.Fatalf("messages %+v", msgs)
	}
	if msgs[2].Content != "next" || msgs[2].Position <= 40 {
		t.Errorf("a new message must come after the imported ones: %+v", msgs[2])
	}
}

func TestImportEventsIgnoresKnownSeqsAndKeepsThemOrdered(t *testing.T) {
	// Arrange
	ctx := context.Background()
	s := memstore.New()
	th, _ := s.CreateThread(ctx, "alice", "t")
	run := exportRun(t, s, th.ID)
	if err := s.AppendEvents(ctx, run.ID, []store.Event{{Seq: 2, Type: "B", Data: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}

	// Act
	s.ImportEvents(run.ID, []store.Event{{Seq: 3, Type: "C", Data: json.RawMessage(`{}`)}, {Seq: 2, Type: "dup", Data: json.RawMessage(`{}`)},
		{Seq: 1, Type: "A", Data: json.RawMessage(`{}`)}, {Seq: 1, Type: "dup", Data: json.RawMessage(`{}`)}})

	// Assert
	got, _ := s.ListEvents(ctx, run.ID, 0, 10)
	var types []string
	for _, e := range got {
		types = append(types, e.Type)
	}
	if len(types) != 3 || types[0] != "A" || types[1] != "B" || types[2] != "C" {
		t.Errorf("events %v, want [A B C]", types)
	}
}

func TestImportThreadRestoresTheDurableStateOfExportThread(t *testing.T) {
	// Everything ExportThread captures (approvals, summary, goal, feedback, hidden messages, compactions) comes back.
	// Arrange
	ctx := context.Background()
	src := memstore.New()
	th, _ := src.CreateThread(ctx, "alice", "t")
	in := store.NewRun{RunID: uuid.NewString(), ThreadID: th.ID, UserLogin: "alice", Model: "m", InteractionMode: store.InteractionInteractive,
		Message: store.Message{ID: uuid.NewString(), Role: store.RoleUser, Content: "first"}}
	run, first, err := src.CreateRun(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := src.AppendMessages(ctx, &store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleAssistant, Content: "answer", ToolCalls: []store.ToolCall{
		{ID: "c1", Type: "function", Function: store.ToolCallFunction{Name: "write_file", Arguments: "{}"}}}}); err != nil {
		t.Fatal(err)
	}
	approval := &store.Approval{ThreadID: th.ID, RunID: run.ID, ToolCallID: "c1", ToolName: "write_file"}
	if err := src.CreateApproval(ctx, approval); err != nil {
		t.Fatal(err)
	}
	msgs, _ := src.ListMessages(ctx, th.ID)
	if err := src.SetFeedback(ctx, "alice", msgs[1].ID, store.Feedback{Sentiment: "like", Comment: "good"}); err != nil {
		t.Fatal(err)
	}
	if err := src.SetThreadSummary(ctx, th.ID, store.ThreadSummary{Text: "sum", ThroughPosition: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.PutGoal(ctx, "alice", th.ID, "ship it", 3); err != nil {
		t.Fatal(err)
	}
	if err := src.RecordCompaction(ctx, store.Compaction{ThreadID: th.ID, RunID: run.ID, Trigger: "manual"}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.ClaimRun(ctx, run.ID, "rt"); err != nil {
		t.Fatal(err)
	}
	if err := src.FinishRun(ctx, run.ID, "rt", store.StatusCompleted, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := src.SupersedeFrom(ctx, "alice", th.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	st, all, _ := src.ExportThread(th.ID)
	dst := memstore.New()

	// Act
	dst.ImportThread(st)
	dst.ImportMessages(all)

	// Assert
	if len(st.Superseded) != 2 || len(st.Approvals) != 1 || st.Goal == nil || len(st.Feedback) != 1 || len(st.Compaction) != 1 {
		t.Fatalf("exported state %+v", st)
	}
	if g, _ := dst.ThreadGoal(ctx, th.ID); g == nil || g.Text != "ship it" {
		t.Errorf("imported goal %+v", g)
	}
	if got, err := dst.GetApproval(ctx, approval.ID); err != nil || got.ToolCallID != "c1" {
		t.Errorf("imported approval %+v (%v)", got, err)
	}
	if visible, _ := dst.ListMessages(ctx, th.ID); len(visible) != 0 {
		t.Errorf("superseded messages are visible after the import: %+v", visible)
	}
	if fb, _ := dst.ListFeedback(ctx, th.ID); len(fb) != 1 || fb[msgs[1].ID].Comment != "good" {
		t.Errorf("imported feedback %+v", fb)
	}
	if got, err := dst.GetThread(ctx, "alice", th.ID); err != nil || got.Title != "t" {
		t.Errorf("imported thread %+v (%v)", got, err)
	}
	if again, _, _ := dst.ExportThread(th.ID); len(again.Compaction) != 1 || len(again.Superseded) != 2 {
		t.Errorf("re-exported state %+v", again)
	}

	// Act: importing a state without goal, summary and feedback clears them
	st.Goal, st.Summary, st.Feedback, st.Compaction = nil, nil, nil, nil
	dst.ImportThread(st)

	// Assert
	if g, _ := dst.ThreadGoal(ctx, th.ID); g != nil {
		t.Errorf("goal %+v", g)
	}
	if fb, _ := dst.ListFeedback(ctx, th.ID); len(fb) != 0 {
		t.Errorf("feedback %+v", fb)
	}
	if again, _, _ := dst.ExportThread(th.ID); len(again.Compaction) != 0 {
		t.Errorf("compactions %+v", again.Compaction)
	}
}
