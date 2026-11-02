package conformance

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/7ar-chat/harness/core/store"
)

// apprNewRun is a chat run with a user message (an interactive one when interactive).
func apprNewRun(threadID, content string) store.NewRun {
	return store.NewRun{RunID: uuid.NewString(), ThreadID: threadID, UserLogin: "alice", Model: "m",
		Message: store.Message{Role: store.RoleUser, Content: content}, AutoTitle: content}
}

// apprResume is a run without message that decides the approval id.
func apprResume(threadID, id, decision, scope string, resp store.ApprovalResponse) store.NewRun {
	return store.NewRun{RunID: uuid.NewString(), ThreadID: threadID, UserLogin: "alice", Model: "m",
		Resume: &store.Resume{InterruptID: id, Decision: decision, Scope: scope, Response: resp}}
}

// apprFinish runs the queued run to the given terminal status.
func apprFinish(t *testing.T, s Store, runID, status string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.ClaimRun(ctx, runID, "rt"); err != nil {
		t.Fatal(err)
	}
	errCode := ""
	if status == store.StatusFailed {
		errCode = "boom"
	}
	if err := s.FinishRun(ctx, runID, "rt", status, errCode, ""); err != nil {
		t.Fatal(err)
	}
}

// apprPauseCall makes run (running or queued state untouched) call toolName as call callID and records the pending approval.
func apprPauseCall(t *testing.T, s Store, run *store.Run, callID, toolName string) *store.Approval {
	t.Helper()
	ctx := context.Background()
	err := s.AppendMessages(ctx, &store.Message{ThreadID: run.ThreadID, RunID: run.ID, Role: store.RoleAssistant,
		ToolCalls: []store.ToolCall{{ID: callID, Type: "function", Function: store.ToolCallFunction{Name: toolName, Arguments: `{"path":"a"}`}}}})
	if err != nil {
		t.Fatal(err)
	}
	a := &store.Approval{ThreadID: run.ThreadID, RunID: run.ID, ToolCallID: callID, ToolName: toolName, ToolInputPreview: `{"path":"a"}`}
	if err := s.CreateApproval(ctx, a); err != nil {
		t.Fatal(err)
	}
	return a
}

