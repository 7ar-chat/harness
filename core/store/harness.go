package store

import (
	"context"
	"errors"
	"time"
)

// Run kinds.
const (
	KindChat    = "chat"
	KindGoal    = "goal"
	KindCompact = "compact"
)

// Thread title sources: auto (derived from the first message), user (set by the
// user, never overwritten), generated (LLM title after the first completed run).
const (
	TitleAuto      = "auto"
	TitleUser      = "user"
	TitleGenerated = "generated"
)

// Goal statuses.
const (
	GoalActive    = "active"
	GoalMet       = "met"
	GoalExhausted = "exhausted"
	GoalCleared   = "cleared"
)

// Plan item statuses.
const (
	PlanPending    = "pending"
	PlanInProgress = "in_progress"
	PlanCompleted  = "completed"
)

var (
	// ErrInvalidReplace means replaceFromMessageId is not a visible user message of the thread.
	ErrInvalidReplace = errors.New("invalid replace target")
	// ErrNoPendingQuestion means a resume does not match the thread's open question.
	ErrNoPendingQuestion = errors.New("no pending question")
	// ErrQuestionPending means the run kind cannot start while a question waits for an answer.
	ErrQuestionPending = errors.New("a question is pending")
	// ErrGoalInactive means a goal run was requested for a goal that is no longer active or out of turns.
	ErrGoalInactive = errors.New("goal is not active")
)

// PlanItem is one entry of the agent's update_plan todo list.
type PlanItem struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

// QuestionOption is an ask_user answer choice.
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// PendingQuestion is an ask_user question waiting for the user's answer.
type PendingQuestion struct {
	InterruptID   string           `json:"interruptId"`
	RunID         string           `json:"runId"`
	ToolCallID    string           `json:"toolCallId"`
	Question      string           `json:"question"`
	Options       []QuestionOption `json:"options"`
	AllowFreeText bool             `json:"allowFreeText"`
}

// GoalVerdict is the last evaluation of a goal.
type GoalVerdict struct {
	Met    bool   `json:"met"`
	Reason string `json:"reason"`
	// Blockers are what the judge found still missing or wrong (harness v3 §4).
	Blockers []string  `json:"blockers,omitempty"`
	At       time.Time `json:"at"`
}

// Goal is the thread's goal (§6).
type Goal struct {
	ID           string
	ThreadID     string
	Text         string
	Status       string
	TurnsUsed    int
	MaxTurns     int
	LastVerdict  *GoalVerdict
	EvalFailures int
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// GoalUpdate records an evaluation of the goal with id ID (ignored when the goal changed meanwhile).
type GoalUpdate struct {
	ID      string
	Verdict GoalVerdict
	// EvalFailed counts a consecutive evaluation failure (reset otherwise).
	EvalFailed bool
	// Status is the new status ("" keeps it).
	Status string
}

// Feedback is a user's rating of an assistant message.
type Feedback struct {
	Sentiment string `json:"sentiment"`
	Comment   string `json:"comment,omitempty"`
}

// ThreadSummary is the compaction summary of the messages up to ThroughPosition.
type ThreadSummary struct {
	Text            string
	ThroughPosition int64
}

// Compaction is a completed context compaction, kept so the UI can show its
// divider after a reload (after the message AfterMessageID).
type Compaction struct {
	ID             string
	ThreadID       string
	RunID          string
	AfterMessageID string
	Trigger        string
	TokensBefore   int
	TokensAfter    int
	CreatedAt      time.Time
}

// Resume answers a thread's pending ask_user question (or decides its pending
// approval) when a run is created.
type Resume struct {
	InterruptID string
	// Content is the ask_user tool result given to the model.
	Content string
	// Decision (approve, deny, respond) and Response decide a pending approval
	// whose id is InterruptID; a cancelled resume of an approval is a deny.
	Decision string
	Response ApprovalResponse
	// Scope of an approve decision: once (default) or run (ApprovalScopeRun).
	Scope string
}

// ImplicitCancelResult is the ask_user result stored when the user sends a new message instead of answering.
const ImplicitCancelResult = "The user did not answer the question and sent a new message instead."

// HarnessStore is the persistence of the harness v2 features.
type HarnessStore interface {
	// SearchThreads is ListThreads restricted to threads whose title or visible message content contains search.
	SearchThreads(ctx context.Context, login, search string, limit int, before *ThreadCursor) ([]*Thread, error)

	GetThreadSummary(ctx context.Context, threadID string) (ThreadSummary, error)
	SetThreadSummary(ctx context.Context, threadID string, s ThreadSummary) error
	SetThreadPlan(ctx context.Context, threadID string, items []PlanItem) error
	// SetPendingQuestion opens (or, with nil, clears) the thread's ask_user question.
	SetPendingQuestion(ctx context.Context, threadID string, q *PendingQuestion) error
	// SetGeneratedTitle replaces an auto-derived title; false when the title is not auto any more.
	SetGeneratedTitle(ctx context.Context, threadID, title string) (bool, error)

	// PutGoal replaces the thread's goal with a new active one.
	PutGoal(ctx context.Context, login, threadID, text string, maxTurns int) (*Goal, error)
	// GetGoal returns the goal of an owned thread (ErrNotFound when none).
	GetGoal(ctx context.Context, login, threadID string) (*Goal, error)
	// ClearGoal sets the goal status to cleared (ErrNotFound when none).
	ClearGoal(ctx context.Context, login, threadID string) error
	// ThreadGoal returns a thread's goal without ownership check (runtime); nil when none.
	ThreadGoal(ctx context.Context, threadID string) (*Goal, error)
	// UpdateGoal records an evaluation; returns the goal as stored afterwards (nil when none).
	UpdateGoal(ctx context.Context, threadID string, u GoalUpdate) (*Goal, error)

	// SetFeedback rates a visible assistant message of one of the user's threads (ErrNotFound otherwise).
	SetFeedback(ctx context.Context, login, messageID string, f Feedback) error
	DeleteFeedback(ctx context.Context, login, messageID string) error
	// ListFeedback returns the feedback of a thread's messages by message id.
	ListFeedback(ctx context.Context, threadID string) (map[string]Feedback, error)

	// AppendToolVerification appends checker results to metadata.verification
	// of the visible tool message of a thread's call (round 3, N2: the checks
	// of files past the per-call cap, run before the next model call); nothing
	// when the call has no stored result.
	AppendToolVerification(ctx context.Context, threadID, toolCallID string, items []any) error

	// RecordCompaction records a completed compaction after the thread's last
	// visible message (nothing when the thread has no message).
	RecordCompaction(ctx context.Context, c Compaction) error
	// ListCompactions returns the compactions whose message is still visible, oldest first.
	ListCompactions(ctx context.Context, threadID string) ([]Compaction, error)
}
