// Package harness is an embeddable agent harness:
// a ReAct loop (eino) whose tools run in a remote sandbox (OpenSandbox), with
// compaction, steering, skills, artifacts, memory and approvals, streaming
// AG-UI events.
//
// It is a library for hosts that own the user interface: a web server (see
// package httpserver), a TUI or a desktop app. A host builds a Harness, starts
// it and then talks to it with a handful of calls:
//
//	h, _ := harness.New(harness.Options{Config: cfg})
//	go h.Run(ctx)
//	th, _ := h.NewThread(ctx, "alice", "My thread")
//	run, _ := h.Send(ctx, "alice", th.ID, "Make a chart of …", harness.RunOptions{Model: "gpt-5"})
//	for ev := range h.Events(ctx, run.ID, 0) { … }   // AG-UI events
//
// Without Redis and Postgres everything lives in the process (memory store,
// event log and queue); pass Options.Store (store/pgstore) and Options.Redis
// to share state between processes.
package harness

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/engine/agent"
	"github.com/GhaziBenDahmane/harness/engine/agui"
	"github.com/GhaziBenDahmane/harness/engine/approvals"
	"github.com/GhaziBenDahmane/harness/engine/eventlog"
	"github.com/GhaziBenDahmane/harness/engine/llm"
	"github.com/GhaziBenDahmane/harness/engine/queue"
	"github.com/GhaziBenDahmane/harness/engine/runtime"
	"github.com/GhaziBenDahmane/harness/engine/tools"
	"github.com/GhaziBenDahmane/harness/features/artifacts"
	"github.com/GhaziBenDahmane/harness/features/skills"
	"github.com/GhaziBenDahmane/harness/features/workspace"
	"github.com/GhaziBenDahmane/harness/sandbox"
	"github.com/GhaziBenDahmane/harness/store"
	"github.com/GhaziBenDahmane/harness/store/memstore"
)

// Store is the persistence the harness needs: the core store plus the
// sandbox, workspace and catalog extensions. store/memstore and store/pgstore
// implement it.
type Store interface {
	store.Store
	sandbox.Store
	store.WorkspaceGitStore
}

// Options configures a Harness. Only Config is required.
type Options struct {
	// Config is the environment-driven configuration (config.Load).
	Config *config.Config
	// Store persists threads, runs, messages, artifacts, memory… (default: in memory;
	// store/pgstore for Postgres).
	Store Store
	// Models builds the chat models (default: llm.New(Config), the OpenAI-compatible
	// provider, or Anthropic with LLM_PROVIDER=anthropic).
	Models llm.Factory
	// Backend provides the sandboxes. Default: the backend named by
	// SANDBOX_BACKEND (local, opensandbox, none, or one registered with
	// sandbox.Register), else OpenSandbox when OPENSANDBOX_URL is set, else
	// none: the agent then has no sandbox tools and a prompt that says so.
	Backend sandbox.Backend
	// Blobs stores artifact contents (default: in the Store).
	Blobs artifacts.Blobs
	// Infra holds the implementations of what several processes share: run
	// queue and control bus, event log, locks, job list, skill installs. Every
	// part is optional and falls back to its in-process version. A host on
	// another transport (NATS, SQS...) injects its own; see RedisInfra.
	Infra *Infra
	// Redis is a convenience for Infra: RedisInfra(Redis, Store). Set at most
	// one of the two. Without either the harness is single-process.
	Redis redis.UniversalClient
	// Features are the capabilities to install, in order (default: the profile
	// of AGENT_PROFILE: ProfileWork, ProfileCoding or ProfileMinimal). Compose
	// your own list from the constructors (Sandbox(), Memory()...) and your
	// own Features; see README "Features and profiles".
	Features []Feature
	// BuiltinSkills holds <name>/SKILL.md folders (default: skills/skillsfs).
	BuiltinSkills fs.FS
	// ExtraSkills holds more <name>/SKILL.md folders, added after the built-ins
	// and before the user directory (config SKILLS_DIR): the host's own skills.
	ExtraSkills fs.FS
	// Tools are extra tool providers (host-defined tools).
	Tools []tools.Provider
	// Hooks run at the end of every run, after the harness' own.
	Hooks []runtime.Hook
	// Version identifies the host to MCP servers.
	Version string
	// RuntimeID names this process in run ownership (default: hostname-random).
	RuntimeID string
}

