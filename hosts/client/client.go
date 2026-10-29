// Package client is the contract between a front end and the harness engine.
//
// The engine is a library (package harness). A front end (the terminal UI, the
// web UI, a Slack bot, a desktop app) should not be written against it: it
// is written against Client, a narrow interface of what a conversation UI
// needs: threads, runs, a resumable event stream, steering, cancelling,
// answering an approval or a question, and a thread's files. Two
// implementations satisfy it and behave the same (clienttest.Run checks both):
//
//   - InProcess drives a harness.Harness living in the same process;
//   - HTTP speaks the JSON + Server-Sent Events API served by hosts/web, so a
//     front end can attach to a remote harness-web.
//
// A Client acts for one user: the in-process one is built for a login, the HTTP
// one authenticates with a bearer token. Threads and runs of other users are
// reported as ErrNotFound.
//
// The DTOs below are the wire format of hosts/web, which serves exactly these
// types; changing a JSON tag changes the HTTP API.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/modelinfo"
)

// Client is what a front end needs from a harness.
type Client interface {
	// Models lists the model ids runs may use and the default one.
	Models(ctx context.Context) (ModelList, error)

	// CreateThread starts a conversation (an empty title is titled from the first message).
	CreateThread(ctx context.Context, title string) (*Thread, error)
	// ListThreads returns the user's threads, pinned first then newest (limit <= 0: 50; at most 200).
	ListThreads(ctx context.Context, limit int) ([]Thread, error)
	// GetThread returns a thread with its messages, pending approval or question and active run.
	GetThread(ctx context.Context, threadID string) (*Thread, error)
	// RenameThread changes the title and/or the pinned flag.
	RenameThread(ctx context.Context, threadID string, p ThreadPatch) (*Thread, error)
	// DeleteThread removes a thread and its runs.
	DeleteThread(ctx context.Context, threadID string) error

	// Send adds a user message to the thread and queues a run answering it.
	// Follow the run with Events.
	Send(ctx context.Context, threadID, text string, o SendOptions) (*Run, error)
	// GetRun returns a run's status.
	GetRun(ctx context.Context, runID string) (*Run, error)
	// Events streams the run's AG-UI events with Seq > after, then follows the
	// run live and closes the channel after its terminal event (RUN_FINISHED or
	// RUN_ERROR) or when ctx is done. The error reports a run that cannot be
	// streamed (ErrNotFound...). A broken connection is resumed from the last
	// seq received: the stream has no gap and no duplicate. When the connection
	// cannot be recovered the channel ends with a synthetic RUN_ERROR event
	// (code "stream_lost").
	Events(ctx context.Context, runID string, after int64) (<-chan Event, error)
	// Steer adds a message to a running run: the model sees it at its next step.
	// ErrRunNotActive when the run already ended.
	Steer(ctx context.Context, runID, text string) error
	// Cancel stops a run (a finished run is left alone).
	Cancel(ctx context.Context, runID string) error
	// Resume answers the interrupt a run ended with (a tool approval or an
	// ask_user question) and queues the run that continues the conversation.
	Resume(ctx context.Context, threadID string, r ResumeRequest) (*Run, error)

	// ListFiles lists a directory of the thread's workspace ("" is its root).
	ListFiles(ctx context.Context, threadID, path string) ([]File, error)
	// DownloadFile returns the content of a file of the thread's workspace.
	DownloadFile(ctx context.Context, threadID, path string) (*Download, error)

	// Sessions lists the saved conversations (ErrNotSupported without a durable store).
	Sessions(ctx context.Context, limit int) ([]Session, error)
}

// Event is one AG-UI event of a run. Seq numbers the run's events from 1 and is
// the resume point of Events.
type Event struct {
	Seq  int64
	Type string
	// Data is the full AG-UI event JSON (including its type and timestamp).
	Data json.RawMessage
}

