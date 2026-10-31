package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/7ar-chat/harness/core/store"
)

func coreSuiteMessages(t *testing.T, f Factory) {
	ctx := context.Background()

	t.Run("AppendMessagesStoresThemInOrderWithIncreasingPositions", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		a := coreNewThread(t, s, "alice", "")
		b := coreNewThread(t, s, "alice", "")
		run := coreQueue(t, s, a, "question")
		m1 := &store.Message{ThreadID: a.ID, RunID: run.ID, Role: store.RoleAssistant, Content: "first"}
		m2 := &store.Message{ThreadID: a.ID, RunID: run.ID, Role: store.RoleTool, ToolCallID: "c1", Content: "second"}
		other := &store.Message{ThreadID: b.ID, Role: store.RoleUser, Content: "elsewhere"}
		m3 := &store.Message{ThreadID: a.ID, RunID: run.ID, Role: store.RoleAssistant, Content: "third"}

		// Act
		err1 := s.AppendMessages(ctx, m1, m2)
		err2 := s.AppendMessages(ctx, other)
		err3 := s.AppendMessages(ctx, m3)
		got, listErr := s.ListMessages(ctx, a.ID)
		otherGot, otherErr := s.ListMessages(ctx, b.ID)

		// Assert
		for _, err := range []error{err1, err2, err3, listErr, otherErr} {
			if err != nil {
				t.Fatal(err)
			}
		}
		var contents []string
		for i, m := range got {
			contents = append(contents, m.Content)
			if m.Position <= 0 || (i > 0 && m.Position <= got[i-1].Position) {
				t.Errorf("positions must increase: %+v", got)
			}
			if m.ThreadID != a.ID || m.ID == "" || m.CreatedAt.IsZero() {
				t.Errorf("message %+v", m)
			}
		}
		if want := []string{"question", "first", "second", "third"}; !reflect.DeepEqual(contents, want) {
			t.Errorf("thread order %v, want %v", contents, want)
		}
		if len(otherGot) != 1 || otherGot[0].Content != "elsewhere" {
			t.Errorf("messages are listed per thread: %+v", otherGot)
		}
		if m1.ID == "" || m1.Position == 0 || m2.Position <= m1.Position {
			t.Errorf("AppendMessages reports the assigned id and position on its arguments: %+v %+v", m1, m2)
		}
	})

	t.Run("MessagesRoundTripEveryField", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := coreNewThread(t, s, "alice", "")
		run := coreQueue(t, s, th, "q")
		in := &store.Message{
			ID: "client-id-1", ThreadID: th.ID, RunID: run.ID, Role: store.RoleAssistant, Content: "answer",
			ToolCalls:      []store.ToolCall{{ID: "call-1", Type: "function", Function: store.ToolCallFunction{Name: "read_file", Arguments: `{"path":"a"}`}}},
			ToolCallID:     "call-0",
			Error:          "partial failure",
			Metadata:       map[string]any{"steer": true, "note": "n"},
			ReasoningItems: json.RawMessage(`[{"id":"rs_1","type":"reasoning"}]`),
			AuthorLogin:    "alice", AuthorKind: store.AuthorSupervisor, OnBehalfOf: "bob",
		}

		// Act
		err := s.AppendMessages(ctx, in)
		got, listErr := s.ListMessages(ctx, th.ID)

		// Assert
		if err != nil || listErr != nil || len(got) != 2 {
			t.Fatalf("append %v list %v (%d messages)", err, listErr, len(got))
		}
		m := got[1]
		if m.ID != "client-id-1" || m.RunID != run.ID || m.Role != store.RoleAssistant || m.Content != "answer" ||
			m.ToolCallID != "call-0" || m.Error != "partial failure" ||
			m.AuthorLogin != "alice" || m.AuthorKind != store.AuthorSupervisor || m.OnBehalfOf != "bob" {
			t.Errorf("scalar fields %+v", m)
		}
		if !reflect.DeepEqual(m.ToolCalls, in.ToolCalls) {
			t.Errorf("tool calls %+v", m.ToolCalls)
		}
		if !reflect.DeepEqual(m.Metadata, in.Metadata) || !m.IsSteer() {
			t.Errorf("metadata %+v", m.Metadata)
		}
		if !coreJSONEqual(t, m.ReasoningItems, in.ReasoningItems) {
			t.Errorf("reasoning items %s", m.ReasoningItems)
		}
		if got[0].ToolCalls != nil || got[0].Metadata != nil || len(got[0].ReasoningItems) != 0 || got[0].AuthorLogin != "" {
			t.Errorf("absent optional fields stay absent: %+v", got[0])
		}
	})

	t.Run("DuplicateMessageIdIsAConflictAndStoresNothing", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := coreNewThread(t, s, "alice", "")
		other := coreNewThread(t, s, "alice", "")
		if err := s.AppendMessages(ctx, &store.Message{ID: "m-1", ThreadID: th.ID, Role: store.RoleUser, Content: "original"}); err != nil {
			t.Fatal(err)
		}

		// Act
		dup := s.AppendMessages(ctx,
			&store.Message{ID: "m-new", ThreadID: th.ID, Role: store.RoleUser, Content: "fresh"},
			&store.Message{ID: "m-1", ThreadID: th.ID, Role: store.RoleUser, Content: "again"})
		inBatch := s.AppendMessages(ctx,
			&store.Message{ID: "twice", ThreadID: th.ID, Role: store.RoleUser, Content: "x"},
			&store.Message{ID: "twice", ThreadID: th.ID, Role: store.RoleUser, Content: "y"})
		elsewhere := s.AppendMessages(ctx, &store.Message{ID: "m-1", ThreadID: other.ID, Role: store.RoleUser, Content: "other thread"})

		// Assert
		if !errors.Is(dup, store.ErrConflict) {
			t.Errorf("reusing an id of the thread: %v, want ErrConflict", dup)
		}
		if !errors.Is(inBatch, store.ErrConflict) {
			t.Errorf("an id twice in one batch: %v, want ErrConflict", inBatch)
		}
		if elsewhere != nil {
			t.Errorf("ids are scoped to their thread: %v", elsewhere)
		}
		got, _ := s.ListMessages(ctx, th.ID)
		if len(got) != 1 || got[0].Content != "original" {
			t.Errorf("a refused batch is stored whole or not at all: %+v", got)
		}
	})

	t.Run("AppendMessagesRefusesDataThatCanNeverBeStored", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := coreNewThread(t, s, "alice", "")
		good := &store.Message{ThreadID: th.ID, Role: store.RoleUser, Content: "ok"}
		cases := map[string]*store.Message{
			"no role":        {ThreadID: th.ID, Content: "x"},
			"unknown role":   {ThreadID: th.ID, Role: "narrator", Content: "x"},
			"unknown thread": {ThreadID: uuid.NewString(), Role: store.RoleUser, Content: "x"},
			"no thread":      {Role: store.RoleUser, Content: "x"},
		}

		for name, bad := range cases {
			// Act
			err := s.AppendMessages(ctx, good, bad)

			// Assert
			if err == nil || !store.IsDataError(err) {
				t.Errorf("%s: got %v, want a permanent data error", name, err)
			}
			if errors.Is(err, store.ErrConflict) {
				t.Errorf("%s: this is not a conflict: %v", name, err)
			}
		}
		if got, _ := s.ListMessages(ctx, th.ID); len(got) != 0 {
			t.Errorf("refused batches store nothing: %+v", got)
		}
	})

	t.Run("AppendMessagesWithNothingIsANoop", func(t *testing.T) {
		// Arrange
		s := f(t).Store

		// Act
		err := s.AppendMessages(ctx)

		// Assert
		if err != nil {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("ListMessagesOfAnUnknownThreadIsEmpty", func(t *testing.T) {
		// Arrange
		s := f(t).Store

		// Act
		got, err := s.ListMessages(ctx, uuid.NewString())

		// Assert
		if err != nil || len(got) != 0 {
			t.Fatalf("got %+v, %v", got, err)
		}
	})

	t.Run("StoredMessagesAreSnapshots", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := coreNewThread(t, s, "alice", "")
		in := &store.Message{ThreadID: th.ID, Role: store.RoleUser, Content: "stored", Metadata: map[string]any{"k": "v"}}
		if err := s.AppendMessages(ctx, in); err != nil {
			t.Fatal(err)
		}
		first, _ := s.ListMessages(ctx, th.ID)

		// Act
		in.Content = "mutated by the caller"
		first[0].Content = "mutated by a reader"
		second, err := s.ListMessages(ctx, th.ID)

		// Assert
		if err != nil || len(second) != 1 || second[0].Content != "stored" {
			t.Fatalf("callers cannot change stored messages: %+v (%v)", second, err)
		}
	})

	t.Run("SanitizesNULAndInvalidUTF8LikePostgres", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		const dirty, clean = "a\x00b\xffc", "a\ufffdb\ufffdc"
		th := coreNewThread(t, s, "alice", "t\x00itle")
		in := coreInput(th.ID, "alice", "hello\x00")
		in.AutoTitle = "ignored"
		run, msg, runErr := s.CreateRun(ctx, in)
		if runErr != nil {
			t.Fatalf("CreateRun with NUL: %v", runErr)
		}
		title := "re\x00named"

		// Act
		appendErr := s.AppendMessages(ctx, &store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleTool, ToolCallID: "c\x00",
			Content: dirty, Error: "e\x00", Metadata: map[string]any{"k": "v\x00"},
			ToolCalls: []store.ToolCall{{ID: "c1", Type: "function", Function: store.ToolCallFunction{Name: "n\x00", Arguments: "{}"}}}})
		eventErr := s.AppendEvents(ctx, run.ID, []store.Event{{Seq: 1, Type: "TOOL\x00", Data: json.RawMessage(`{"type":"X","content":"x\u0000y"}`)}})
		renamed, renameErr := s.UpdateThread(ctx, "alice", th.ID, store.ThreadPatch{Title: &title})
		startErr := error(nil)
		if _, err := s.ClaimRun(ctx, run.ID, "rt"); err != nil {
			startErr = err
		}
		finishErr := s.FinishRun(ctx, run.ID, "rt", store.StatusFailed, "", "boom\x00")

		// Assert
		for _, err := range []error{appendErr, eventErr, renameErr, startErr, finishErr} {
			if err != nil {
				t.Fatalf("dirty text must be sanitized, not refused: %v", err)
			}
		}
		if msg.Content != "hello\ufffd" {
			t.Errorf("returned message %q", msg.Content)
		}
		if got := coreGetThread(t, s, "alice", th.ID); got.Title != "re\ufffdnamed" || renamed.Title != "re\ufffdnamed" {
			t.Errorf("thread title %q / %q", got.Title, renamed.Title)
		}
		msgs, _ := s.ListMessages(ctx, th.ID)
		if len(msgs) != 2 || msgs[0].Content != "hello\ufffd" {
			t.Fatalf("messages %+v", msgs)
		}
		m := msgs[1]
		if m.Content != clean || m.Error != "e\ufffd" || m.ToolCallID != "c\ufffd" || m.Metadata["k"] != "v\ufffd" ||
			len(m.ToolCalls) != 1 || m.ToolCalls[0].Function.Name != "n\ufffd" {
			t.Errorf("sanitized message %+v", m)
		}
		evs, err := s.ListEvents(ctx, run.ID, 0, 0)
		if err != nil || len(evs) != 1 || evs[0].Type != "TOOL\ufffd" || !coreJSONEqual(t, evs[0].Data, []byte(`{"type":"X","content":"x\ufffdy"}`)) {
			t.Errorf("sanitized events %+v (%v)", evs, err)
		}
		if got := coreGetRun(t, s, run.ID); got.Error != "boom\ufffd" {
			t.Errorf("run error %q", got.Error)
		}
	})
}

