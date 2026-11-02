package conformance

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/7ar-chat/harness/core/store"
)

func coreSuiteRuns(t *testing.T, f Factory) {
	ctx := context.Background()

	t.Run("CreateRunQueuesARunAndStoresItsMessage", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := coreNewThread(t, s, "alice", "")
		in := coreInput(th.ID, "alice", "hello")
		in.Message.AuthorLogin, in.Message.AuthorKind = "alice", store.AuthorUser
		in.ReasoningEffort = "high"

		// Act
		run, msg, err := s.CreateRun(ctx, in)

		// Assert
		if err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		if run.ID != in.RunID || run.ThreadID != th.ID || run.UserLogin != "alice" || run.Model != "model-1" ||
			run.Status != store.StatusQueued || run.Kind != store.KindChat || run.ReasoningEffort != "high" {
			t.Errorf("run %+v", run)
		}
		if run.RuntimeID != "" || run.HeartbeatAt != nil || run.StartedAt != nil || run.FinishedAt != nil ||
			run.CancelRequested || run.LastSeq != 0 || run.CreatedAt.IsZero() {
			t.Errorf("a queued run has not started: %+v", run)
		}
		if msg == nil || msg.ID == "" || msg.ThreadID != th.ID || msg.RunID != run.ID || msg.Role != store.RoleUser ||
			msg.Content != "hello" || msg.Position <= 0 || msg.CreatedAt.IsZero() {
			t.Fatalf("message %+v", msg)
		}
		listed, err := s.ListMessages(ctx, th.ID)
		if err != nil || len(listed) != 1 || listed[0].ID != msg.ID || listed[0].RunID != run.ID ||
			listed[0].AuthorLogin != "alice" || listed[0].AuthorKind != store.AuthorUser || listed[0].Position != msg.Position {
			t.Errorf("stored messages %+v (%v)", listed, err)
		}
		got := coreGetThread(t, s, "alice", th.ID)
		if got.ActiveRun == nil || got.ActiveRun.ID != run.ID || got.ActiveRun.Status != store.StatusQueued {
			t.Errorf("thread active run %+v", got.ActiveRun)
		}
	})

	t.Run("CreateRunWithoutMessageRoleStoresNoMessage", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := coreNewThread(t, s, "alice", "")
		in := store.NewRun{RunID: uuid.NewString(), ThreadID: th.ID, UserLogin: "alice", Model: "m", Kind: store.KindCompact,
			AutoTitle: "from nothing"}

		// Act
		run, msg, err := s.CreateRun(ctx, in)

		// Assert
		if err != nil || run == nil || msg != nil {
			t.Fatalf("run %+v msg %+v err %v", run, msg, err)
		}
		if run.Kind != store.KindCompact {
			t.Errorf("kind %q", run.Kind)
		}
		if msgs, _ := s.ListMessages(ctx, th.ID); len(msgs) != 0 {
			t.Errorf("stored %d messages", len(msgs))
		}
		if got := coreGetThread(t, s, "alice", th.ID); got.Title != "from nothing" {
			t.Errorf("the auto title applies without a message: %q", got.Title)
		}
	})

	t.Run("CreateRunOnUnknownThreadIsNotFound", func(t *testing.T) {
		// Arrange
		s := f(t).Store

		// Act
		_, _, err := s.CreateRun(ctx, coreInput(uuid.NewString(), "alice", "x"))

		// Assert
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})

	t.Run("OnlyOneRunIsActivePerThread", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := coreNewThread(t, s, "alice", "")
		other := coreNewThread(t, s, "alice", "")
		first := coreQueue(t, s, th, "one")

		// Act
		_, _, whileQueued := s.CreateRun(ctx, coreInput(th.ID, "alice", "two"))
		_, claimErr := s.ClaimRun(ctx, first.ID, "rt")
		_, _, whileRunning := s.CreateRun(ctx, coreInput(th.ID, "alice", "three"))
		otherRun, _, otherErr := s.CreateRun(ctx, coreInput(other.ID, "alice", "elsewhere"))
		finishErr := s.FinishRun(ctx, first.ID, "rt", store.StatusCompleted, "", "")
		next, _, afterFinish := s.CreateRun(ctx, coreInput(th.ID, "alice", "four"))

		// Assert
		if !errors.Is(whileQueued, store.ErrRunActive) || !errors.Is(whileRunning, store.ErrRunActive) {
			t.Errorf("while queued %v, while running %v, want ErrRunActive", whileQueued, whileRunning)
		}
		if claimErr != nil || finishErr != nil || otherErr != nil || otherRun == nil || afterFinish != nil || next == nil {
			t.Errorf("claim %v finish %v other %v after %v", claimErr, finishErr, otherErr, afterFinish)
		}
		msgs, _ := s.ListMessages(ctx, th.ID)
		if len(msgs) != 2 || msgs[0].Content != "one" || msgs[1].Content != "four" {
			t.Errorf("a refused run stores nothing; messages %+v", msgs)
		}
	})

	t.Run("RefusedRunsChangeNothing", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := coreNewThread(t, s, "alice", "")
		first := store.NewRun{RunID: uuid.NewString(), ThreadID: th.ID, UserLogin: "alice", Model: "m"}
		if _, _, err := s.CreateRun(ctx, first); err != nil {
			t.Fatal(err)
		}
		late := coreInput(th.ID, "alice", "late")
		late.AutoTitle = "Late title"

		// Act
		_, _, err := s.CreateRun(ctx, late)

		// Assert
		if !errors.Is(err, store.ErrRunActive) {
			t.Fatalf("got %v, want ErrRunActive", err)
		}
		if got := coreGetThread(t, s, "alice", th.ID); got.Title != "" {
			t.Errorf("a refused run set the title %q", got.Title)
		}
		if msgs, _ := s.ListMessages(ctx, th.ID); len(msgs) != 0 {
			t.Errorf("a refused run stored %d messages", len(msgs))
		}
	})

	t.Run("DuplicateMessageIdIsAConflictWithinTheThread", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := coreNewThread(t, s, "alice", "")
		other := coreNewThread(t, s, "alice", "")
		in := coreInput(th.ID, "alice", "first")
		in.Message.ID = "m-1"
		if r, _, err := s.CreateRun(ctx, in); err != nil {
			t.Fatal(err)
		} else if ok, err := s.CancelQueuedRun(ctx, r.ID); err != nil || !ok {
			t.Fatal(err)
		}
		dup := coreInput(th.ID, "alice", "again")
		dup.Message.ID = "m-1"
		dup.AutoTitle = "Dup title"
		elsewhere := coreInput(other.ID, "alice", "same id, other thread")
		elsewhere.Message.ID = "m-1"

		// Act
		_, _, dupErr := s.CreateRun(ctx, dup)
		_, _, elsewhereErr := s.CreateRun(ctx, elsewhere)

		// Assert
		if !errors.Is(dupErr, store.ErrConflict) {
			t.Fatalf("duplicate id: %v, want ErrConflict", dupErr)
		}
		if elsewhereErr != nil {
			t.Errorf("ids are only unique within a thread: %v", elsewhereErr)
		}
		got := coreGetThread(t, s, "alice", th.ID)
		if got.ActiveRun != nil || got.Title != "" {
			t.Errorf("the conflicting run left traces: %+v", got)
		}
		if msgs, _ := s.ListMessages(ctx, th.ID); len(msgs) != 1 || msgs[0].Content != "first" {
			t.Errorf("messages %+v", msgs)
		}
	})

	t.Run("AutoTitleOnlyNamesUntitledThreads", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		untitled := coreNewThread(t, s, "alice", "")
		titled := coreNewThread(t, s, "alice", "Mine")
		for _, th := range []*store.Thread{untitled, titled} {
			in := coreInput(th.ID, "alice", "q")
			in.AutoTitle = "Auto one"
			r, _, err := s.CreateRun(ctx, in)
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := s.CancelQueuedRun(ctx, r.ID); err != nil || !ok {
				t.Fatal(err)
			}
		}

		// Act
		again := coreInput(untitled.ID, "alice", "q2")
		again.AutoTitle = "Auto two"
		_, _, err := s.CreateRun(ctx, again)

		// Assert
		if err != nil {
			t.Fatal(err)
		}
		if got := coreGetThread(t, s, "alice", untitled.ID); got.Title != "Auto one" {
			t.Errorf("untitled thread title %q, want the first auto title kept", got.Title)
		}
		if got := coreGetThread(t, s, "alice", titled.ID); got.Title != "Mine" {
			t.Errorf("titled thread title %q", got.Title)
		}
	})

	t.Run("RunModesAreRememberedAndInherited", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := coreNewThread(t, s, "alice", "")
		first := coreInput(th.ID, "alice", "a")
		first.Sandbox, first.Mode, first.InteractionMode = store.SandboxOff, store.ModePlan, store.InteractionInteractive
		r1, _, err := s.CreateRun(ctx, first)
		if err != nil {
			t.Fatal(err)
		}
		coreEnd(t, s, r1, store.StatusCompleted)

		// Act
		r2, _, err2 := s.CreateRun(ctx, coreInput(th.ID, "alice", "b")) // everything empty: inherit
		coreEnd(t, s, r2, store.StatusCompleted)
		override := coreInput(th.ID, "alice", "c")
		override.Sandbox, override.Mode, override.InteractionMode = store.SandboxOn, store.ModeBuild, store.InteractionAutomatic
		r3, _, err3 := s.CreateRun(ctx, override)
		coreEnd(t, s, r3, store.StatusCompleted)
		r4, _, err4 := s.CreateRun(ctx, coreInput(th.ID, "alice", "d"))

		// Assert
		for _, err := range []error{err2, err3, err4} {
			if err != nil {
				t.Fatal(err)
			}
		}
		if r1.Sandbox != store.SandboxOff || r1.Mode != store.ModePlan || r1.InteractionMode != store.InteractionInteractive {
			t.Errorf("explicit modes: sandbox %q mode %q interaction %q", r1.Sandbox, r1.Mode, r1.InteractionMode)
		}
		if r2.Sandbox != store.SandboxOff || r2.Mode != store.ModePlan || r2.InteractionMode != store.InteractionInteractive {
			t.Errorf("an empty request inherits the previous run: sandbox %q mode %q interaction %q", r2.Sandbox, r2.Mode, r2.InteractionMode)
		}
		if r3.Sandbox != store.SandboxOn || r3.Mode != store.ModeBuild || r3.InteractionMode != store.InteractionAutomatic {
			t.Errorf("an explicit request wins: sandbox %q mode %q interaction %q", r3.Sandbox, r3.Mode, r3.InteractionMode)
		}
		if r4.Sandbox != store.SandboxOn || r4.Mode != store.ModeBuild || r4.InteractionMode != store.InteractionAutomatic {
			t.Errorf("inheritance follows the latest run: sandbox %q mode %q interaction %q", r4.Sandbox, r4.Mode, r4.InteractionMode)
		}
		if got := coreGetThread(t, s, "alice", th.ID); got.InteractionMode != store.InteractionAutomatic {
			t.Errorf("the thread remembers the last interaction mode: %q", got.InteractionMode)
		}
	})

	t.Run("FirstRunDefaultsToAutomaticInteraction", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := coreNewThread(t, s, "alice", "")

		// Act
		run := coreQueue(t, s, th, "hi")

		// Assert
		if run.InteractionMode != store.InteractionAutomatic {
			t.Errorf("interaction mode %q", run.InteractionMode)
		}
	})

	t.Run("GetRunScopesTheUserVariant", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := coreNewThread(t, s, "alice", "")
		run := coreQueue(t, s, th, "x")

		// Act
		any1, anyErr := s.GetRun(ctx, run.ID)
		mine, mineErr := s.GetUserRun(ctx, "alice", run.ID)
		_, otherErr := s.GetUserRun(ctx, "bob", run.ID)
		_, unknownErr := s.GetRun(ctx, uuid.NewString())
		_, unknownUserErr := s.GetUserRun(ctx, "alice", uuid.NewString())
		_, garbageErr := s.GetRun(ctx, "not-a-uuid")
		_, garbageUserErr := s.GetUserRun(ctx, "alice", "not-a-uuid")
		_, delErr := s.DeleteThread(ctx, "alice", th.ID)
		_, deletedUserErr := s.GetUserRun(ctx, "alice", run.ID)
		stillThere, stillErr := s.GetRun(ctx, run.ID)

		// Assert
		if anyErr != nil || mineErr != nil || any1.ID != run.ID || mine.ID != run.ID {
			t.Fatalf("get %v / %v", anyErr, mineErr)
		}
		for name, err := range map[string]error{"other user": otherErr, "unknown run": unknownErr, "unknown user run": unknownUserErr,
			"garbage id": garbageErr, "garbage user id": garbageUserErr, "deleted thread": deletedUserErr} {
			if !errors.Is(err, store.ErrNotFound) {
				t.Errorf("%s: %v, want ErrNotFound", name, err)
			}
		}
		if delErr != nil || stillErr != nil || stillThere.ID != run.ID {
			t.Errorf("GetRun ignores thread deletion: %v %v", delErr, stillErr)
		}
	})
}