// Reasons of an interrupt and decisions of a tool approval.
const (
	ReasonToolApproval = store.ReasonToolApproval
	DecisionApprove    = store.DecisionApprove
	DecisionDeny       = store.DecisionDeny
	DecisionRespond    = store.DecisionRespond
)

// Roles of a Message.
const (
	RoleUser      = store.RoleUser
	RoleAssistant = store.RoleAssistant
	RoleTool      = store.RoleTool
)

// ModelList is the answer of Models.
type ModelList struct {
	Models  []string `json:"models"`
	Default string   `json:"default"`
	// Info describes the models it knows (context window, prices, capabilities),
	// by id; absent from servers that have no model table.
	Info map[string]ModelInfo `json:"info,omitempty"`
}

// ModelInfo describes a model; a zero field is unknown. Prices are dollars per token.
type ModelInfo struct {
	Provider      string `json:"provider,omitempty"`
	ContextWindow int    `json:"contextWindow,omitempty"`
	MaxOutput     int    `json:"maxOutput,omitempty"`
	// Priced is false when the model has no price (zero prices then mean unknown).
	Priced             bool    `json:"priced,omitempty"`
	InputPerToken      float64 `json:"inputPerToken,omitempty"`
	OutputPerToken     float64 `json:"outputPerToken,omitempty"`
	CacheReadPerToken  float64 `json:"cacheReadPerToken,omitempty"`
	CacheWritePerToken float64 `json:"cacheWritePerToken,omitempty"`
	// Tier lists the higher prices of prompts above Tier.Above tokens.
	Tier      *PriceTier `json:"tier,omitempty"`
	Vision    bool       `json:"vision,omitempty"`
	Reasoning bool       `json:"reasoning,omitempty"`
	Tools     bool       `json:"tools,omitempty"`
}

// PriceTier is the price list of prompts above Above tokens (dollars per token).
type PriceTier struct {
	Above      int     `json:"above"`
	Input      float64 `json:"input,omitempty"`
	Output     float64 `json:"output,omitempty"`
	CacheRead  float64 `json:"cacheRead,omitempty"`
	CacheWrite float64 `json:"cacheWrite,omitempty"`
}

// Cost is the price in dollars of in input tokens (cached of them served from the
// prompt cache) and out output tokens (reasoning included); prompt is the largest
// single prompt, which selects the price tier (0: the base prices). ok is false
// when the model has no price.
func (m ModelInfo) Cost(in, out, cached, prompt int) (usd float64, ok bool) {
	i := modelinfo.Info{Priced: m.Priced, InputPerToken: m.InputPerToken, OutputPerToken: m.OutputPerToken,
		CacheReadPerToken: m.CacheReadPerToken, CacheWritePerToken: m.CacheWritePerToken}
	if t := m.Tier; t != nil {
		i.Tier = &modelinfo.Tier{Above: t.Above, Input: t.Input, Output: t.Output, CacheRead: t.CacheRead, CacheWrite: t.CacheWrite}
	}
	return modelinfo.Cost(i, modelinfo.Usage{In: in, Out: out, Cached: cached, Prompt: prompt})
}

// TableLookup is the WithModelInfo lookup over a price table; hints are the
// provider names to prefer, tried in order (the configured provider name, then its type).
func TableLookup(t *modelinfo.Table, hints ...string) func(id string) (ModelInfo, bool) {
	return func(id string) (ModelInfo, bool) {
		for _, h := range append(hints, "") {
			if i, ok := t.Lookup(id, h); ok {
				return FromModelInfo(i), true
			}
		}
		return ModelInfo{}, false
	}
}