// Harness runs agent runs for any number of users and threads.
type Harness struct {
	cfg       *config.Config
	Store     Store
	Log       eventlog.Log
	Queue     queue.Queue
	Bus       queue.Bus
	Sandboxes sandbox.Manager
	Artifacts *artifacts.Service
	Skills    *skills.Registry
	Workspace *workspace.Service
	Runtime   *runtime.Runtime
	Agent     *agent.Agent

	sandboxTools bool
}

// New wires the harness: the core (store, models, sandboxes, infrastructure,
// agent loop, runtime) and the features of Options.Features (default: the
// profile of AGENT_PROFILE, see ProfileWork, ProfileCoding, ProfileMinimal).
func New(o Options) (*Harness, error) {
	cfg := o.Config
	if cfg == nil {
		return nil, errors.New("harness: Options.Config is required")
	}
	st := o.Store
	if st == nil {
		st = memstore.New()
	}
	models := o.Models
	if models == nil {
		models = llm.New(cfg)
	}
	backend := o.Backend
	if backend == nil {
		var err error
		backend, err = sandbox.NewBackend(cfg)
		if err != nil {
			return nil, err
		}
	}
	infra, err := resolveInfra(o, st)
	if err != nil {
		return nil, err
	}
	blobs := o.Blobs
	if blobs == nil {
		blobs = artifacts.PGBlobs{S: st}
	}
	features := o.Features
	if features == nil {
		features = profileOf(cfg)
	}

	b := newBuilder(o, cfg, st, models, blobs, infra)
	var coord sandbox.Coordinator
	var locks runtime.Locks
	if infra.Coordinator != nil {
		coord, locks = infra.Coordinator, infra.Coordinator
	}
	mgr := sandbox.NewManager(backend, st, coord, sandbox.ManagerOptions{
		MaxPerUser: cfg.MaxSandboxesPerUser, Timeout: cfg.SandboxTimeout(),
		UnhealthyRecreate: time.Duration(cfg.SandboxUnhealthyRecreate) * time.Second,
		OnCreate:          b.onCreate,
	})
	b.Sandboxes = mgr
	if err := b.install(features); err != nil {
		return nil, err
	}

	hs := agentOptions(cfg, st, mgr)
	b.applyTo(&hs)
	if cfg.SandboxEnabled() || backend != nil {
		hs.Spill = tools.SandboxSpiller{Sandboxes: mgr}
	}
	providers := append(append([]tools.Provider{}, b.providers...), o.Tools...)
	var registry tools.Provider = tools.NewRegistry(providers...)
	for _, w := range b.wrappers {
		registry = w(registry)
	}

	h := &Harness{cfg: cfg, Store: st, Log: infra.Log, Queue: infra.Broker, Bus: infra.Broker,
		Sandboxes: mgr, Artifacts: b.Artifacts, Skills: b.Skills, Workspace: b.Workspace,
		sandboxTools: b.sandboxTools != nil}
	h.Agent = &agent.Agent{
		Models: models, Tools: registry, Store: st, Steers: h.Bus,
		MaxSteps: cfg.AgentMaxSteps, MaxToolOutputChars: cfg.AgentMaxToolOutputChars,
		Harness:           hs,
		ModelTimeout:      time.Duration(cfg.LLMTimeout) * time.Second,
		StreamIdleTimeout: cfg.StreamIdle(),
		CallCeiling:       cfg.CallCeiling(),
		StepRetries:       agent.StepRetriesSetting(cfg.LLMStepRetries),
		SpillChars:        cfg.ToolOutputSpillChars,
	}

	id := o.RuntimeID
	if id == "" {
		host, _ := os.Hostname()
		id = host + "-" + uuid.NewString()[:8]
	}
	jobs := append(append([]runtime.Job{}, b.jobs...), runtime.Job{Name: "approval-expiry", Every: time.Minute,
		Run: func(ctx context.Context) error {
			_, err := st.ExpireApprovals(ctx, time.Duration(cfg.ApprovalTTLHours)*time.Hour, "")
			return err
		}})
	h.Runtime = runtime.New(runtime.Options{
		ID: id, Concurrency: cfg.RuntimeConcurrency, ShutdownGrace: time.Duration(cfg.RuntimeShutdownGrace) * time.Second,
		Hooks:  append(append([]runtime.Hook{}, b.endOfRun...), o.Hooks...),
		Purger: b.purger,
		Jobs:   jobs,
	}, st, h.Log, infra.Broker, h.Bus, h.Agent, locks)
	for _, f := range b.starts {
		go f(context.Background())
	}
	return h, nil
}

// Run consumes queued runs until ctx is done, then lets in-flight runs finish
// (or interrupts them after the shutdown grace).
func (h *Harness) Run(ctx context.Context) error { return h.Runtime.Run(ctx) }