// coreEnd takes a queued run to a terminal state through the port.
func coreEnd(t *testing.T, s store.Store, r *store.Run, status string) {
	t.Helper()
	if r == nil {
		t.Fatal("no run to end")
	}
	ctx := context.Background()
	if _, err := s.ClaimRun(ctx, r.ID, "rt"); err != nil {
		t.Fatalf("ClaimRun: %v", err)
	}
	if err := s.FinishRun(ctx, r.ID, "rt", status, "", ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
}

func coreSuiteRunLifecycle(t *testing.T, f Factory) {
	ctx := context.Background()

	t.Run("ClaimRunStartsAQueuedRunOnce", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := coreNewThread(t, s, "alice", "")
		run := coreQueue(t, s, th, "x")

		// Act
		claimed, err := s.ClaimRun(ctx, run.ID, "rt-1")
		_, again := s.ClaimRun(ctx, run.ID, "rt-2")
		_, unknown := s.ClaimRun(ctx, uuid.NewString(), "rt-1")
		_, garbage := s.ClaimRun(ctx, "not-a-uuid", "rt-1")

		// Assert
		if err != nil {
			t.Fatalf("ClaimRun: %v", err)
		}
		if claimed.Status != store.StatusRunning || claimed.RuntimeID != "rt-1" || claimed.StartedAt == nil || claimed.HeartbeatAt == nil {
			t.Errorf("claimed run %+v", claimed)
		}
		for name, err := range map[string]error{"second claim": again, "unknown run": unknown, "garbage id": garbage} {
			if !errors.Is(err, store.ErrNotClaimable) {
				t.Errorf("%s: %v, want ErrNotClaimable", name, err)
			}
		}
		stored := coreGetRun(t, s, run.ID)
		if stored.Status != store.StatusRunning || stored.RuntimeID != "rt-1" {
			t.Errorf("a refused claim must not steal the run: %+v", stored)
		}
		if th := coreGetThread(t, s, "alice", th.ID); th.ActiveRun == nil || th.ActiveRun.Status != store.StatusRunning {
			t.Errorf("thread active run %+v", th.ActiveRun)
		}
	})

	t.Run("ClaimRunCancelsAQueuedRunOfADeletedThread", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := coreNewThread(t, s, "alice", "")
		run := coreQueue(t, s, th, "x")
		if _, err := s.DeleteThread(ctx, "alice", th.ID); err != nil {
			t.Fatal(err)
		}

		// Act
		_, err := s.ClaimRun(ctx, run.ID, "rt")

		// Assert
		if !errors.Is(err, store.ErrNotClaimable) {
			t.Fatalf("got %v, want ErrNotClaimable", err)
		}
		got := coreGetRun(t, s, run.ID)
		if got.Status != store.StatusCancelled || !got.CancelRequested || got.FinishedAt == nil || got.StartedAt != nil {
			t.Errorf("a run of a deleted thread is cancelled, never executed: %+v", got)
		}
	})

	t.Run("HeartbeatReportsOwnershipAndCancelRequests", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := coreNewThread(t, s, "alice", "")
		queued := coreQueue(t, s, th, "x")
		beforeClaim, _, errQueued := s.Heartbeat(ctx, queued.ID, "rt")
		_ = beforeClaim
		running, err := s.ClaimRun(ctx, queued.ID, "rt")
		if err != nil {
			t.Fatal(err)
		}

		// Act
		owned, cancel, hbErr := s.Heartbeat(ctx, running.ID, "rt")
		foreignOwned, foreignCancel, foreignErr := s.Heartbeat(ctx, running.ID, "other-rt")
		unknownOwned, _, unknownErr := s.Heartbeat(ctx, uuid.NewString(), "rt")
		_, reqErr := s.RequestCancel(ctx, running.ID)
		ownedAfter, cancelAfter, afterErr := s.Heartbeat(ctx, running.ID, "rt")
		finishErr := s.FinishRun(ctx, running.ID, "rt", store.StatusCancelled, "", "")
		finishedOwned, _, finishedErr := s.Heartbeat(ctx, running.ID, "rt")

		// Assert
		if errQueued != nil || hbErr != nil || foreignErr != nil || unknownErr != nil || reqErr != nil || afterErr != nil || finishErr != nil || finishedErr != nil {
			t.Fatalf("errors: %v %v %v %v %v %v %v %v", errQueued, hbErr, foreignErr, unknownErr, reqErr, afterErr, finishErr, finishedErr)
		}
		if beforeClaim {
			t.Errorf("a queued run is not owned by anyone")
		}
		if !owned || cancel {
			t.Errorf("owner heartbeat: owned %v cancel %v", owned, cancel)
		}
		if foreignOwned || foreignCancel || unknownOwned {
			t.Errorf("another runtime / unknown run must not own: %v %v %v", foreignOwned, foreignCancel, unknownOwned)
		}
		if !ownedAfter || !cancelAfter {
			t.Errorf("after RequestCancel the heartbeat reports it: owned %v cancel %v", ownedAfter, cancelAfter)
		}
		if finishedOwned {
			t.Errorf("a finished run is not owned any more")
		}
	})

	t.Run("RequestCancelFlagsQueuedAndRunningRunsWithoutEndingThem", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		a := coreNewThread(t, s, "alice", "")
		b := coreNewThread(t, s, "alice", "")
		queued := coreQueue(t, s, a, "x")
		running := coreStart(t, s, b, "rt")

		// Act
		q, qErr := s.RequestCancel(ctx, queued.ID)
		r, rErr := s.RequestCancel(ctx, running.ID)
		_, unknownErr := s.RequestCancel(ctx, uuid.NewString())

		// Assert
		if qErr != nil || rErr != nil {
			t.Fatalf("RequestCancel: %v %v", qErr, rErr)
		}
		if !q.CancelRequested || q.Status != store.StatusQueued {
			t.Errorf("queued run: %+v", q)
		}
		if !r.CancelRequested || r.Status != store.StatusRunning {
			t.Errorf("running run: %+v", r)
		}
		if !errors.Is(unknownErr, store.ErrNotFound) {
			t.Errorf("unknown run: %v, want ErrNotFound", unknownErr)
		}
		if got := coreGetRun(t, s, running.ID); !got.CancelRequested {
			t.Errorf("the flag is stored: %+v", got)
		}
	})

	t.Run("CancelQueuedRunOnlyCancelsQueuedRuns", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		a := coreNewThread(t, s, "alice", "")
		b := coreNewThread(t, s, "alice", "")
		queued := coreQueue(t, s, a, "x")
		running := coreStart(t, s, b, "rt")

		// Act
		cancelled, err := s.CancelQueuedRun(ctx, queued.ID)
		twice, twiceErr := s.CancelQueuedRun(ctx, queued.ID)
		notQueued, runningErr := s.CancelQueuedRun(ctx, running.ID)
		unknown, unknownErr := s.CancelQueuedRun(ctx, uuid.NewString())

		// Assert
		if err != nil || twiceErr != nil || runningErr != nil || unknownErr != nil {
			t.Fatalf("errors %v %v %v %v", err, twiceErr, runningErr, unknownErr)
		}
		if !cancelled || twice || notQueued || unknown {
			t.Errorf("cancelled %v again %v running %v unknown %v", cancelled, twice, notQueued, unknown)
		}
		got := coreGetRun(t, s, queued.ID)
		if got.Status != store.StatusCancelled || !got.CancelRequested || got.FinishedAt == nil {
			t.Errorf("cancelled run %+v", got)
		}
		if coreGetRun(t, s, running.ID).Status != store.StatusRunning {
			t.Errorf("a running run is left alone")
		}
		if th := coreGetThread(t, s, "alice", a.ID); th.ActiveRun != nil {
			t.Errorf("a cancelled run is no longer the active run: %+v", th.ActiveRun)
		}
		if _, _, err := s.CreateRun(ctx, coreInput(a.ID, "alice", "next")); err != nil {
			t.Errorf("the thread accepts a new run after the cancel: %v", err)
		}
	})

	t.Run("FinishRunStoresTheTerminalStatusAndErrorCode", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		cases := []struct {
			name, status, code, msg string
			wantCode                string
		}{
			{"completed", store.StatusCompleted, "", "", ""},
			{"completed ignores a code", store.StatusCompleted, "boom", "", ""},
			{"cancelled ignores a code", store.StatusCancelled, "boom", "", ""},
			{"failed without code is internal", store.StatusFailed, "", "it broke", "internal"},
			{"failed keeps its code", store.StatusFailed, "rate_limited", "slow down", "rate_limited"},
			{"interrupted without code", store.StatusInterrupted, "", "gone", "interrupted"},
			{"interrupted keeps its code", store.StatusInterrupted, "shutdown", "bye", "shutdown"},
		}
		for _, c := range cases {
			th := coreNewThread(t, s, "alice", "")
			run := coreStart(t, s, th, "rt")

			// Act
			err := s.FinishRun(ctx, run.ID, "rt", c.status, c.code, c.msg)

			// Assert
			if err != nil {
				t.Errorf("%s: FinishRun: %v", c.name, err)
				continue
			}
			got := coreGetRun(t, s, run.ID)
			if got.Status != c.status || got.ErrorCode != c.wantCode || got.FinishedAt == nil {
				t.Errorf("%s: stored %+v", c.name, got)
			}
			if (c.status == store.StatusFailed || c.status == store.StatusInterrupted) && got.Error != c.msg {
				t.Errorf("%s: error message %q, want %q", c.name, got.Error, c.msg)
			}
			if th := coreGetThread(t, s, "alice", th.ID); th.ActiveRun != nil {
				t.Errorf("%s: a finished run is not active: %+v", c.name, th.ActiveRun)
			}
		}
	})

	t.Run("FinishRunRequiresTheOwningRuntimeOfARunningRun", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		a := coreNewThread(t, s, "alice", "")
		b := coreNewThread(t, s, "alice", "")
		running := coreStart(t, s, a, "rt")
		queued := coreQueue(t, s, b, "x")

		// Act
		foreign := s.FinishRun(ctx, running.ID, "other", store.StatusCompleted, "", "")
		notStarted := s.FinishRun(ctx, queued.ID, "rt", store.StatusCompleted, "", "")
		unknown := s.FinishRun(ctx, uuid.NewString(), "rt", store.StatusCompleted, "", "")
		first := s.FinishRun(ctx, running.ID, "rt", store.StatusCompleted, "", "")
		second := s.FinishRun(ctx, running.ID, "rt", store.StatusFailed, "x", "late")

		// Assert
		for name, err := range map[string]error{"foreign runtime": foreign, "queued run": notStarted, "unknown run": unknown, "second finish": second} {
			if !errors.Is(err, store.ErrNotOwned) {
				t.Errorf("%s: %v, want ErrNotOwned", name, err)
			}
		}
		if first != nil {
			t.Errorf("the owner finishes: %v", first)
		}
		if got := coreGetRun(t, s, running.ID); got.Status != store.StatusCompleted {
			t.Errorf("a refused finish must not overwrite the outcome: %+v", got)
		}
		if got := coreGetRun(t, s, queued.ID); got.Status != store.StatusQueued {
			t.Errorf("a refused finish must not end a queued run: %+v", got)
		}
	})

	t.Run("SetRunOutcomeOverwritesTheTerminalStatus", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := coreNewThread(t, s, "alice", "")
		run := coreStart(t, s, th, "rt")
		if err := s.FinishRun(ctx, run.ID, "rt", store.StatusFailed, "boom", "it broke"); err != nil {
			t.Fatal(err)
		}
		finished := coreGetRun(t, s, run.ID).FinishedAt

		// Act
		err := s.SetRunOutcome(ctx, run.ID, store.StatusCompleted, "", "")
		repaired := coreGetRun(t, s, run.ID)
		err2 := s.SetRunOutcome(ctx, run.ID, store.StatusInterrupted, "", "lost")
		interrupted := coreGetRun(t, s, run.ID)

		// Assert
		if err != nil || err2 != nil {
			t.Fatalf("SetRunOutcome: %v %v", err, err2)
		}
		if repaired.Status != store.StatusCompleted || repaired.ErrorCode != "" || repaired.Error != "" {
			t.Errorf("overwritten outcome %+v", repaired)
		}
		if repaired.FinishedAt == nil || finished == nil || !repaired.FinishedAt.Equal(*finished) {
			t.Errorf("the finish time is kept: %v then %v", finished, repaired.FinishedAt)
		}
		if interrupted.Status != store.StatusInterrupted || interrupted.ErrorCode != "interrupted" || interrupted.Error != "lost" {
			t.Errorf("error code defaults like FinishRun: %+v", interrupted)
		}
	})

	t.Run("SetRunOutcomeStampsTheFinishTimeOfAnUnfinishedRun", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := coreNewThread(t, s, "alice", "")
		run := coreStart(t, s, th, "rt")

		// Act
		err := s.SetRunOutcome(ctx, run.ID, store.StatusFailed, "", "log proves it")

		// Assert
		got := coreGetRun(t, s, run.ID)
		if err != nil || got.Status != store.StatusFailed || got.ErrorCode != "internal" || got.FinishedAt == nil {
			t.Fatalf("outcome %+v (%v)", got, err)
		}
	})
}