// FromModelInfo converts a table entry to its wire form.
func FromModelInfo(i modelinfo.Info) ModelInfo {
	m := ModelInfo{Provider: i.Provider, ContextWindow: i.ContextWindow, MaxOutput: i.MaxOutput, Priced: i.Priced,
		InputPerToken: i.InputPerToken, OutputPerToken: i.OutputPerToken, CacheReadPerToken: i.CacheReadPerToken,
		CacheWritePerToken: i.CacheWritePerToken, Vision: i.Vision, Reasoning: i.Reasoning, Tools: i.Tools}
	if t := i.Tier; t != nil {
		m.Tier = &PriceTier{Above: t.Above, Input: t.Input, Output: t.Output, CacheRead: t.CacheRead, CacheWrite: t.CacheWrite}
	}
	return m
}

// ThreadPatch changes a thread; nil fields stay as they are.
type ThreadPatch struct {
	Title  *string `json:"title,omitempty"`
	Pinned *bool   `json:"pinned,omitempty"`
}

// ActiveRun, PendingQuestion, QuestionOption and ToolCall are the engine's own
// wire types, shared so that there is a single definition of them.
type (
	ActiveRun       = store.ActiveRun
	PendingQuestion = store.PendingQuestion
	QuestionOption  = store.QuestionOption
	ToolCall        = store.ToolCall
)

// Thread is a conversation. Messages is set by GetThread only.
type Thread struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Pinned    bool      `json:"pinned"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// ActiveRun is the run in progress, nil when the thread is idle.
	ActiveRun *ActiveRun `json:"activeRun"`
	// PendingApproval is the risky tool call waiting for a decision (id,
	// toolName, arguments...), nil when there is none. Its "id" is the
	// InterruptID of the Resume answering it.
	PendingApproval map[string]any `json:"pendingApproval"`
	// PendingQuestion is the ask_user question waiting for an answer.
	PendingQuestion *PendingQuestion `json:"pendingQuestion"`
	Messages        []Message        `json:"messages,omitempty"`
}

// Message is one message of a thread.
type Message struct {
	ID         string         `json:"id"`
	RunID      string         `json:"runId,omitempty"`
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []ToolCall     `json:"toolCalls,omitempty"`
	ToolCallID string         `json:"toolCallId,omitempty"`
	Error      string         `json:"error,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	CreatedAt  time.Time      `json:"createdAt"`
	// Author is who wrote a user message in a shared thread (nil for the
	// model's messages and for messages without a recorded author).
	Author *Author `json:"author,omitempty"`
}

// Author identifies the speaker of a user message. Kind is "user" or
// "supervisor"; a supervisor acts OnBehalfOf a user. Name and AvatarURL are
// filled in by servers that know their users.
type Author struct {
	Login      string `json:"login"`
	Kind       string `json:"kind,omitempty"`
	OnBehalfOf string `json:"onBehalfOf,omitempty"`
	Name       string `json:"name,omitempty"`
	AvatarURL  string `json:"avatarUrl,omitempty"`
}

// Run is one execution of the agent.
type Run struct {
	ID        string    `json:"id"`
	ThreadID  string    `json:"threadId"`
	Status    string    `json:"status"`
	Model     string    `json:"model"`
	Error     string    `json:"error,omitempty"`
	ErrorCode string    `json:"errorCode,omitempty"`
	LastSeq   int64     `json:"lastSeq"`
	CreatedAt time.Time `json:"createdAt"`
	// Sandbox is the sandbox mode of the run: auto, off or on.
	Sandbox string `json:"sandbox,omitempty"`
}

// SendOptions are the per-run settings of Send; they are also the optional
// fields of the run-creation request.
type SendOptions struct {
	// Model is the model id (default: the server's default model).
	Model string `json:"model,omitempty"`
	// ReasoningEffort is low|medium|high|... ("" for the model's default).
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
	// Skills are skill names requested explicitly for this run.
	Skills []string `json:"skills,omitempty"`
	// Interactive makes risky tool calls wait for a decision (Resume).
	Interactive bool `json:"interactive,omitempty"`
	// Mode is ModeBuild (default) or ModePlan (read-only investigation).
	Mode string `json:"mode,omitempty"`
	// Attachments are files (images) sent with the message.
	Attachments []Attachment `json:"attachments,omitempty"`
	// Sandbox is auto (default: sandbox tools when the server has a backend, created on first use), off (no
	// sandbox tools for this run) or on (fail when the server has no sandbox).
	Sandbox string `json:"sandbox,omitempty"`
}