// RunOptions are the per-run settings of Send.
type RunOptions struct {
	// Model is required: an id the Config allows.
	Model string
	// ReasoningEffort is low|medium|high|… ("" for the model's default).
	ReasoningEffort string
	// Skills are skill names requested explicitly for this run.
	Skills []string
	// Interactive makes risky tool calls wait for Approve/Reject.
	Interactive bool
	// MessageID makes Send idempotent for that user message (optional).
	MessageID string
	// Sandbox says whether the run may use a sandbox (default SandboxAuto).
	// It is kept in the run record: a resumed run, a goal turn and a
	// compaction keep the mode of the thread's latest run.
	Sandbox SandboxMode
	// Mode is ModeBuild (default) or ModePlan (read-only: no tool may change
	// anything). It is kept in the run record, so a worker process honours it.
	Mode RunMode
	// Attachments are images sent with the message (at most MaxAttachments of
	// MaxAttachmentBytes each); they are stored in the thread's workspace.
	Attachments []Attachment
}

// SandboxMode is RunOptions.Sandbox.
type SandboxMode string

const (
	// SandboxAuto (the default): the run has the sandbox tools when a backend
	// is configured and the Sandbox feature is installed; the sandbox is only
	// created when a tool first needs it. Without a backend the run simply has
	// no sandbox tools.
	SandboxAuto SandboxMode = "auto"
	// SandboxOff: the run has no sandbox tools, gets the no-sandbox prompt and
	// never creates (or uses) a sandbox, whatever the harness is configured with.
	SandboxOff SandboxMode = "off"
	// SandboxOn: like SandboxAuto, but Send fails with ErrInvalidRequest when
	// the harness cannot provide a sandbox (no backend, or no Sandbox feature).
	SandboxOn SandboxMode = "on"
)

// ErrNoSandbox is returned (wrapped in ErrInvalidRequest) by Send when the run
// asks for SandboxOn and the harness has no sandbox.
var ErrNoSandbox = errors.New("this run requires a sandbox but the harness has none: set Options.Backend or SANDBOX_BACKEND")

// ErrInvalidRequest wraps the errors caused by the caller's input.
var ErrInvalidRequest = errors.New("invalid request")

// NewThread creates a conversation owned by user.
func (h *Harness) NewThread(ctx context.Context, user, title string) (*store.Thread, error) {
	return h.Store.CreateThread(ctx, user, title)
}

// Send adds a user message to the thread and queues a run answering it. The
// run is executed by Run; follow it with Events.
func (h *Harness) Send(ctx context.Context, user, threadID, text string, o RunOptions) (*store.Run, error) {
	if err := h.ownsThread(ctx, user, threadID); err != nil {
		return nil, err
	}
	if o.Model == "" {
		return nil, fmt.Errorf("%w: model is required", ErrInvalidRequest)
	}
	if !h.cfg.ModelAllowed(o.Model) {
		return nil, fmt.Errorf("%w: model %q is not allowed", ErrInvalidRequest, o.Model)
	}
	sbMode, err := h.sandboxMode(o.Sandbox)
	if err != nil {
		return nil, err
	}
	runMd, err := runMode(o.Mode)
	if err != nil {
		return nil, err
	}
	if err := checkAttachments(o.Attachments); err != nil {
		return nil, err
	}
	meta, err := h.storeAttachments(ctx, user, threadID, o.Attachments)
	if err != nil {
		return nil, err
	}
	mode := ""
	if o.Interactive {
		mode = "interactive"
	}
	run, _, err := h.Store.CreateRun(ctx, store.NewRun{
		RunID: uuid.NewString(), ThreadID: threadID, UserLogin: user, Model: o.Model,
		Message:         store.Message{ID: o.MessageID, Role: store.RoleUser, Content: text, Metadata: meta},
		Mode:            runMd,
		AutoTitle:       autoTitle(text),
		Skills:          o.Skills,
		ReasoningEffort: o.ReasoningEffort,
		InteractionMode: mode,
		Sandbox:         sbMode,
	})
	if err != nil {
		return nil, err
	}
	if err := h.Queue.Enqueue(ctx, run.ID); err != nil {
		return nil, err
	}
	return run, nil
}

// HasSandbox reports whether runs can use a sandbox: a backend is configured
// and the Sandbox feature is installed.
func (h *Harness) HasSandbox() bool { return h.sandboxTools }