func coreSuiteStaleRuns(t *testing.T, f Factory) {
	ctx := context.Background()
	const stale = 5 * time.Minute

	t.Run("ListQueuedRunsOlderThanFindsOnlyOldQueuedRuns", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		oldA := coreQueue(t, s, coreNewThread(t, s, "alice", ""), "a")
		oldB := coreQueue(t, s, coreNewThread(t, s, "alice", ""), "b")
		started := coreStart(t, s, coreNewThread(t, s, "alice", ""), "rt")
		b.Advance(t, 10*time.Minute)
		fresh := coreQueue(t, s, coreNewThread(t, s, "alice", ""), "fresh")

		// Act
		old, err := s.ListQueuedRunsOlderThan(ctx, stale)
		none, noneErr := s.ListQueuedRunsOlderThan(ctx, time.Hour)

		// Assert
		if err != nil || noneErr != nil {
			t.Fatalf("errors %v %v", err, noneErr)
		}
		if got, want := coreSorted(old), coreSorted([]string{oldA.ID, oldB.ID}); !reflect.DeepEqual(got, want) {
			t.Errorf("old queued runs %v, want %v (not the running %s nor the fresh %s)", got, want, started.ID, fresh.ID)
		}
		if len(none) != 0 {
			t.Errorf("nothing is an hour old: %v", none)
		}
	})

	t.Run("InterruptStaleRunsInterruptsRunsWhoseHeartbeatStopped", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		thStale := coreNewThread(t, s, "alice", "")
		stalled := coreStart(t, s, thStale, "rt-dead")
		thAlive := coreNewThread(t, s, "alice", "")
		beating := coreStart(t, s, thAlive, "rt-alive")
		queued := coreQueue(t, s, coreNewThread(t, s, "alice", ""), "waiting")
		b.Advance(t, 10*time.Minute)
		if owned, _, err := s.Heartbeat(ctx, beating.ID, "rt-alive"); err != nil || !owned {
			t.Fatalf("heartbeat: %v %v", owned, err)
		}

		// Act
		got, err := s.InterruptStaleRuns(ctx, stale)
		again, againErr := s.InterruptStaleRuns(ctx, stale)

		// Assert
		if err != nil || againErr != nil {
			t.Fatalf("errors %v %v", err, againErr)
		}
		if len(got) != 1 || got[0].ID != stalled.ID || got[0].Status != store.StatusInterrupted ||
			got[0].ErrorCode != "interrupted" || got[0].Error == "" || got[0].FinishedAt == nil || got[0].ThreadID != thStale.ID {
			t.Fatalf("interrupted %+v", got)
		}
		if len(again) != 0 {
			t.Errorf("a run is interrupted once: %v", coreRunIDs(again))
		}
		if r := coreGetRun(t, s, beating.ID); r.Status != store.StatusRunning {
			t.Errorf("a run that heartbeated is left running: %+v", r)
		}
		if r := coreGetRun(t, s, queued.ID); r.Status != store.StatusQueued {
			t.Errorf("a queued run is not a stale running run: %+v", r)
		}
		stored := coreGetRun(t, s, stalled.ID)
		if stored.Status != store.StatusInterrupted || stored.ErrorCode != "interrupted" {
			t.Errorf("stored %+v", stored)
		}
		if th := coreGetThread(t, s, "alice", thStale.ID); th.ActiveRun != nil {
			t.Errorf("the thread is free again: %+v", th.ActiveRun)
		}
	})

	t.Run("AnInterruptedRunLosesItsOwner", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		run := coreStart(t, s, coreNewThread(t, s, "alice", ""), "rt")
		b.Advance(t, 10*time.Minute)
		if got, err := s.InterruptStaleRuns(ctx, stale); err != nil || len(got) != 1 {
			t.Fatalf("interrupt: %v %v", got, err)
		}

		// Act
		owned, _, hbErr := s.Heartbeat(ctx, run.ID, "rt")
		finishErr := s.FinishRun(ctx, run.ID, "rt", store.StatusCompleted, "", "")

		// Assert
		if hbErr != nil || owned {
			t.Errorf("heartbeat of an interrupted run: owned %v err %v", owned, hbErr)
		}
		if !errors.Is(finishErr, store.ErrNotOwned) {
			t.Errorf("finish: %v, want ErrNotOwned", finishErr)
		}
	})

	t.Run("AliveChecksVouchForStaleRuns", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		vouched := coreStart(t, s, coreNewThread(t, s, "alice", ""), "rt-redis")
		doomed := coreStart(t, s, coreNewThread(t, s, "alice", ""), "rt-gone")
		b.Advance(t, 10*time.Minute)
		var asked []string
		check := func(r *store.Run) bool {
			asked = append(asked, r.ID)
			return r.RuntimeID == "rt-redis"
		}
		never := func(*store.Run) bool { return false }

		// Act
		got, err := s.InterruptStaleRuns(ctx, stale, nil, never, check)

		// Assert
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != doomed.ID {
			t.Fatalf("interrupted %v, want only %s", coreRunIDs(got), doomed.ID)
		}
		if r := coreGetRun(t, s, vouched.ID); r.Status != store.StatusRunning {
			t.Errorf("a vouched-for run stays running: %+v", r)
		}
		if !reflect.DeepEqual(coreSorted(asked), coreSorted([]string{vouched.ID, doomed.ID})) {
			t.Errorf("the check sees each stale run once: %v", asked)
		}
	})
}

func coreRunIDs(runs []*store.Run) []string {
	ids := make([]string, 0, len(runs))
	for _, r := range runs {
		ids = append(ids, r.ID)
	}
	return ids
}
