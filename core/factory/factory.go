// Package factory is the vocabulary of the agent factory: tasks that agents
// work on unattended, the triggers that create them, and the ports the factory
// features share (the task store, the code forge, the controller that resumes
// or blocks a task, the escalation channel). It holds types and interfaces
// only; the features under features/factory implement the behaviour and
// adapters/store/factorystore the persistence.
package factory

import (
	"context"
	"errors"
	"time"
)

// State is where a task is in its life.
type State string

// Task states. Pending waits for DependsOn; Ready may be dispatched; Running
// has an agent run working on it; Review has an open pull request that CI and
// the reviewer are judging; Blocked needs a human (the reason says why).
const (
	StatePending   State = "pending"
	StateReady     State = "ready"
	StateRunning   State = "running"
	StateReview    State = "review"
	StateBlocked   State = "blocked"
	StateDone      State = "done"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
)

// Terminal reports whether no further work happens in s.
func (s State) Terminal() bool { return s == StateDone || s == StateFailed || s == StateCancelled }

// Valid reports whether s is one of the states above.
func (s State) Valid() bool {
	switch s {
	case StatePending, StateReady, StateRunning, StateReview, StateBlocked, StateDone, StateFailed, StateCancelled:
		return true
	}
	return false
}

// Policy names (Task.Policy). The policy feature maps a name to rules.
const (
	PolicySupervised = "supervised" // every risky call asks a human
	PolicySandboxed  = "sandboxed"  // free inside the sandbox; push, merge and secrets ask
	PolicyAutonomous = "autonomous" // free except what the protected rules deny
)

// Roles built into the delegate feature.
const (
	RolePlanner  = "planner"
	RoleCoder    = "coder"
	RoleReviewer = "reviewer"
	RoleTester   = "tester"
	RoleExplorer = "explorer"
)

// Budget caps what a task may spend in total (all attempts, delegated runs
// included). A zero field is no cap on that axis.
type Budget struct {
	MaxUSD    float64 `json:"maxUsd,omitempty"`
	MaxTokens int64   `json:"maxTokens,omitempty"`
}

// Spend is what a task has used so far (also the unit of AddSpend).
type Spend struct {
	USD    float64 `json:"usd"`
	Tokens int64   `json:"tokens"`
	Runs   int     `json:"runs"`
	// Unpriced counts runs of a model with no known price: their USD is 0, not free.
	Unpriced int `json:"unpriced,omitempty"`
}

// Source kinds (Source.Kind).
const (
	SourceManual     = "manual"
	SourceGitHub     = "github"
	SourceCron       = "cron"
	SourceSupervisor = "supervisor"
	// SourceDelegate marks a sub-task that the delegate tool runs inline in the
	// delegating run: the dispatcher never picks it up.
	SourceDelegate = "delegate"
)

// Task event kinds (TaskEvent.Kind) and the Data keys the features agree on.
//
//	ci     {"sha": head sha, "state": "passed"|"failed"|"pending"}   written by the forge loop
//	review {"sha": head sha, "verdict": "approve"|"changes"|"error"}  written by the review gate
//	budget {"reason": ...}   policy {"tool": ..., "rule": ...}   escalated {"reason": ...}
const (
	EventCreated   = "created"
	EventState     = "state"
	EventRun       = "run"
	EventPR        = "pr"
	EventCI        = "ci"
	EventReview    = "review"
	EventBudget    = "budget"
	EventPolicy    = "policy"
	EventEscalated = "escalated"
	EventLesson    = "lesson"
	EventNote      = "note"
)

// Over reports whether s exceeds b.
func (b Budget) Over(s Spend) bool {
	return (b.MaxUSD > 0 && s.USD >= b.MaxUSD) || (b.MaxTokens > 0 && s.Tokens >= b.MaxTokens)
}

// Source says where a task came from. Key is unique per project when not
// empty: creating a task with an existing key returns the existing one, which
// makes webhook redelivery and cron overlap idempotent
// (for example "github:issue:acme/app#12" or "cron:nightly:2026-10-31T02:00").
type Source struct {
	Kind string `json:"kind"` // manual, github, cron, delegate, supervisor
	Key  string `json:"key,omitempty"`
	URL  string `json:"url,omitempty"`
}

// PullRequest is the pull request of a task.
type PullRequest struct {
	Number  int    `json:"number,omitempty"`
	URL     string `json:"url,omitempty"`
	HeadSHA string `json:"headSha,omitempty"`
}