// apprPaused creates an interactive run that is still queued and waits for the approval of call "c1" (write_file).
func apprPaused(t *testing.T, s Store, threadID string) (*store.Run, *store.Approval) {
	t.Helper()
	in := apprNewRun(threadID, "write it")
	in.InteractionMode = store.InteractionInteractive
	run, _, err := s.CreateRun(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return run, apprPauseCall(t, s, run, "c1", "write_file")
}

// apprPausedDone is apprPaused with the run completed: the thread waits for the decision.
func apprPausedDone(t *testing.T, s Store, threadID string) (*store.Run, *store.Approval) {
	t.Helper()
	run, a := apprPaused(t, s, threadID)
	apprFinish(t, s, run.ID, store.StatusCompleted)
	return run, a
}

func apprThread(t *testing.T, s Store, login, title string) *store.Thread {
	t.Helper()
	th, err := s.CreateThread(context.Background(), login, title)
	if err != nil {
		t.Fatal(err)
	}
	return th
}

func apprToolResults(t *testing.T, s Store, threadID string) []*store.Message {
	t.Helper()
	msgs, err := s.ListMessages(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	var out []*store.Message
	for _, m := range msgs {
		if m.Role == store.RoleTool {
			out = append(out, m)
		}
	}
	return out
}

func apprGet(t *testing.T, s Store, id string) *store.Approval {
	t.Helper()
	a, err := s.GetApproval(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func apprPending(t *testing.T, s Store, threadID string) *store.Approval {
	t.Helper()
	th, err := s.GetThread(context.Background(), "alice", threadID)
	if err != nil {
		t.Fatal(err)
	}
	return th.PendingApproval
}

// Approvals runs the store.ApprovalStore suite.
func Approvals(t *testing.T, f Factory) {
	t.Helper()
	t.Run("an approval is recorded pending with its defaults and read back by id", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		ctx := context.Background()
		th := apprThread(t, s, "alice", "t")
		run, _, _ := s.CreateRun(ctx, apprNewRun(th.ID, "go"))
		fixed := uuid.NewString()
		a := &store.Approval{ThreadID: th.ID, RunID: run.ID, ToolCallID: "c1", ToolName: "write_file", ToolInputPreview: `{"path":"a"}`}
		other := &store.Approval{ID: fixed, Kind: store.ApprovalKindRiskyTool, ThreadID: th.ID, RunID: run.ID, ToolCallID: "c2", ToolName: "x"}

		// Act
		err := s.CreateApproval(ctx, a)
		got, getErr := s.GetApproval(ctx, a.ID)
		secondOfRun := s.CreateApproval(ctx, other)
		unknownThread := s.CreateApproval(ctx, &store.Approval{ThreadID: uuid.NewString(), RunID: run.ID, ToolCallID: "c3", ToolName: "x"})
		unknownRun := s.CreateApproval(ctx, &store.Approval{ThreadID: th.ID, RunID: uuid.NewString(), ToolCallID: "c3", ToolName: "x"})
		_, unknownID := s.GetApproval(ctx, uuid.NewString())
		_, badID := s.GetApproval(ctx, "not-a-uuid")

		// Assert
		if err != nil || getErr != nil {
			t.Fatalf("create %v get %v", err, getErr)
		}
		if a.ID == "" || a.Kind != store.ApprovalKindRiskyTool || a.Status != store.ApprovalPending || a.CreatedAt.IsZero() {
			t.Fatalf("defaults not set on the created approval: %+v", a)
		}
		if got.ID != a.ID || got.ThreadID != th.ID || got.RunID != run.ID || got.ToolCallID != "c1" || got.ToolName != "write_file" ||
			got.ToolInputPreview != `{"path":"a"}` || got.Kind != store.ApprovalKindRiskyTool || got.Status != store.ApprovalPending ||
			got.Decision != "" || got.ResumeRunID != "" || got.ResolvedAt != nil || got.CreatedAt.IsZero() {
			t.Fatalf("read back %+v", got)
		}
		if secondOfRun == nil {
			t.Fatal("a second pending approval of the same run was accepted")
		}
		if unknownThread == nil || unknownRun == nil {
			t.Fatalf("approval of an unknown thread (%v) or run (%v) was accepted", unknownThread, unknownRun)
		}
		if !errors.Is(unknownID, store.ErrNotFound) || !errors.Is(badID, store.ErrNotFound) {
			t.Fatalf("unknown id %v malformed id %v", unknownID, badID)
		}
	})

	t.Run("a caller-chosen approval id is kept", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		ctx := context.Background()
		th := apprThread(t, s, "alice", "t")
		run, _, _ := s.CreateRun(ctx, apprNewRun(th.ID, "go"))
		id := uuid.NewString()

		// Act
		err := s.CreateApproval(ctx, &store.Approval{ID: id, ThreadID: th.ID, RunID: run.ID, ToolCallID: "c1", ToolName: "x"})

		// Assert
		if err != nil || apprGet(t, s, id).RunID != run.ID {
			t.Fatalf("create %v", err)
		}
	})

	t.Run("the pending approval shows on the thread only once its run completed", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := apprThread(t, s, "alice", "t")
		run, a := apprPaused(t, s, th.ID)
		whileRunning := apprPending(t, s, th.ID)

		// Act
		apprFinish(t, s, run.ID, store.StatusCompleted)
		pending := apprPending(t, s, th.ID)
		got, _ := s.GetThread(context.Background(), "alice", th.ID)

		// Assert
		if whileRunning != nil {
			t.Fatalf("pending before the run completed: %+v", whileRunning)
		}
		if run.InteractionMode != store.InteractionInteractive || got.InteractionMode != store.InteractionInteractive {
			t.Fatalf("run mode %q thread mode %q", run.InteractionMode, got.InteractionMode)
		}
		if pending == nil || pending.ID != a.ID || pending.RunID != run.ID || pending.ToolCallID != "c1" || pending.ToolName != "write_file" ||
			pending.ToolInputPreview != `{"path":"a"}` || pending.Status != store.ApprovalPending || pending.Kind != store.ApprovalKindRiskyTool || pending.CreatedAt.IsZero() {
			t.Fatalf("pending %+v", pending)
		}
	})

	t.Run("an approval of a run that failed never becomes the thread's pending one", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := apprThread(t, s, "alice", "t")
		run, a := apprPaused(t, s, th.ID)

		// Act
		apprFinish(t, s, run.ID, store.StatusFailed)

		// Assert
		if p := apprPending(t, s, th.ID); p != nil {
			t.Fatalf("pending %+v", p)
		}
		if apprGet(t, s, a.ID).Status != store.ApprovalPending {
			t.Fatal("the approval record itself changed")
		}
	})

	t.Run("a resume decides the approval", func(t *testing.T) {
		cases := []struct {
			name     string
			decision string
			resp     store.ApprovalResponse
			want     string // the stored decision
		}{
			{"approve", store.DecisionApprove, store.ApprovalResponse{}, store.DecisionApprove},
			{"deny", store.DecisionDeny, store.ApprovalResponse{}, store.DecisionDeny},
			{"an empty decision is a deny", "", store.ApprovalResponse{}, store.DecisionDeny},
			{"respond", store.DecisionRespond, store.ApprovalResponse{SelectedChoice: "B", ResponseText: "use b"}, store.DecisionRespond},
			{"respond without details", store.DecisionRespond, store.ApprovalResponse{}, store.DecisionRespond},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				// Arrange
				s := f(t).Store
				ctx := context.Background()
				th := apprThread(t, s, "alice", "t")
				_, a := apprPausedDone(t, s, th.ID)

				// Act
				next, _, err := s.CreateRun(ctx, apprResume(th.ID, a.ID, tc.decision, "", tc.resp))

				// Assert
				if err != nil {
					t.Fatal(err)
				}
				got, rerr := s.ResumedApproval(ctx, next.ID)
				if rerr != nil || got == nil || got.ID != a.ID || got.Status != store.ApprovalResolved || got.Decision != tc.want ||
					got.ResumeRunID != next.ID || got.ResolvedAt == nil || got.Scope != store.ApprovalScopeOnce || got.Response != tc.resp {
					t.Fatalf("approval %+v err %v", got, rerr)
				}
				if byID := apprGet(t, s, a.ID); byID.Status != store.ApprovalResolved || byID.Decision != tc.want {
					t.Fatalf("by id %+v", byID)
				}
				want := store.ApprovalResult(tc.want, tc.resp)
				results := apprToolResults(t, s, th.ID)
				if want == "" && len(results) != 0 {
					t.Fatalf("an approve stored a tool result: %+v", results)
				}
				if want != "" && (len(results) != 1 || results[0].Content != want || results[0].RunID != next.ID || results[0].ToolCallID != "c1") {
					t.Fatalf("results %+v, want %q", results, want)
				}
				if next.InteractionMode != store.InteractionInteractive {
					t.Fatalf("resume run mode %q", next.InteractionMode)
				}
				if p := apprPending(t, s, th.ID); p != nil {
					t.Fatalf("still pending: %+v", p)
				}
			})
		}
	})

	t.Run("a run that does not decide the pending approval is refused or cancels it", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		ctx := context.Background()
		th := apprThread(t, s, "alice", "t")
		_, a := apprPausedDone(t, s, th.ID)
		compact := apprNewRun(th.ID, "")
		compact.Message, compact.Kind = store.Message{}, store.KindCompact
		wrong := apprResume(th.ID, "other", store.DecisionApprove, "", store.ApprovalResponse{})

		// Act
		_, _, compactErr := s.CreateRun(ctx, compact)
		_, _, wrongErr := s.CreateRun(ctx, wrong)
		stillPending := apprPending(t, s, th.ID)
		next, _, msgErr := s.CreateRun(ctx, apprNewRun(th.ID, "something else"))

		// Assert
		if !errors.Is(compactErr, store.ErrApprovalPending) || !errors.Is(wrongErr, store.ErrNoPendingQuestion) || msgErr != nil {
			t.Fatalf("errors %v %v %v", compactErr, wrongErr, msgErr)
		}
		if stillPending == nil || stillPending.ID != a.ID {
			t.Fatalf("a refused run changed the approval: %+v", stillPending)
		}
		got := apprGet(t, s, a.ID)
		results := apprToolResults(t, s, th.ID)
		if got.Status != store.ApprovalCancelled || got.ResumeRunID != next.ID || got.ResolvedAt == nil || got.Decision != "" {
			t.Fatalf("approval %+v", got)
		}
		if len(results) != 1 || results[0].Content != store.ImplicitApprovalCancelResult || results[0].ToolCallID != "c1" || results[0].RunID != next.ID {
			t.Fatalf("results %+v", results)
		}
	})

	t.Run("a deny after an edit hides the result with the call it answers", func(t *testing.T) {
		cases := []struct {
			name       string
			resume     bool
			wantResult string
		}{
			{"message", false, store.ImplicitApprovalCancelResult},
			{"deny", true, store.ApprovalDeniedResult},
		}
		for _, tc := range cases {
			t.Run(tc.name+" with the call hidden", func(t *testing.T) {
				// Arrange
				s := f(t).Store
				ctx := context.Background()
				th := apprThread(t, s, "alice", "t")
				first := apprNewRun(th.ID, "first")
				first.Message.ID = "m1"
				first.InteractionMode = store.InteractionInteractive
				run, _, _ := s.CreateRun(ctx, first)
				a := apprPauseCall(t, s, run, "c1", "write_file")
				apprFinish(t, s, run.ID, store.StatusCompleted)
				in := apprNewRun(th.ID, "edited")
				if tc.resume {
					in = apprResume(th.ID, a.ID, store.DecisionDeny, "", store.ApprovalResponse{})
					in.Message = store.Message{Role: store.RoleUser, Content: "edited"}
				}
				in.ReplaceFromMessageID = "m1"

				// Act
				next, _, err := s.CreateRun(ctx, in)

				// Assert
				if err != nil {
					t.Fatal(err)
				}
				if got := apprGet(t, s, a.ID); got.ResumeRunID != next.ID || got.Status == store.ApprovalPending {
					t.Fatalf("approval %+v", got)
				}
				if results := apprToolResults(t, s, th.ID); len(results) != 0 {
					t.Fatalf("the result of a hidden call is visible: %+v", results)
				}
			})
			t.Run(tc.name+" with the call kept", func(t *testing.T) {
				// Arrange
				s := f(t).Store
				ctx := context.Background()
				th := apprThread(t, s, "alice", "t")
				run, _, _ := s.CreateRun(ctx, apprNewRun(th.ID, "first"))
				a := apprPauseCall(t, s, run, "c1", "write_file")
				// A user message placed after the call: editing from it keeps the call visible.
				if err := s.AppendMessages(ctx, &store.Message{ID: "m2", ThreadID: th.ID, RunID: run.ID, Role: store.RoleUser, Content: "later"}); err != nil {
					t.Fatal(err)
				}
				apprFinish(t, s, run.ID, store.StatusCompleted)
				in := apprNewRun(th.ID, "edited")
				if tc.resume {
					in = apprResume(th.ID, a.ID, store.DecisionDeny, "", store.ApprovalResponse{})
					in.Message = store.Message{Role: store.RoleUser, Content: "edited"}
				}
				in.ReplaceFromMessageID = "m2"

				// Act
				next, _, err := s.CreateRun(ctx, in)

				// Assert
				if err != nil {
					t.Fatal(err)
				}
				results := apprToolResults(t, s, th.ID)
				if len(results) != 1 || results[0].Content != tc.wantResult || results[0].RunID != next.ID || results[0].ToolCallID != "c1" {
					t.Fatalf("results %+v, want %q", results, tc.wantResult)
				}
			})
		}
	})

	t.Run("cancelling the pending approval of a completed run stores the result once", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		ctx := context.Background()
		th := apprThread(t, s, "alice", "t")
		run, a := apprPausedDone(t, s, th.ID)

		// Act
		cancelled, err := s.CancelPendingApproval(ctx, run.ID, store.ApprovalCancelledResult)
		_, again := s.CancelPendingApproval(ctx, run.ID, store.ApprovalCancelledResult)

		// Assert
		if err != nil || cancelled.ID != a.ID || cancelled.Status != store.ApprovalCancelled || cancelled.ResolvedAt == nil {
			t.Fatalf("cancel %+v %v", cancelled, err)
		}
		if !errors.Is(again, store.ErrApprovalNotPending) {
			t.Fatalf("second cancel: %v", again)
		}
		if results := apprToolResults(t, s, th.ID); len(results) != 1 || results[0].ToolCallID != "c1" || results[0].Content != store.ApprovalCancelledResult || results[0].RunID != run.ID {
			t.Fatalf("results %+v", results)
		}
		if got := apprGet(t, s, a.ID); got.Status != store.ApprovalCancelled || got.ResolvedAt == nil || apprPending(t, s, th.ID) != nil {
			t.Fatalf("approval %+v", got)
		}
	})

	t.Run("there is no pending approval to cancel for an unfinished, other or unknown run", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		ctx := context.Background()
		th := apprThread(t, s, "alice", "t")
		unfinished, a := apprPaused(t, s, th.ID)
		other := apprThread(t, s, "alice", "u")
		done, _ := apprPausedDone(t, s, other.ID)
		quiet, _, _ := s.CreateRun(ctx, apprNewRun(apprThread(t, s, "alice", "v").ID, "no approval"))
		apprFinish(t, s, quiet.ID, store.StatusCompleted)

		// Act
		_, unfinishedErr := s.CancelPendingApproval(ctx, unfinished.ID, "r")
		_, unknownErr := s.CancelPendingApproval(ctx, uuid.NewString(), "r")
		_, malformedErr := s.CancelPendingApproval(ctx, "not-a-uuid", "r")
		_, noApprovalErr := s.CancelPendingApproval(ctx, quiet.ID, "r")

		// Assert
		for name, err := range map[string]error{"unfinished": unfinishedErr, "unknown": unknownErr, "malformed": malformedErr, "none": noApprovalErr} {
			if !errors.Is(err, store.ErrApprovalNotPending) {
				t.Errorf("%s run: %v", name, err)
			}
		}
		if got := apprGet(t, s, a.ID); got.Status != store.ApprovalPending || len(apprToolResults(t, s, th.ID)) != 0 {
			t.Fatalf("unfinished run's approval changed: %+v", got)
		}
		if apprPending(t, s, other.ID) == nil || done.ID == "" {
			t.Fatal("another thread's approval changed")
		}
	})

	t.Run("cancelling a run's approvals drops those of a run that did not complete without a result", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		ctx := context.Background()
		th := apprThread(t, s, "alice", "t")
		failed, a := apprPaused(t, s, th.ID)
		apprFinish(t, s, failed.ID, store.StatusFailed)
		other := apprThread(t, s, "alice", "u")
		_, b := apprPausedDone(t, s, other.ID)

		// Act
		err := s.CancelRunApprovals(ctx, failed.ID)
		unknown := s.CancelRunApprovals(ctx, uuid.NewString())
		malformed := s.CancelRunApprovals(ctx, "not-a-uuid")

		// Assert
		if err != nil || unknown != nil || malformed != nil {
			t.Fatalf("errors %v %v %v", err, unknown, malformed)
		}
		if got := apprGet(t, s, a.ID); got.Status != store.ApprovalCancelled || got.ResolvedAt == nil || len(apprToolResults(t, s, th.ID)) != 0 {
			t.Fatalf("dropped %+v", got)
		}
		if got := apprGet(t, s, b.ID); got.Status != store.ApprovalPending {
			t.Fatalf("another run's approval changed: %+v", got)
		}
	})

	t.Run("a resumed run gets its approval; an unrelated or unknown run gets none", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		ctx := context.Background()
		th := apprThread(t, s, "alice", "t")
		_, a := apprPausedDone(t, s, th.ID)
		next, _, _ := s.CreateRun(ctx, apprResume(th.ID, a.ID, store.DecisionApprove, "", store.ApprovalResponse{}))

		// Act
		got, err := s.ResumedApproval(ctx, next.ID)
		none, noneErr := s.ResumedApproval(ctx, uuid.NewString())
		bad, badErr := s.ResumedApproval(ctx, "not-a-uuid")

		// Assert
		if err != nil || got == nil || got.ID != a.ID || noneErr != nil || none != nil || badErr != nil || bad != nil {
			t.Fatalf("got %+v %v none %+v %v bad %+v %v", got, err, none, noneErr, bad, badErr)
		}
	})

	t.Run("the next run announces an approval cancelled with its waiting run, once", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		ctx := context.Background()
		th := apprThread(t, s, "alice", "t")
		run, a := apprPausedDone(t, s, th.ID)
		if _, err := s.CancelPendingApproval(ctx, run.ID, store.ApprovalCancelledResult); err != nil {
			t.Fatal(err)
		}

		// Act
		next, _, err := s.CreateRun(ctx, apprNewRun(th.ID, "never mind"))
		if err != nil {
			t.Fatal(err)
		}
		apprFinish(t, s, next.ID, store.StatusCompleted)
		later, _, laterErr := s.CreateRun(ctx, apprNewRun(th.ID, "thanks"))

		// Assert
		got, _ := s.ResumedApproval(ctx, next.ID)
		none, _ := s.ResumedApproval(ctx, later.ID)
		if laterErr != nil || got == nil || got.ID != a.ID || got.Status != store.ApprovalCancelled || none != nil {
			t.Fatalf("next %+v later %+v %v", got, none, laterErr)
		}
	})

	t.Run("a compaction run does not take over the announcement of a cancelled approval", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		ctx := context.Background()
		th := apprThread(t, s, "alice", "t")
		run, a := apprPausedDone(t, s, th.ID)
		if _, err := s.CancelPendingApproval(ctx, run.ID, store.ApprovalCancelledResult); err != nil {
			t.Fatal(err)
		}
		compact := apprNewRun(th.ID, "")
		compact.Message, compact.Kind = store.Message{}, store.KindCompact

		// Act
		c, _, err := s.CreateRun(ctx, compact)
		if err != nil {
			t.Fatal(err)
		}
		apprFinish(t, s, c.ID, store.StatusCompleted)
		next, _, nextErr := s.CreateRun(ctx, apprNewRun(th.ID, "go on"))

		// Assert
		byCompact, _ := s.ResumedApproval(ctx, c.ID)
		byNext, _ := s.ResumedApproval(ctx, next.ID)
		if nextErr != nil || byCompact != nil || byNext == nil || byNext.ID != a.ID {
			t.Fatalf("compact %+v next %+v %v", byCompact, byNext, nextErr)
		}
	})

	t.Run("a resolved approval reopens only when its resume run did not run the call", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		ctx := context.Background()
		th := apprThread(t, s, "alice", "t")
		_, a := apprPausedDone(t, s, th.ID)
		resume, _, err := s.CreateRun(ctx, apprResume(th.ID, a.ID, store.DecisionApprove, "", store.ApprovalResponse{}))
		if err != nil {
			t.Fatal(err)
		}

		// Act
		_, whileQueued := s.ReopenApproval(ctx, a.ID)
		if _, err := s.ClaimRun(ctx, resume.ID, "rt"); err != nil {
			t.Fatal(err)
		}
		_, whileRunning := s.ReopenApproval(ctx, a.ID)
		if err := s.FinishRun(ctx, resume.ID, "rt", store.StatusFailed, "boom", ""); err != nil {
			t.Fatal(err)
		}
		reopened, reopenErr := s.ReopenApproval(ctx, a.ID)
		_, again := s.ReopenApproval(ctx, a.ID)

		// Assert
		if !errors.Is(whileQueued, store.ErrApprovalNotPending) || !errors.Is(whileRunning, store.ErrApprovalNotPending) {
			t.Fatalf("reopened while the resume run was active: queued %v running %v", whileQueued, whileRunning)
		}
		if reopenErr != nil || reopened.ID != a.ID || reopened.Status != store.ApprovalPending || reopened.Decision != "" ||
			reopened.ResumeRunID != "" || reopened.ResolvedAt != nil || reopened.Response != (store.ApprovalResponse{}) {
			t.Fatalf("reopened %+v %v", reopened, reopenErr)
		}
		if !errors.Is(again, store.ErrApprovalNotPending) {
			t.Fatalf("reopening a pending approval: %v", again)
		}
		if p := apprPending(t, s, th.ID); p == nil || p.ID != a.ID {
			t.Fatalf("thread pending %+v", p)
		}
		// The user may decide again: a deny stores its result, after which the approval cannot reopen.
		denied, _, err := s.CreateRun(ctx, apprResume(th.ID, a.ID, store.DecisionDeny, "", store.ApprovalResponse{}))
		if err != nil {
			t.Fatal(err)
		}
		apprFinish(t, s, denied.ID, store.StatusCompleted)
		if _, err := s.ReopenApproval(ctx, a.ID); !errors.Is(err, store.ErrApprovalNotPending) {
			t.Fatalf("reopened although the deny result is stored: %v", err)
		}
	})

	t.Run("a resume run that completed without a result for the call lets the approval reopen", func(t *testing.T) {
		for _, status := range []string{store.StatusCompleted, store.StatusCancelled, store.StatusInterrupted} {
			t.Run(status, func(t *testing.T) {
				// Arrange
				s := f(t).Store
				ctx := context.Background()
				th := apprThread(t, s, "alice", "t")
				_, a := apprPausedDone(t, s, th.ID)
				resume, _, _ := s.CreateRun(ctx, apprResume(th.ID, a.ID, store.DecisionApprove, "", store.ApprovalResponse{}))
				switch status {
				case store.StatusCancelled:
					if ok, err := s.CancelQueuedRun(ctx, resume.ID); err != nil || !ok {
						t.Fatalf("cancel queued: %v %v", ok, err)
					}
				case store.StatusInterrupted:
					if _, err := s.ClaimRun(ctx, resume.ID, "rt"); err != nil {
						t.Fatal(err)
					}
					if err := s.FinishRun(ctx, resume.ID, "rt", status, "", ""); err != nil {
						t.Fatal(err)
					}
				default:
					apprFinish(t, s, resume.ID, status)
				}

				// Act
				reopened, err := s.ReopenApproval(ctx, a.ID)

				// Assert
				if err != nil || reopened.Status != store.ApprovalPending {
					t.Fatalf("reopen after %s: %+v %v", status, reopened, err)
				}
			})
		}
	})

	t.Run("an approval does not reopen when it is not resolved, a later run exists or its call has a result", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		ctx := context.Background()
		th := apprThread(t, s, "alice", "t")
		_, a := apprPausedDone(t, s, th.ID)
		_, pendingErr := s.ReopenApproval(ctx, a.ID)
		_, unknownErr := s.ReopenApproval(ctx, uuid.NewString())
		_, malformedErr := s.ReopenApproval(ctx, "not-a-uuid")
		resume, _, _ := s.CreateRun(ctx, apprResume(th.ID, a.ID, store.DecisionApprove, "", store.ApprovalResponse{}))
		apprFinish(t, s, resume.ID, store.StatusCompleted)
		later, _, _ := s.CreateRun(ctx, apprNewRun(th.ID, "next"))

		// Act
		_, whileLaterActive := s.ReopenApproval(ctx, a.ID)
		apprFinish(t, s, later.ID, store.StatusCompleted)
		_, afterLater := s.ReopenApproval(ctx, a.ID)

		// Assert
		for name, err := range map[string]error{"pending": pendingErr, "unknown": unknownErr, "malformed": malformedErr,
			"later run active": whileLaterActive, "later run finished": afterLater} {
			if !errors.Is(err, store.ErrApprovalNotPending) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if got := apprGet(t, s, a.ID); got.Status != store.ApprovalResolved || got.Decision != store.DecisionApprove {
			t.Fatalf("approval changed: %+v", got)
		}
	})

	t.Run("approvals follow a thread's interaction mode", func(t *testing.T) {
		approvalsModes(t, f)
	})
	t.Run("an approval with scope run is inherited along the run chain", func(t *testing.T) {
		approvalsScopes(t, f)
	})
	t.Run("approvals expire", func(t *testing.T) {
		approvalsExpiry(t, f)
	})
	t.Run("sanitizes NUL and invalid UTF-8 like Postgres", func(t *testing.T) {
		approvalsSanitize(t, f)
	})
}