func coreSuiteEvents(t *testing.T, f Factory) {
	ctx := context.Background()

	ev := func(seq int64) store.Event {
		return store.Event{Seq: seq, Type: "TEXT", Data: json.RawMessage(`{"type":"TEXT","n":` + json.Number(itoa(seq)).String() + `}`)}
	}
	seqs := func(evs []store.Event) []int64 {
		out := make([]int64, 0, len(evs))
		for _, e := range evs {
			out = append(out, e.Seq)
		}
		return out
	}

	t.Run("AppendEventsKeepsTheLogOrderedBySeq", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		run := coreStart(t, s, coreNewThread(t, s, "alice", ""), "rt")

		// Act
		err1 := s.AppendEvents(ctx, run.ID, []store.Event{ev(3), ev(1)})
		err2 := s.AppendEvents(ctx, run.ID, []store.Event{ev(2), ev(4)})
		got, listErr := s.ListEvents(ctx, run.ID, 0, 100)

		// Assert
		if err1 != nil || err2 != nil || listErr != nil {
			t.Fatalf("errors %v %v %v", err1, err2, listErr)
		}
		if want := []int64{1, 2, 3, 4}; !reflect.DeepEqual(seqs(got), want) {
			t.Fatalf("seqs %v, want %v", seqs(got), want)
		}
		for _, e := range got {
			if e.Type != "TEXT" || !coreJSONEqual(t, e.Data, ev(e.Seq).Data) {
				t.Errorf("event %d round trip: %s %s", e.Seq, e.Type, e.Data)
			}
		}
	})

	t.Run("AppendEventsIsIdempotentPerSeq", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		run := coreStart(t, s, coreNewThread(t, s, "alice", ""), "rt")
		first := store.Event{Seq: 1, Type: "A", Data: json.RawMessage(`{"v":"first"}`)}
		if err := s.AppendEvents(ctx, run.ID, []store.Event{first, ev(2)}); err != nil {
			t.Fatal(err)
		}

		// Act
		retry := store.Event{Seq: 1, Type: "B", Data: json.RawMessage(`{"v":"retry"}`)}
		err := s.AppendEvents(ctx, run.ID, []store.Event{retry, ev(2), ev(3)})
		got, listErr := s.ListEvents(ctx, run.ID, 0, 100)

		// Assert
		if err != nil || listErr != nil {
			t.Fatalf("errors %v %v", err, listErr)
		}
		if want := []int64{1, 2, 3}; !reflect.DeepEqual(seqs(got), want) {
			t.Fatalf("a replayed batch adds only the new seqs: %v, want %v", seqs(got), want)
		}
		if got[0].Type != "A" || !coreJSONEqual(t, got[0].Data, first.Data) {
			t.Errorf("the first write of a seq wins: %s %s", got[0].Type, got[0].Data)
		}
	})

	t.Run("AppendEventsWithNothingIsANoop", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		run := coreStart(t, s, coreNewThread(t, s, "alice", ""), "rt")

		// Act
		err := s.AppendEvents(ctx, run.ID, nil)

		// Assert
		if _, ok, lastErr := s.LastEvent(ctx, run.ID); err != nil || lastErr != nil || ok {
			t.Fatalf("append %v last %v %v", err, lastErr, ok)
		}
		if got := coreGetRun(t, s, run.ID); got.LastSeq != 0 {
			t.Errorf("last seq %d", got.LastSeq)
		}
	})

	t.Run("ListEventsPagesWithAfterSeqAndLimit", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		run := coreStart(t, s, coreNewThread(t, s, "alice", ""), "rt")
		var batch []store.Event
		for i := int64(1); i <= 7; i++ {
			batch = append(batch, ev(i))
		}
		if err := s.AppendEvents(ctx, run.ID, batch); err != nil {
			t.Fatal(err)
		}

		// Act
		afterThree, err1 := s.ListEvents(ctx, run.ID, 3, 100)
		firstTwo, err2 := s.ListEvents(ctx, run.ID, 0, 2)
		nextTwo, err3 := s.ListEvents(ctx, run.ID, 2, 2)
		pastEnd, err4 := s.ListEvents(ctx, run.ID, 7, 10)
		defaultLimit, err5 := s.ListEvents(ctx, run.ID, 0, 0)
		unknown, err6 := s.ListEvents(ctx, uuid.NewString(), 0, 10)

		// Assert
		for _, err := range []error{err1, err2, err3, err4, err5, err6} {
			if err != nil {
				t.Fatal(err)
			}
		}
		if want := []int64{4, 5, 6, 7}; !reflect.DeepEqual(seqs(afterThree), want) {
			t.Errorf("after 3: %v", seqs(afterThree))
		}
		if want := []int64{1, 2}; !reflect.DeepEqual(seqs(firstTwo), want) {
			t.Errorf("limit 2: %v", seqs(firstTwo))
		}
		if want := []int64{3, 4}; !reflect.DeepEqual(seqs(nextTwo), want) {
			t.Errorf("after 2 limit 2: %v", seqs(nextTwo))
		}
		if len(pastEnd) != 0 || len(unknown) != 0 {
			t.Errorf("past the end %v, unknown run %v", seqs(pastEnd), seqs(unknown))
		}
		if len(defaultLimit) != 7 {
			t.Errorf("a non-positive limit means the default page: got %d events", len(defaultLimit))
		}
	})

	t.Run("LastEventIsTheHighestSeqAndTheRunTracksIt", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		run := coreStart(t, s, coreNewThread(t, s, "alice", ""), "rt")
		_, emptyOK, emptyErr := s.LastEvent(ctx, run.ID)
		_, unknownOK, unknownErr := s.LastEvent(ctx, uuid.NewString())

		// Act
		err1 := s.AppendEvents(ctx, run.ID, []store.Event{ev(5), ev(2)})
		afterFirst, okFirst, lastErr1 := s.LastEvent(ctx, run.ID)
		seqFirst := coreGetRun(t, s, run.ID).LastSeq
		err2 := s.AppendEvents(ctx, run.ID, []store.Event{ev(3)}) // an older seq arriving late never lowers LastSeq
		afterSecond, okSecond, lastErr2 := s.LastEvent(ctx, run.ID)
		seqSecond := coreGetRun(t, s, run.ID).LastSeq
		err3 := s.AppendEvents(ctx, run.ID, []store.Event{ev(9)})
		afterThird, _, _ := s.LastEvent(ctx, run.ID)
		seqThird := coreGetRun(t, s, run.ID).LastSeq

		// Assert
		for _, err := range []error{emptyErr, unknownErr, err1, err2, err3, lastErr1, lastErr2} {
			if err != nil {
				t.Fatal(err)
			}
		}
		if emptyOK || unknownOK {
			t.Errorf("a run without events has no last event")
		}
		if !okFirst || afterFirst.Seq != 5 || seqFirst != 5 {
			t.Errorf("after {5,2}: last %d (%v) run.LastSeq %d", afterFirst.Seq, okFirst, seqFirst)
		}
		if !okSecond || afterSecond.Seq != 5 || seqSecond != 5 {
			t.Errorf("after the late 3: last %d run.LastSeq %d", afterSecond.Seq, seqSecond)
		}
		if afterThird.Seq != 9 || seqThird != 9 || afterThird.Type != "TEXT" || !coreJSONEqual(t, afterThird.Data, ev(9).Data) {
			t.Errorf("after 9: %+v run.LastSeq %d", afterThird, seqThird)
		}
	})

	t.Run("EventLogsAreIsolatedPerRun", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		a := coreStart(t, s, coreNewThread(t, s, "alice", ""), "rt")
		b := coreStart(t, s, coreNewThread(t, s, "alice", ""), "rt")

		// Act
		errA := s.AppendEvents(ctx, a.ID, []store.Event{ev(1), ev(2)})
		errB := s.AppendEvents(ctx, b.ID, []store.Event{ev(1)})
		gotA, _ := s.ListEvents(ctx, a.ID, 0, 10)
		gotB, _ := s.ListEvents(ctx, b.ID, 0, 10)

		// Assert
		if errA != nil || errB != nil || len(gotA) != 2 || len(gotB) != 1 {
			t.Fatalf("errors %v %v; a %v b %v", errA, errB, seqs(gotA), seqs(gotB))
		}
		if coreGetRun(t, s, a.ID).LastSeq != 2 || coreGetRun(t, s, b.ID).LastSeq != 1 {
			t.Errorf("each run tracks its own LastSeq")
		}
	})
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