// RunRequest is the body of the run-creation request (POST /threads/{id}/runs).
type RunRequest struct {
	Message string `json:"message"`
	SendOptions
}

// ResumeRequest answers the interrupt a run ended with (POST /threads/{id}/resume).
type ResumeRequest struct {
	// InterruptID is the interrupt's id (the approval id for a tool approval).
	InterruptID string `json:"interruptId"`
	// Reason is the interrupt reason: ReasonToolApproval or "ask_user" (inferred from Decision when empty).
	Reason string `json:"reason,omitempty"`
	// Decision applies to tool approvals: DecisionApprove, DecisionDeny or DecisionRespond.
	Decision string `json:"decision,omitempty"`
	// Always approves the tool for the rest of the run chain.
	Always bool `json:"always,omitempty"`
	// Text is the answer to an ask_user question, or the free text of a respond.
	Text string `json:"text,omitempty"`
	// Model, ReasoningEffort and Interactive are the settings of the resumed run.
	Model           string `json:"model,omitempty"`
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
	Interactive     bool   `json:"interactive,omitempty"`
	// Mode is ModeBuild or ModePlan ("": the mode of the thread's previous run).
	Mode string `json:"mode,omitempty"`
}

// File is an entry of a thread's workspace.
type File struct {
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	Type       string    `json:"type"` // "file" or "directory"
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modifiedAt"`
}

// Download is a file fetched from a thread's workspace.
type Download struct {
	Path string
	Name string
	Data []byte
}

// Session is a saved conversation.
type Session struct {
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	Preview    string    `json:"preview"`
	Messages   int       `json:"messages"`
	LastStatus string    `json:"lastStatus"`
	Pinned     bool      `json:"pinned"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// Error is an API error: the same value whether it comes from the engine in
// process or from a server's {"error":{"code","message"}} answer. Test it with
// errors.Is against the Err* values.
type Error struct {
	// Status is the HTTP status the error maps to.
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
	cause   error
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// Is matches errors of the same code.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// Unwrap returns the engine error behind an internal error.
func (e *Error) Unwrap() error { return e.cause }

// The errors a Client reports.
var (
	ErrUnauthenticated = &Error{Status: 401, Code: "unauthenticated", Message: "Authentication required."}
	ErrNotFound        = &Error{Status: 404, Code: "not_found", Message: "Not found."}
	ErrConflict        = &Error{Status: 409, Code: "conflict", Message: "Conflict."}
	ErrInvalidRequest  = &Error{Status: 400, Code: "invalid_request", Message: "Invalid request."}
	ErrInvalidPath     = &Error{Status: 400, Code: "invalid_path", Message: "Invalid path."}
	ErrRunNotActive    = &Error{Status: 409, Code: "run_not_active", Message: "The run no longer accepts messages."}
	ErrTooManySteers   = &Error{Status: 429, Code: "too_many_steers", Message: "Too many pending messages."}
	ErrNotSupported    = &Error{Status: 501, Code: "not_supported", Message: "The store has no session listing."}
	ErrNoSandbox       = &Error{Status: 501, Code: "no_sandbox", Message: "This server has no sandbox."}
	ErrTooLarge        = &Error{Status: 413, Code: "too_large", Message: "The file is too large to download."}
)

// errorf returns a copy of sentinel with a specific message.
func errorf(sentinel *Error, format string, args ...any) *Error {
	e := *sentinel
	e.Message = fmt.Sprintf(format, args...)
	return &e
}

// AsError returns err as an *Error (nil when it is not one).
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}