// Task is one unit of work for an agent.
type Task struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId,omitempty"`
	// Owner is the login the task's runs execute as.
	Owner string `json:"owner"`
	Title string `json:"title"`
	// Spec is what to do, in the words the agent will read.
	Spec   string `json:"spec"`
	Role   string `json:"role,omitempty"` // "" is RoleCoder
	Source Source `json:"source"`
	State  State  `json:"state"`
	// BlockedReason is set while State is StateBlocked; Error when it failed.
	BlockedReason string `json:"blockedReason,omitempty"`
	Error         string `json:"error,omitempty"`
	// Result is the agent's final report of the last attempt.
	Result string `json:"result,omitempty"`

	ParentID  string   `json:"parentId,omitempty"`
	DependsOn []string `json:"dependsOn,omitempty"`
	Labels    []string `json:"labels,omitempty"`
	Priority  int      `json:"priority,omitempty"` // higher first
	// NotBefore delays dispatch (retry backoff).
	NotBefore time.Time `json:"notBefore,omitzero"`

	// ThreadID and RunID are those of the current (or last) attempt.
	ThreadID string `json:"threadId,omitempty"`
	RunID    string `json:"runId,omitempty"`
	Model    string `json:"model,omitempty"`

	// Repo is "owner/name"; BaseBranch the branch the PR targets; Branch the
	// agent's working branch.
	Repo       string      `json:"repo,omitempty"`
	BaseBranch string      `json:"baseBranch,omitempty"`
	Branch     string      `json:"branch,omitempty"`
	PR         PullRequest `json:"pr,omitzero"`

	// AutoMerge lets the PR loop merge once CI is green and the gate approved;
	// false leaves the merge to a human (the task stays in review, escalated).
	AutoMerge   bool   `json:"autoMerge,omitempty"`
	Attempts    int    `json:"attempts"`
	MaxAttempts int    `json:"maxAttempts,omitempty"` // 0: the service default
	Policy      string `json:"policy,omitempty"`      // "": the service default
	Budget      Budget `json:"budget,omitzero"`
	Spent       Spend  `json:"spent"`
	// Fingerprints are the failure signatures of the attempts, newest last
	// (the budget feature blocks a task that fails the same way repeatedly).
	Fingerprints []string `json:"fingerprints,omitempty"`
	// Feedback is what the next attempt must address (CI log, review findings).
	Feedback string `json:"feedback,omitempty"`

	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
}

// Patch is a partial update of a task. Nil fields are left alone.
type Patch struct {
	// IfState makes the update conditional: ErrConflict unless the task is in
	// one of these states (empty: unconditional).
	IfState []State

	State         *State
	BlockedReason *string
	Error         *string
	Result        *string
	Title         *string
	Spec          *string
	ThreadID      *string
	RunID         *string
	Model         *string
	Branch        *string
	PR            *PullRequest
	Attempts      *int
	MaxAttempts   *int
	AutoMerge     *bool
	Policy        *string
	Budget        *Budget
	NotBefore     *time.Time
	Feedback      *string
	DependsOn     *[]string
	Labels        *[]string
	// AddFingerprint appends a failure signature (kept to the last 10).
	AddFingerprint string
}

// Filter selects tasks (all fields optional, ANDed).
type Filter struct {
	ProjectID string
	Owner     string
	States    []State
	ParentID  string
	ThreadID  string
	SourceKey string
	Limit     int // 0: 200
}

// TaskEvent is one line of a task's history.
type TaskEvent struct {
	ID     int64          `json:"id"`
	TaskID string         `json:"taskId"`
	Kind   string         `json:"kind"` // created, state, run, pr, review, ci, budget, policy, escalated, lesson, note
	Text   string         `json:"text"`
	Data   map[string]any `json:"data,omitempty"`
	At     time.Time      `json:"at"`
}

// Trigger kinds.
const (
	TriggerCron    = "cron"
	TriggerWebhook = "webhook"
)

