// Package store holds the domain types and persistence interface.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Run statuses.
const (
	StatusQueued      = "queued"
	StatusRunning     = "running"
	StatusCompleted   = "completed"
	StatusFailed      = "failed"
	StatusCancelled   = "cancelled"
	StatusInterrupted = "interrupted"
)

// Message roles.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
	RoleReasoning = "reasoning"
)

var (
	// ErrNotFound means the row does not exist, is deleted or belongs to someone else.
	ErrNotFound = errors.New("not found")
	// ErrRunActive means the thread already has a queued/running run.
	ErrRunActive = errors.New("thread already has an active run")
	// ErrNotClaimable means the run is no longer queued.
	ErrNotClaimable = errors.New("run is not claimable")
	// ErrConflict means a unique constraint (e.g. message id) was violated.
	ErrConflict = errors.New("conflict")
	// ErrNotOwned means the run is no longer running on this runtime (the
	// sweeper interrupted it, or it was already finished).
	ErrNotOwned = errors.New("run is not running on this runtime")
)

// ErrorCodeFor is the error code stored for a terminal status (empty for successes).
func ErrorCodeFor(status, code string) string {
	switch status {
	case StatusFailed, StatusInterrupted:
		if code == "" {
			if status == StatusInterrupted {
				return "interrupted"
			}
			return "internal"
		}
		return code
	}
	return ""
}

// IsTerminal reports whether a run status is final.
func IsTerminal(status string) bool {
	switch status {
	case StatusQueued, StatusRunning:
		return false
	}
	return true
}

// ActiveRun is the short run reference embedded in a thread.
type ActiveRun struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// Thread is a conversation.
type Thread struct {
	ID        string
	UserLogin string
	Title     string
	PinnedAt  *time.Time
	SandboxID string
	CreatedAt time.Time
	UpdatedAt time.Time
	ActiveRun *ActiveRun
	// Harness v2.
	TitleSource     string
	Plan            []PlanItem
	PendingQuestion *PendingQuestion
	Goal            *Goal
	// Approvals: the last interaction mode chosen on the thread ("" when never
	// set) and the risky tool call waiting for the user's decision.
	InteractionMode string
	PendingApproval *Approval
	// Notifications (harness v3 §8): Unread when a run reached a terminal state
	// after LastSeenAt; LastRun is the latest run (nil without runs).
	LastSeenAt *time.Time
	LastRun    *LastRun
	Unread     bool
}

// Run is one agent execution on a thread.
type Run struct {
	ID              string
	ThreadID        string
	UserLogin       string
	Kind            string
	Status          string
	Model           string
	ReasoningEffort string
	// InteractionMode is interactive or automatic ("" is automatic).
	InteractionMode string
	// Sandbox is the run's sandbox mode: SandboxAuto ("" too), SandboxOff or
	// SandboxOn. A resumed run keeps the mode of the thread's previous run.
	Sandbox string
	// ApprovedTools are the tool kinds approved with scope "run" in this run
	// chain (the run and its resume runs): they run without asking again.
	ApprovedTools   []string
	Error           string
	ErrorCode       string
	CancelRequested bool
	RuntimeID       string
	HeartbeatAt     *time.Time
	LastSeq         int64
	CreatedAt       time.Time
	StartedAt       *time.Time
	FinishedAt      *time.Time
}

// ToolCall is an assistant tool call (OpenAI/AG-UI shape).
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction is the function part of a tool call.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Message is a persisted chat message.
type Message struct {
	ID         string
	ThreadID   string
	RunID      string
	Position   int64
	Role       string
	Content    string
	ToolCalls  []ToolCall
	ToolCallID string
	Error      string
	Metadata   map[string]any
	// ReasoningItems are the opaque reasoning items (Responses API, encrypted by
	// the provider) of an assistant message, sent back to its model with the
	// message (column reasoning_items; never exposed by the API).
	ReasoningItems json.RawMessage
	CreatedAt      time.Time
}

// IsSteer reports whether the message is a steer injected during a run.
func (m *Message) IsSteer() bool {
	v, _ := m.Metadata["steer"].(bool)
	return v
}

// Event is one persisted AG-UI event of a run.
type Event struct {
	Seq  int64
	Type string
	// Data is the full AG-UI event JSON (including type and timestamp).
	Data json.RawMessage
}

// ThreadCursor positions thread pagination (pinned first, then createdAt desc):
// keyed on the immutable creation time, so a thread updated between two page
// fetches is neither skipped nor repeated.
type ThreadCursor struct {
	Pinned    bool
	CreatedAt time.Time
	ID        string
}

// ThreadPatch is a partial thread update.
type ThreadPatch struct {
	Title  *string
	Pinned *bool
}

