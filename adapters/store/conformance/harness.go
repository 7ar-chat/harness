package conformance

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/7ar-chat/harness/core/store"
)

// Harness runs the suite of store.HarnessStore and store.HistoryStore: summaries, plans, titles, search, the pending
// question, goals, feedback, compactions, and edit-and-resend / undo (superseded messages).
func Harness(t *testing.T, f Factory) {
	t.Helper()
	t.Run("Summary", func(t *testing.T) { harnessSummary(t, f) })
	t.Run("Plan", func(t *testing.T) { harnessPlan(t, f) })
	t.Run("Title", func(t *testing.T) { harnessTitle(t, f) })
	t.Run("Search", func(t *testing.T) { harnessSearch(t, f) })
	t.Run("Question", func(t *testing.T) { harnessQuestion(t, f) })
	t.Run("Goal", func(t *testing.T) { harnessGoal(t, f) })
	t.Run("GoalRuns", func(t *testing.T) { harnessGoalRuns(t, f) })
	t.Run("Feedback", func(t *testing.T) { harnessFeedback(t, f) })
	t.Run("Compaction", func(t *testing.T) { harnessCompaction(t, f) })
	t.Run("Replace", func(t *testing.T) { harnessReplace(t, f) })
	t.Run("SupersedeFrom", func(t *testing.T) { harnessSupersede(t, f) })
	t.Run("Reopen", func(t *testing.T) { harnessReopen(t, f) })
	t.Run("Sanitize", func(t *testing.T) { harnessSanitize(t, f) })
}

// ---- helpers (prefix harness*, shared by the harness_*.go files) ----

func harnessNewRun(threadID, text string) store.NewRun {
	return store.NewRun{RunID: uuid.NewString(), ThreadID: threadID, UserLogin: "alice", Model: "m",
		Message: store.Message{ID: uuid.NewString(), Role: store.RoleUser, Content: text}}
}

func harnessThread(t *testing.T, s Store, login, title string) *store.Thread {
	t.Helper()
	th, err := s.CreateThread(context.Background(), login, title)
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	return th
}