// sandboxMode validates a run's sandbox mode and returns the stored value.
func (h *Harness) sandboxMode(m SandboxMode) (string, error) {
	switch m {
	case "", SandboxAuto:
		return store.SandboxAuto, nil
	case SandboxOff:
		return store.SandboxOff, nil
	case SandboxOn:
		if !h.HasSandbox() {
			return "", fmt.Errorf("%w: %w", ErrInvalidRequest, ErrNoSandbox)
		}
		return store.SandboxOn, nil
	}
	return "", fmt.Errorf("%w: sandbox must be auto, off or on, not %q", ErrInvalidRequest, m)
}

// Events streams the run's AG-UI events with seq > after, then follows it live
// until RUN_FINISHED or RUN_ERROR. Each event carries its seq (resume point).
//
// Events takes no user: it trusts the caller to have established that the run belongs to the requesting
// user (Store.GetUserRun), as hosts/client and hosts/web do. A host that exposes run ids to end users
// without going through them must make that check itself.
func (h *Harness) Events(ctx context.Context, runID string, after int64) <-chan store.Event {
	return h.Log.Replay(ctx, runID, after)
}

// Cancel stops a run: a queued one is cancelled at once, a running one gets a
// cancel signal and ends with a "cancelled" RUN_FINISHED.
func (h *Harness) Cancel(ctx context.Context, user, runID string) error {
	run, err := h.Store.GetUserRun(ctx, user, runID)
	if err != nil {
		return err
	}
	if store.IsTerminal(run.Status) {
		return nil
	}
	if run.Status == store.StatusQueued {
		if ok, err := h.Store.CancelQueuedRun(ctx, run.ID); err != nil {
			return err
		} else if ok {
			w := h.Log.Writer(run.ID, run.LastSeq)
			_, _ = w.Append(ctx, agui.RunStartedEvent(run.ThreadID, run.ID))
			_, _ = w.Append(ctx, agui.RunFinishedEvent(run.ThreadID, run.ID, "cancelled", nil))
			return w.Close(ctx)
		}
	}
	if _, err := h.Store.RequestCancel(ctx, run.ID); err != nil {
		return err
	}
	return h.Bus.PublishCancel(ctx, run.ID)
}

// Steer adds a message to a running run: the model sees it at its next step.
func (h *Harness) Steer(ctx context.Context, user, runID, text string) error {
	run, err := h.Store.GetUserRun(ctx, user, runID)
	if err != nil {
		return err
	}
	if store.IsTerminal(run.Status) {
		return queue.ErrSteersClosed
	}
	return h.Bus.PushSteer(ctx, run.ID, queue.Steer{ID: uuid.NewString(), Text: text})
}

func autoTitle(text string) string {
	const max = 60
	r := []rune(text)
	if len(r) > max {
		r = append(r[:max], '…')
	}
	return string(r)
}

// agentOptions is the agent's configuration without features: compaction,
// reasoning, vision and the interactive approvals gate.
func agentOptions(cfg *config.Config, st store.Store, mgr sandbox.Manager) agent.Harness {
	return agent.Harness{
		PromptVariant:         cfg.PromptVariant,
		Summaries:             st,
		Plans:                 st,
		Compactions:           st,
		ContextWindow:         cfg.ContextWindow,
		CompactionThreshold:   cfg.CompactionThreshold,
		OutputReserve:         cfg.OutputReserve,
		KeepRecentToolResults: agent.MaskSetting(cfg.KeepRecentToolResults),
		MaskFromFraction:      agent.MaskFromSetting(cfg.MaskFromFraction),
		CompactionModel:       cfg.CompactionModel,
		SupportsReasoning:     cfg.SupportsReasoning,
		ReasoningEffort:       cfg.EffortFor,
		ReasoningPassback:     cfg.ReasoningPassback,
		PromptCacheKey:        cfg.PromptCacheKey,
		Vision:                cfg.SupportsVision,
		BeforeTool:            []agent.BeforeToolHook{&approvals.Gate{Store: st, RiskyToolPattern: cfg.RiskyToolPattern, Resolver: gateResolver(mgr)}},
		Approvals:             st,
	}
}

func gateResolver(mgr sandbox.Manager) approvals.PathResolver {
	if mgr == nil || !mgr.Enabled() {
		return nil
	}
	return approvals.SandboxResolver{Sandboxes: mgr}
}

// ownsThread returns store.ErrNotFound unless the thread exists and belongs to user. A thread of another user
// is indistinguishable from a missing one, and the check comes before any other, so the answer never reveals
// whether the thread is busy.
func (h *Harness) ownsThread(ctx context.Context, user, threadID string) error {
	_, err := h.Store.GetThread(ctx, user, threadID)
	return err
}