// Template is the task a trigger creates. Title and Spec are Go text/template
// strings rendered with the event (webhook) or the fire time (cron).
type Template struct {
	Title       string   `json:"title"`
	Spec        string   `json:"spec"`
	Role        string   `json:"role,omitempty"`
	Policy      string   `json:"policy,omitempty"`
	Model       string   `json:"model,omitempty"`
	Repo        string   `json:"repo,omitempty"`
	BaseBranch  string   `json:"baseBranch,omitempty"`
	Budget      Budget   `json:"budget,omitzero"`
	MaxAttempts int      `json:"maxAttempts,omitempty"`
	AutoMerge   bool     `json:"autoMerge,omitempty"`
	Labels      []string `json:"labels,omitempty"`
	Priority    int      `json:"priority,omitempty"`
}

// Trigger creates tasks on a schedule or on a webhook event.
type Trigger struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId,omitempty"`
	Owner     string `json:"owner"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	// Schedule is a five-field cron expression or "@every <duration>" (cron).
	Schedule string `json:"schedule,omitempty"`
	// Event is "<github event>[.<action>]", for example "issues.labeled" or
	// "check_suite.completed" (webhook). Match narrows it: every key must equal
	// the normalized event field of that name (label, repo, branch, conclusion).
	Event    string            `json:"event,omitempty"`
	Match    map[string]string `json:"match,omitempty"`
	Template Template          `json:"template"`
	Enabled  bool              `json:"enabled"`

	NextFireAt  time.Time `json:"nextFireAt,omitzero"`
	LastFiredAt time.Time `json:"lastFiredAt,omitzero"`
	CreatedAt   time.Time `json:"createdAt"`
}

// Event is a normalized outside event (a GitHub webhook delivery, a cron tick).
// Fields holds the flat values Trigger.Match compares and templates read.
type Event struct {
	DeliveryID string            `json:"deliveryId,omitempty"`
	Name       string            `json:"name"` // "issues.labeled"
	Repo       string            `json:"repo,omitempty"`
	Fields     map[string]string `json:"fields,omitempty"`
	// Key identifies the thing the event is about ("github:issue:acme/app#12"); it becomes Source.Key.
	Key string `json:"key,omitempty"`
	URL string `json:"url,omitempty"`
	At  time.Time
}

// Errors of the ports.
var (
	ErrNotFound = errors.New("factory: not found")
	// ErrConflict: a conditional update found the task in another state.
	ErrConflict = errors.New("factory: state conflict")
	// ErrInvalid wraps a rejected input.
	ErrInvalid = errors.New("factory: invalid")
)

// Store persists tasks, their history, triggers and webhook deliveries. A
// Store is safe for concurrent use, also across processes for the Postgres one.
type Store interface {
	// CreateTask stores t (a fresh ID, StatePending or StateReady by DependsOn,
	// CreatedAt when unset). When t.Source.Key is not empty and a task of the
	// project has it, that task is returned with created=false.
	CreateTask(ctx context.Context, t Task) (task *Task, created bool, err error)
	GetTask(ctx context.Context, id string) (*Task, error)
	// UpdateTask applies p; ErrConflict when p.IfState does not hold.
	UpdateTask(ctx context.Context, id string, p Patch) (*Task, error)
	ListTasks(ctx context.Context, f Filter) ([]Task, error)
	// AddSpend adds d atomically and returns the task.
	AddSpend(ctx context.Context, id string, d Spend) (*Task, error)
	// SpendSince is the total spend of the project's tasks created or updated since t ("" project: all).
	SpendSince(ctx context.Context, projectID string, t time.Time) (Spend, error)

	AppendEvent(ctx context.Context, e TaskEvent) error
	ListEvents(ctx context.Context, taskID string, limit int) ([]TaskEvent, error)

	PutTrigger(ctx context.Context, t Trigger) (*Trigger, error)
	GetTrigger(ctx context.Context, id string) (*Trigger, error)
	ListTriggers(ctx context.Context, projectID string) ([]Trigger, error)
	DeleteTrigger(ctx context.Context, id string) error
	// ClaimDueTriggers returns the enabled cron triggers whose NextFireAt is at
	// or before now and moves each one's NextFireAt to next(trigger), so that
	// exactly one process fires a given tick.
	ClaimDueTriggers(ctx context.Context, now time.Time, next func(Trigger) time.Time) ([]Trigger, error)

	// SeenDelivery records a webhook delivery id and reports whether it was already recorded.
	SeenDelivery(ctx context.Context, id string) (bool, error)

	// SetThreadInfo records what a thread is for (replaces); GetThreadInfo
	// returns ErrNotFound for an ordinary thread. Stored, not kept in process
	// memory, because the worker that runs a thread is not always the process
	// that created it.
	SetThreadInfo(ctx context.Context, ti ThreadInfo) error
	GetThreadInfo(ctx context.Context, threadID string) (*ThreadInfo, error)
}

// ThreadInfo tells the factory features what a thread is for: the task it
// works on, the role it plays, the policy that governs its tools.
type ThreadInfo struct {
	ThreadID       string `json:"threadId"`
	Login          string `json:"login,omitempty"`
	TaskID         string `json:"taskId,omitempty"`
	Role           string `json:"role,omitempty"`
	Instructions   string `json:"instructions,omitempty"`
	Policy         string `json:"policy,omitempty"`
	ParentThreadID string `json:"parentThreadId,omitempty"`
	// ReadOnly threads run in plan mode.
	ReadOnly bool `json:"readOnly,omitempty"`
	// Repo and BaseBranch are the task's, copied so tools need not load the task.
	Repo       string `json:"repo,omitempty"`
	BaseBranch string `json:"baseBranch,omitempty"`
	Branch     string `json:"branch,omitempty"`
}

// Controller is how the features act on a task without knowing the scheduler.
// The tasks feature implements it.
type Controller interface {
	// Resume puts a Running/Review/Blocked task back to Ready with feedback for
	// its next attempt, or blocks it when attempts ran out.
	Resume(ctx context.Context, taskID, feedback string) error
	// Block parks the task for a human and escalates.
	Block(ctx context.Context, taskID, reason string) error
	// Complete marks the task done with a result.
	Complete(ctx context.Context, taskID, result string) error
	// Fail marks the task failed.
	Fail(ctx context.Context, taskID, reason string) error
	// Review moves the task to StateReview with its pull request.
	Review(ctx context.Context, taskID string, pr PullRequest) error
	// Create adds a task (a delegated sub-task, a follow-up).
	Create(ctx context.Context, t Task) (*Task, bool, error)
}

// Observer is told once when a task reaches a terminal state (done, failed,
// cancelled) or is blocked for a human, with its history. The lessons feature
// is one: it distils what the task taught.
type Observer interface {
	TaskFinished(ctx context.Context, t Task, history []TaskEvent)
}

// Escalator tells a human a task needs them (Telegram, the live feed, a log).
type Escalator interface {
	Escalate(ctx context.Context, t Task, reason string)
}

// EscalatorFunc adapts a function to an Escalator.
type EscalatorFunc func(ctx context.Context, t Task, reason string)

// Escalate implements Escalator.
func (f EscalatorFunc) Escalate(ctx context.Context, t Task, reason string) { f(ctx, t, reason) }

// CheckState is the state of one CI check.
type CheckState string

// Check states.
const (
	CheckPending CheckState = "pending"
	CheckPassed  CheckState = "passed"
	CheckFailed  CheckState = "failed"
)

// Check is one CI check of a pull request head.
type Check struct {
	Name   string     `json:"name"`
	State  CheckState `json:"state"`
	URL    string     `json:"url,omitempty"`
	Detail string     `json:"detail,omitempty"` // the failing step or summary
}

// Comment is a review or issue comment on a pull request.
type Comment struct {
	Author string `json:"author"`
	Body   string `json:"body"`
	Path   string `json:"path,omitempty"`
	Line   int    `json:"line,omitempty"`
	// Review is "approved", "changes_requested" or "" for a plain comment.
	Review string `json:"review,omitempty"`
}

// PRInfo is the live state of a pull request.
type PRInfo struct {
	Number    int    `json:"number"`
	URL       string `json:"url"`
	Title     string `json:"title"`
	State     string `json:"state"` // open, closed, merged
	Draft     bool   `json:"draft,omitempty"`
	Base      string `json:"base"`
	Head      string `json:"head"`
	HeadSHA   string `json:"headSha"`
	Mergeable *bool  `json:"mergeable,omitempty"` // nil: GitHub has not computed it yet
}

// OpenPR is the input of Forge.OpenPR.
type OpenPR struct {
	Repo, Title, Body, Head, Base string
	Draft                         bool
}

// MergeMethod of Forge.Merge.
type MergeMethod string

// Merge methods.
const (
	MergeSquash MergeMethod = "squash"
	MergeMerge  MergeMethod = "merge"
	MergeRebase MergeMethod = "rebase"
)

// Forge is the code host (GitHub). Repo is always "owner/name".
type Forge interface {
	OpenPR(ctx context.Context, in OpenPR) (*PRInfo, error)
	GetPR(ctx context.Context, repo string, number int) (*PRInfo, error)
	// Diff is the unified diff of the pull request.
	Diff(ctx context.Context, repo string, number int) (string, error)
	// Checks lists the CI checks (check runs and commit statuses) of a commit.
	Checks(ctx context.Context, repo, sha string) ([]Check, error)
	// FailureLog is the tail of the failing job's log of a check ("" when none is available).
	FailureLog(ctx context.Context, repo string, c Check) (string, error)
	Comments(ctx context.Context, repo string, number int) ([]Comment, error)
	// Comment posts a comment on the pull request.
	Comment(ctx context.Context, repo string, number int, body string) error
	// Review submits a review: event is "APPROVE", "REQUEST_CHANGES" or "COMMENT".
	Review(ctx context.Context, repo string, number int, event, body string) error
	Merge(ctx context.Context, repo string, number int, sha string, m MergeMethod) error
	// DefaultBranch of the repository.
	DefaultBranch(ctx context.Context, repo string) (string, error)
	// ProtectedBranches lists the branches an agent must never push to directly.
	ProtectedBranches(ctx context.Context, repo string) ([]string, error)
}

// Verdict is a Gate's decision on a pull request.
type Verdict struct {
	Approve bool `json:"approve"`
	// Summary is one paragraph for the PR comment and the task history.
	Summary string `json:"summary"`
	// Findings are what must change when Approve is false (fed to the next attempt).
	Findings []string `json:"findings,omitempty"`
	// Hard means the problem is not fixable by another attempt (a protected
	// path touched, a diff far over the limit): the task blocks for a human.
	Hard bool `json:"hard,omitempty"`
}

// Gate decides whether a pull request whose checks passed may merge. An error
// means the gate could not decide (the loop retries later, then escalates).
type Gate interface {
	Judge(ctx context.Context, t Task, pr PRInfo, checks []Check) (Verdict, error)
}

// RepoAuth mints a short-lived token with read and write access to repo
// ("owner/name"): to clone, push and call the forge. The token is never put on
// a command line.
type RepoAuth interface {
	Token(ctx context.Context, repo string) (string, error)
}

// Spawner starts a full agent run in a fresh thread and waits for its end.
// The facade implements it over harness.Harness.
type Spawner interface {
	// Spawn creates a thread for login, runs prompt on it with the model and
	// role instructions, and returns when the run ended.
	Spawn(ctx context.Context, req SpawnRequest) (*SpawnResult, error)
}

// SpawnRequest is the input of Spawner.Spawn.
type SpawnRequest struct {
	Login  string
	Title  string
	Prompt string
	// Model "" uses the default model.
	Model string
	// Role is the role name; Instructions is the role's prompt text, added to
	// the thread's prompt (stored in ThreadInfo, rendered by the tasks feature's
	// prompt section).
	Role         string
	Instructions string
	// ReadOnly runs the thread in plan mode: no tool that changes anything.
	ReadOnly bool
	// Policy is the factory policy name the thread runs under ("" none).
	Policy string
	// TaskID links the thread to a factory task ("" none).
	TaskID string
	// ParentThreadID is the delegating thread, for budgets and the audit trail.
	ParentThreadID string
	// Repo ("owner/name"), BaseBranch and Branch: the repository the run works
	// on. The forge tools clone it into /workspace/repo on Branch when the
	// thread's sandbox is created. "" for a task without a repository.
	Repo, BaseBranch, Branch string
	// NoSandbox runs without sandbox tools (the reviewer reads diffs from the prompt).
	NoSandbox bool
	// Detach returns right after the run was queued instead of waiting; the
	// result then has no Status or Answer, and whoever cares learns of the end
	// from a runtime.Hook (the tasks feature does).
	Detach bool
	// Interactive makes the run's risky tool calls wait for approval (the run
	// then ends with WaitingFor set). False runs them without asking, which is
	// what policy-governed task runs use for everything the policy allows.
	Interactive bool
}

// SpawnResult is what a spawned run produced.
type SpawnResult struct {
	ThreadID string
	RunID    string
	// Status is the run's final status (completed, failed, cancelled, interrupted).
	Status string
	// Answer is the last assistant message.
	Answer string
	// WaitingFor is "approval" or "question" when the run ended waiting for a
	// human answer (Status is then "completed"), else "".
	WaitingFor string
	Usage      Spend
}