func harnessFinish(t *testing.T, s Store, runID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.ClaimRun(ctx, runID, "rt"); err != nil {
		t.Fatalf("ClaimRun: %v", err)
	}
	if err := s.FinishRun(ctx, runID, "rt", store.StatusCompleted, "", ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
}

// harnessTurn runs one answered turn: the user message, the assistant reply, the run completed.
func harnessTurn(t *testing.T, s Store, threadID, text, reply string) (user, assistant *store.Message) {
	t.Helper()
	ctx := context.Background()
	run, user, err := s.CreateRun(ctx, harnessNewRun(threadID, text))
	if err != nil {
		t.Fatalf("CreateRun(%q): %v", text, err)
	}
	assistant = &store.Message{ID: uuid.NewString(), ThreadID: threadID, RunID: run.ID, Role: store.RoleAssistant, Content: reply}
	if err := s.AppendMessages(ctx, assistant); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	harnessFinish(t, s, run.ID)
	return user, assistant
}

// harnessPlanTurn is a turn whose assistant calls update_plan (callID) with one in-progress item titled title.
func harnessPlanTurn(t *testing.T, s Store, threadID, text, callID, title string, failed bool) *store.Message {
	t.Helper()
	ctx := context.Background()
	run, user, err := s.CreateRun(ctx, harnessNewRun(threadID, text))
	if err != nil {
		t.Fatalf("CreateRun(%q): %v", text, err)
	}
	call := store.ToolCall{ID: callID, Type: "function", Function: store.ToolCallFunction{Name: "update_plan",
		Arguments: fmt.Sprintf(`{"items":[{"id":"1","title":%q,"status":"in_progress"}]}`, title)}}
	result := &store.Message{ThreadID: threadID, RunID: run.ID, Role: store.RoleTool, ToolCallID: callID, Content: "Plan updated"}
	if failed {
		result.Error = "rejected"
	}
	if err := s.AppendMessages(ctx,
		&store.Message{ID: uuid.NewString(), ThreadID: threadID, RunID: run.ID, Role: store.RoleAssistant, ToolCalls: []store.ToolCall{call}},
		result); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	harnessFinish(t, s, run.ID)
	return user
}

func harnessContents(t *testing.T, s Store, threadID string) []string {
	t.Helper()
	msgs, err := s.ListMessages(context.Background(), threadID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Content
	}
	return out
}

func harnessPlanTitles(t *testing.T, s Store, threadID string) []string {
	t.Helper()
	th, err := s.GetThread(context.Background(), "alice", threadID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	var out []string
	for _, it := range th.Plan {
		out = append(out, it.Title)
	}
	return out
}

func harnessThreadIDs(threads []*store.Thread) []string {
	out := make([]string, len(threads))
	for i, th := range threads {
		out[i] = th.ID
	}
	return out
}

func harnessSorted(ids ...string) []string {
	out := slices.Clone(ids)
	slices.Sort(out)
	return out
}

func harnessEqual(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s = %q, want %q", what, got, want)
	}
}

func harnessCursor(th *store.Thread) *store.ThreadCursor {
	return &store.ThreadCursor{Pinned: th.PinnedAt != nil, CreatedAt: th.CreatedAt, ID: th.ID}
}

// ---- summary, plan, title ----

func harnessSummary(t *testing.T, f Factory) {
	ctx := context.Background()
	t.Run("RoundTripsAndClearsWithEmptyText", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		fresh, freshErr := s.GetThreadSummary(ctx, th.ID)

		// Act
		setErr := s.SetThreadSummary(ctx, th.ID, store.ThreadSummary{Text: "the story so far", ThroughPosition: 7})
		set, _ := s.GetThreadSummary(ctx, th.ID)
		clearErr := s.SetThreadSummary(ctx, th.ID, store.ThreadSummary{})
		cleared, _ := s.GetThreadSummary(ctx, th.ID)

		// Assert
		if freshErr != nil || fresh != (store.ThreadSummary{}) {
			t.Errorf("a thread without summary: %+v, %v", fresh, freshErr)
		}
		if setErr != nil || set != (store.ThreadSummary{Text: "the story so far", ThroughPosition: 7}) {
			t.Errorf("after set: %+v, %v", set, setErr)
		}
		if clearErr != nil || cleared != (store.ThreadSummary{}) {
			t.Errorf("an empty text must clear the summary: %+v, %v", cleared, clearErr)
		}
	})
	t.Run("UnknownThreadIsNotFound", func(t *testing.T) {
		// Arrange
		s := f(t).Store

		// Act
		_, err := s.GetThreadSummary(ctx, uuid.NewString())

		// Assert
		if err != store.ErrNotFound {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
}

func harnessPlan(t *testing.T, f Factory) {
	ctx := context.Background()
	t.Run("RoundTripsOnTheThread", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "t")
		items := []store.PlanItem{{ID: "1", Title: "Load the data", Status: store.PlanCompleted}, {ID: "2", Title: "Chart it", Status: store.PlanInProgress}}

		// Act
		err := s.SetThreadPlan(ctx, th.ID, items)
		got, _ := s.GetThread(ctx, "alice", th.ID)
		clearErr := s.SetThreadPlan(ctx, th.ID, nil)
		cleared, _ := s.GetThread(ctx, "alice", th.ID)

		// Assert
		if err != nil || !slices.Equal(got.Plan, items) {
			t.Errorf("plan = %+v, %v; want %+v", got.Plan, err, items)
		}
		if clearErr != nil || len(cleared.Plan) != 0 {
			t.Errorf("a nil plan must clear it: %+v, %v", cleared.Plan, clearErr)
		}
	})
}

func harnessTitle(t *testing.T, f Factory) {
	ctx := context.Background()
	t.Run("GeneratedTitleReplacesAnAutoTitleOnce", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "")
		in := harnessNewRun(th.ID, "hello there")
		in.AutoTitle = "hello there"
		_, _, runErr := s.CreateRun(ctx, in)
		derived, _ := s.GetThread(ctx, "alice", th.ID)

		// Act
		first, firstErr := s.SetGeneratedTitle(ctx, th.ID, "A friendly greeting")
		second, secondErr := s.SetGeneratedTitle(ctx, th.ID, "Another title")

		// Assert
		got, _ := s.GetThread(ctx, "alice", th.ID)
		if runErr != nil || derived.Title != "hello there" || derived.TitleSource != store.TitleAuto {
			t.Fatalf("auto title: %+v, %v", derived, runErr)
		}
		if !first || firstErr != nil || second || secondErr != nil {
			t.Errorf("first %v %v, second %v %v; want true then false", first, firstErr, second, secondErr)
		}
		if got.Title != "A friendly greeting" || got.TitleSource != store.TitleGenerated {
			t.Errorf("thread = %q (%s)", got.Title, got.TitleSource)
		}
	})
	t.Run("UserTitlesAreNeverOverwritten", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		created := harnessThread(t, s, "alice", "Mine")
		renamed := harnessThread(t, s, "alice", "")
		newTitle := "Renamed"
		if _, err := s.UpdateThread(ctx, "alice", renamed.ID, store.ThreadPatch{Title: &newTitle}); err != nil {
			t.Fatal(err)
		}

		// Act
		okCreated, _ := s.SetGeneratedTitle(ctx, created.ID, "Generated")
		okRenamed, _ := s.SetGeneratedTitle(ctx, renamed.ID, "Generated")

		// Assert
		a, _ := s.GetThread(ctx, "alice", created.ID)
		b, _ := s.GetThread(ctx, "alice", renamed.ID)
		if okCreated || okRenamed {
			t.Errorf("generated over a user title: created %v renamed %v", okCreated, okRenamed)
		}
		if a.Title != "Mine" || a.TitleSource != store.TitleUser || b.Title != "Renamed" || b.TitleSource != store.TitleUser {
			t.Errorf("titles = %q (%s), %q (%s)", a.Title, a.TitleSource, b.Title, b.TitleSource)
		}
	})
	t.Run("DeletedAndUnknownThreadsAreSkipped", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := harnessThread(t, s, "alice", "")
		if _, err := s.DeleteThread(ctx, "alice", th.ID); err != nil {
			t.Fatal(err)
		}

		// Act
		deleted, deletedErr := s.SetGeneratedTitle(ctx, th.ID, "Generated")
		unknown, unknownErr := s.SetGeneratedTitle(ctx, uuid.NewString(), "Generated")

		// Assert
		if deleted || deletedErr != nil || unknown || unknownErr != nil {
			t.Errorf("deleted %v %v, unknown %v %v; want false, nil", deleted, deletedErr, unknown, unknownErr)
		}
	})
}