// NewRun is the input of CreateRun.
type NewRun struct {
	RunID     string
	ThreadID  string
	UserLogin string
	Model     string
	Message   Message
	// AutoTitle is used as the thread title when the thread has none.
	AutoTitle string
	// Skills are skill names explicitly requested for this run (work_runs.skills).
	Skills []string
	// Harness v2. Kind defaults to chat. A Message without Role is not stored
	// (compact runs, resume-only runs).
	Kind            string
	ReasoningEffort string
	// ReplaceFromMessageID supersedes that user message and every later one.
	ReplaceFromMessageID string
	// GoalID claims a turn of that goal (ErrGoalInactive when not active or out of turns).
	GoalID string
	// Resume answers the pending question; without it a pending question is
	// implicitly cancelled by a message (ErrQuestionPending without a message).
	Resume *Resume
	// InteractionMode is interactive or automatic; a non-empty mode is also
	// remembered as the thread's last choice, an empty one uses that choice
	// (automatic when the thread has none).
	InteractionMode string
	// Sandbox is the run's sandbox mode (SandboxAuto, SandboxOff, SandboxOn).
	// Empty inherits the mode of the thread's latest run (resume, goal and
	// compaction runs), else auto.
	Sandbox string
}

// Sandbox modes of a run (Run.Sandbox).
const (
	// SandboxAuto: sandbox tools are offered when a backend is configured; the
	// sandbox is created on the first command.
	SandboxAuto = "auto"
	// SandboxOff: the run has no sandbox tools and never creates a sandbox.
	SandboxOff = "off"
	// SandboxOn: like auto, but the run is refused without a backend.
	SandboxOn = "on"
)

// Store is the persistence boundary used by api, runtime and agent.
type Store interface {
	Ping(ctx context.Context) error

	CreateThread(ctx context.Context, login, title string) (*Thread, error)
	GetThread(ctx context.Context, login, id string) (*Thread, error)
	ListThreads(ctx context.Context, login string, limit int, before *ThreadCursor) ([]*Thread, error)
	UpdateThread(ctx context.Context, login, id string, patch ThreadPatch) (*Thread, error)
	DeleteThread(ctx context.Context, login, id string) (*Thread, error)

	CreateRun(ctx context.Context, in NewRun) (*Run, *Message, error)
	GetRun(ctx context.Context, id string) (*Run, error)
	// GetUserRun is GetRun for the API: ErrNotFound unless the run belongs to
	// login and its thread is not deleted.
	GetUserRun(ctx context.Context, login, id string) (*Run, error)
	ClaimRun(ctx context.Context, id, runtimeID string) (*Run, error)
	// Heartbeat refreshes heartbeat_at; owned=false when the run is no longer
	// running on this runtime.
	Heartbeat(ctx context.Context, id, runtimeID string) (owned, cancelRequested bool, err error)
	RequestCancel(ctx context.Context, id string) (*Run, error)
	// CancelQueuedRun moves a queued run to cancelled; false when it was not queued.
	CancelQueuedRun(ctx context.Context, id string) (bool, error)
	// FinishRun sets a terminal status; failed/interrupted runs store errCode (default "internal").
	// ErrNotOwned when the run is not running on runtimeID any more.
	FinishRun(ctx context.Context, id, runtimeID, status, errCode, errMsg string) error
	ListQueuedRunsOlderThan(ctx context.Context, age time.Duration) ([]string, error)
	// InterruptStaleRuns marks running runs whose heartbeat is older than age as
	// interrupted and returns them; a run any alive check vouches for (its
	// runtime still heartbeats elsewhere, e.g. in Redis during a Postgres
	// outage) is left running.
	InterruptStaleRuns(ctx context.Context, age time.Duration, alive ...func(*Run) bool) ([]*Run, error)

	ListMessages(ctx context.Context, threadID string) ([]*Message, error)
	AppendMessages(ctx context.Context, msgs ...*Message) error

	AppendEvents(ctx context.Context, runID string, events []Event) error
	ListEvents(ctx context.Context, runID string, afterSeq int64, limit int) ([]Event, error)
	// LastEvent returns the highest-seq persisted event of a run.
	LastEvent(ctx context.Context, runID string) (Event, bool, error)
	// SetRunOutcome overwrites a run's terminal status (used when its log proves the outcome).
	SetRunOutcome(ctx context.Context, id, status, errCode, errMsg string) error

	SandboxStore
	ArtifactStore
	BlobStore
	SkillStore
	HarnessStore
	ApprovalStore
	MemoryStore
	NotificationStore
}

// SandboxStore is the subset used by the sandbox manager.
type SandboxStore interface {
	ThreadSandbox(ctx context.Context, threadID string) (string, error)
	// SetThreadSandbox sets sandbox_id when it currently equals expected.
	SetThreadSandbox(ctx context.Context, threadID, expected, sandboxID string) error
	CountUserSandboxes(ctx context.Context, login string) (int, error)
}