// ---- search ----

func harnessSearch(t *testing.T, f Factory) {
	ctx := context.Background()
	t.Run("MatchesTitlesAndVisibleMessagesCaseInsensitively", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		byTitle := harnessThread(t, s, "alice", "Quarterly REPORT")
		byUser := harnessThread(t, s, "alice", "u")
		harnessTurn(t, s, byUser.ID, "need the Budget please", "ok")
		byAssistant := harnessThread(t, s, "alice", "a")
		harnessTurn(t, s, byAssistant.ID, "hello", "BUDGET approved")
		byTool := harnessThread(t, s, "alice", "tool")
		run, _, err := s.CreateRun(ctx, harnessNewRun(byTool.ID, "hi"))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AppendMessages(ctx, &store.Message{ThreadID: byTool.ID, RunID: run.ID, Role: store.RoleTool, ToolCallID: "c1", Content: "budget secret"}); err != nil {
			t.Fatal(err)
		}
		harnessFinish(t, s, run.ID)

		// Act
		budget, err1 := s.SearchThreads(ctx, "alice", "bUdGeT", 10, nil)
		report, err2 := s.SearchThreads(ctx, "alice", "report", 10, nil)
		none, err3 := s.SearchThreads(ctx, "alice", "zebra", 10, nil)

		// Assert
		if err1 != nil || err2 != nil || err3 != nil {
			t.Fatalf("errors: %v %v %v", err1, err2, err3)
		}
		harnessEqual(t, "budget", harnessSorted(harnessThreadIDs(budget)...), harnessSorted(byUser.ID, byAssistant.ID))
		harnessEqual(t, "report", harnessThreadIDs(report), []string{byTitle.ID})
		if len(none) != 0 {
			t.Errorf("zebra matched %v", harnessThreadIDs(none))
		}
	})
	t.Run("TreatsTheSearchAsLiteralText", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		pct := harnessThread(t, s, "alice", "Growth 100% plan")
		harnessThread(t, s, "alice", "100 percent")
		under := harnessThread(t, s, "alice", "a_c")
		harnessThread(t, s, "alice", "abc")

		// Act
		percent, _ := s.SearchThreads(ctx, "alice", "100%", 10, nil)
		underscore, _ := s.SearchThreads(ctx, "alice", "a_c", 10, nil)

		// Assert
		harnessEqual(t, "100%", harnessThreadIDs(percent), []string{pct.ID})
		harnessEqual(t, "a_c", harnessThreadIDs(underscore), []string{under.ID})
	})
	t.Run("IgnoresHiddenDeletedAndForeignThreads", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		mine := harnessThread(t, s, "alice", "mine budget")
		hidden := harnessThread(t, s, "alice", "h")
		draft, _ := harnessTurn(t, s, hidden.ID, "budget draft", "noted")
		deleted := harnessThread(t, s, "alice", "deleted budget")
		if _, err := s.DeleteThread(ctx, "alice", deleted.ID); err != nil {
			t.Fatal(err)
		}
		harnessThread(t, s, "bob", "bob budget")
		if err := s.(store.HistoryStore).SupersedeFrom(ctx, "alice", hidden.ID, draft.ID); err != nil {
			t.Fatal(err)
		}

		// Act
		got, err := s.SearchThreads(ctx, "alice", "budget", 10, nil)

		// Assert
		if err != nil {
			t.Fatal(err)
		}
		harnessEqual(t, "budget", harnessThreadIDs(got), []string{mine.ID})
	})
	t.Run("EmptySearchListsEveryThread", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		a := harnessThread(t, s, "alice", "one")
		b := harnessThread(t, s, "alice", "two")
		harnessThread(t, s, "bob", "three")

		// Act
		got, err := s.SearchThreads(ctx, "alice", "", 10, nil)

		// Assert
		if err != nil {
			t.Fatal(err)
		}
		harnessEqual(t, "threads", harnessSorted(harnessThreadIDs(got)...), harnessSorted(a.ID, b.ID))
	})
	t.Run("PagesNewestFirstWithPinnedOnTop", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		var threads []*store.Thread
		for _, title := range []string{"match 1", "match 2", "match 3"} {
			threads = append(threads, harnessThread(t, s, "alice", title))
			b.Advance(t, time.Minute)
		}
		harnessThread(t, s, "alice", "other")
		oldest, middle, newest := threads[0], threads[1], threads[2]

		// Act
		page1, err1 := s.SearchThreads(ctx, "alice", "match", 2, nil)
		var page2 []*store.Thread
		var err2 error
		if len(page1) == 2 {
			page2, err2 = s.SearchThreads(ctx, "alice", "match", 2, harnessCursor(page1[1]))
		}
		pin := true
		if _, err := s.UpdateThread(ctx, "alice", oldest.ID, store.ThreadPatch{Pinned: &pin}); err != nil {
			t.Fatal(err)
		}
		pinned, err3 := s.SearchThreads(ctx, "alice", "match", 10, nil)

		// Assert
		if err1 != nil || err2 != nil || err3 != nil {
			t.Fatalf("errors: %v %v %v", err1, err2, err3)
		}
		harnessEqual(t, "page 1", harnessThreadIDs(page1), []string{newest.ID, middle.ID})
		harnessEqual(t, "page 2", harnessThreadIDs(page2), []string{oldest.ID})
		harnessEqual(t, "pinned first", harnessThreadIDs(pinned), []string{oldest.ID, newest.ID, middle.ID})
	})
}
