// Package harness is the embeddable agent harness behind Studio's Work Chat:
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
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/GhaziBenDahmane/harness/agent"
	"github.com/GhaziBenDahmane/harness/agent/prompts"
	"github.com/GhaziBenDahmane/harness/agui"
	"github.com/GhaziBenDahmane/harness/approvals"
	"github.com/GhaziBenDahmane/harness/artifacts"
	"github.com/GhaziBenDahmane/harness/codecheck"
	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/eventlog"
	"github.com/GhaziBenDahmane/harness/goals"
	"github.com/GhaziBenDahmane/harness/llm"
	"github.com/GhaziBenDahmane/harness/memory"
	"github.com/GhaziBenDahmane/harness/projectdocs"
	"github.com/GhaziBenDahmane/harness/queue"
	"github.com/GhaziBenDahmane/harness/runtime"
	"github.com/GhaziBenDahmane/harness/sandbox"
	"github.com/GhaziBenDahmane/harness/skills"
	"github.com/GhaziBenDahmane/harness/skills/skillsfs"
	"github.com/GhaziBenDahmane/harness/store"
	"github.com/GhaziBenDahmane/harness/store/memstore"
	"github.com/GhaziBenDahmane/harness/titles"
	"github.com/GhaziBenDahmane/harness/tools"
	"github.com/GhaziBenDahmane/harness/tools/exploretools"
	"github.com/GhaziBenDahmane/harness/tools/harnesstools"
	"github.com/GhaziBenDahmane/harness/tools/mcptools"
	"github.com/GhaziBenDahmane/harness/tools/memorytools"
	"github.com/GhaziBenDahmane/harness/tools/sandboxtools"
	"github.com/GhaziBenDahmane/harness/tools/skilltools"
	"github.com/GhaziBenDahmane/harness/verify"
	"github.com/GhaziBenDahmane/harness/workspace"
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
	// provider or the Studio LLM gateway).
	Models llm.Factory
	// Backend provides the sandboxes (default: the local machine when SANDBOX_BACKEND=local,
	// OpenSandbox when OPENSANDBOX_URL is set, else none: the agent then has no sandbox tools).
	Backend sandbox.Backend
	// Blobs stores artifact contents (default: in the Store).
	Blobs artifacts.Blobs
	// Redis, when set, shares the event log, run queue, sandbox locks and skill
	// installs between processes. Without it the harness is single-process.
	Redis redis.UniversalClient
	// BuiltinSkills holds <name>/SKILL.md folders (default: skills/skillsfs).
	BuiltinSkills fs.FS
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
}

// New wires the harness.
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
		if cfg.LocalSandbox() {
			backend, err = localBackend(cfg)
		} else {
			backend, err = sandbox.BackendFromConfig(cfg)
		}
		if err != nil {
			return nil, err
		}
	}
	h := &Harness{cfg: cfg, Store: st}

	// Coordination: Redis between processes, memory inside one.
	var runQueue runtime.RunQueue
	if o.Redis != nil {
		q := queue.New(o.Redis)
		h.Log, h.Queue, h.Bus, runQueue = eventlog.New(o.Redis, st), q, q, q
	} else {
		q := queue.NewMemory()
		h.Log, h.Queue, h.Bus, runQueue = eventlog.NewMemory(st), q, q, q
	}

	var err error
	blobs := o.Blobs
	if blobs == nil {
		blobs = artifacts.PGBlobs{S: st}
	}

	var artSvc *artifacts.Service
	var wsSvc *workspace.Service
	mgr := sandbox.NewManager(backend, st, o.Redis, sandbox.ManagerOptions{
		MaxPerUser: cfg.MaxSandboxesPerUser, Timeout: cfg.SandboxTimeout(),
		UnhealthyRecreate: time.Duration(cfg.SandboxUnhealthyRecreate) * time.Second,
		OnCreate: func(ctx context.Context, threadID string, sb sandbox.Sandbox, recreated bool) string {
			if cfg.LocalSandbox() {
				return "" // the host keeps its files; nothing to restore into the user's project
			}
			wsSvc.OnCreate(ctx, threadID, sb) // the checkpoint history first, then the files
			return artSvc.RestoreWorkspace(ctx, threadID, sb, recreated)
		},
	})
	artSvc = artifacts.New(st, blobs, mgr, artifacts.Options{PreviewConcurrency: cfg.PreviewMaxConcurrency,
		BlobGCGrace: time.Duration(cfg.BlobGCGraceHours) * time.Hour})
	wsSvc = workspace.New(mgr, st, st, artSvc, workspace.Options{Disabled: !cfg.WorkspaceCheckpoints || cfg.LocalSandbox(), ArchiveMaxBytes: cfg.GitArchiveMaxBytes,
		MaxFileBytes: cfg.CheckpointMaxFile})
	builtin := o.BuiltinSkills
	if builtin == nil {
		builtin = skillsfs.Builtin()
	}
	reg := skills.NewRegistry(builtin, st)
	installer := &skills.Installer{Registry: reg, Store: st, Redis: o.Redis, Messages: st}
	h.Sandboxes, h.Artifacts, h.Skills, h.Workspace = mgr, artSvc, reg, wsSvc

	mcpProvider := mcptools.New(cfg.MCPServers, o.Version, mcptools.MaxResultChars)
	if mcpProvider != nil {
		mcpProvider.CallTimeout = cfg.MCPCallTimeout()
	}
	sbOpts := sandboxtools.Options{CommandTimeout: cfg.CommandTimeout(), MaxSandboxesPerUser: cfg.MaxSandboxesPerUser,
		OnAcquire: func(ctx context.Context, rc tools.RunContext, sb sandbox.Sandbox) {
			installer.EnsureRunSkills(ctx, rc, sb)
			wsSvc.OnAcquire(ctx, rc, sb) // "Before AI turn" checkpoint of a sandbox created mid-run
		},
		BeforeCommand:      installer.EnsureCommandSkills,
		OnWrite:            installer.NoteContent,
		AfterFailedCommand: installer.AfterFailedCommand,
	}
	if strings.EqualFold(strings.TrimSpace(cfg.AgentProfile), "coding") || strings.TrimSpace(cfg.CodeChecks) != "" {
		extra, err := codecheck.Parse(cfg.CodeChecks)
		if err != nil {
			return nil, err
		}
		sbOpts.CheckWrite = codecheck.New(extra).Check
		sbOpts.Coding = strings.EqualFold(strings.TrimSpace(cfg.AgentProfile), "coding")
	}
	if o.Redis != nil {
		sbOpts.Jobs = sandboxtools.RedisJobs{RDB: o.Redis}
	}
	sbTools := sandboxtools.New(mgr, sbOpts)
	if sbTools != nil && o.Redis != nil {
		go sbTools.KeepResumingJobs(context.Background(), 2*time.Minute, nil)
	}
	memSvc := memory.New(st, memory.OptionsFromConfig(cfg))
	var explore tools.Provider
	if sbTools != nil && strings.EqualFold(strings.TrimSpace(cfg.AgentProfile), "coding") {
		if m := cmp.Or(cfg.ExploreModel, cfg.LLMDefaultModel); m != "" {
			explore = &exploretools.Provider{Source: sbTools, Models: models, Model: m}
		}
	}
	providers := []tools.Provider{
		nilProvider(sbTools),
		explore,
		&skilltools.Provider{Registry: reg, Installer: installer, Sandbox: mgr},
		nilProvider(mcpProvider),
		harnesstools.New(st),
		memorytools.New(memSvc),
	}
	providers = append(providers, o.Tools...)

	hs := harnessOptions(cfg, st, reg, artSvc, wsSvc, mgr, memSvc)
	if hs, err = wirePermissions(cfg, hs); err != nil { // local/coding only: permission rules + git snapshots
		return nil, err
	}
	if cfg.SandboxEnabled() || backend != nil {
		hs.Spill = tools.SandboxSpiller{Sandboxes: mgr}
	}
	checker := verify.New(verify.Options{Sandbox: mgr, Messages: st, Artifacts: artSvc, EnsureSkill: installer.EnsureSkill})
	hs.Checker, hs.Deliverables = checker, checker

	h.Agent = &agent.Agent{
		Models: models, Tools: newProfile(cfg, reg).Wrap(tools.NewRegistry(providers...)), Store: st, Steers: h.Bus,
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
	hooks := append([]runtime.Hook{
		artifacts.EndOfRunHook{Svc: artSvc},
		workspace.EndOfRunHook{Svc: wsSvc},
		&titles.Hook{Store: st, Models: models, SupportsReasoning: cfg.SupportsReasoning},
		&goals.Hook{Store: st, Models: models, Queue: h.Queue, ListArtifacts: artSvc.ProducedPaths,
			Evidence: checker.Evidence, SupportsReasoning: cfg.SupportsReasoning},
	}, o.Hooks...)
	h.Runtime = runtime.New(runtime.Options{
		ID: id, Concurrency: cfg.RuntimeConcurrency, ShutdownGrace: time.Duration(cfg.RuntimeShutdownGrace) * time.Second,
		Hooks:  hooks,
		Purger: artifacts.PurgeJob{Svc: artSvc, After: time.Duration(cfg.PurgeAfterDays) * 24 * time.Hour},
		Jobs: []runtime.Job{{Name: "memory-consolidation", Every: memory.ConsolidationEvery(cfg),
			Run: memory.Consolidator{Store: st, Retention: memory.DeletedRetention(cfg)}.Run},
			{Name: "approval-expiry", Every: time.Minute, Run: func(ctx context.Context) error {
				_, err := st.ExpireApprovals(ctx, time.Duration(cfg.ApprovalTTLHours)*time.Hour, "")
				return err
			}}},
	}, st, h.Log, runQueue, h.Bus, h.Agent, o.Redis)
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
	// Userinfo is forwarded to the LLM gateway and MCP servers (X-Userinfo, base64 JSON).
	Userinfo string
	// Interactive makes risky tool calls wait for Approve/Reject.
	Interactive bool
	// MessageID makes Send idempotent for that user message (optional).
	MessageID string
}

// ErrInvalidRequest wraps the errors caused by the caller's input.
var ErrInvalidRequest = errors.New("invalid request")

// NewThread creates a conversation owned by user.
func (h *Harness) NewThread(ctx context.Context, user, title string) (*store.Thread, error) {
	return h.Store.CreateThread(ctx, user, title)
}

// Send adds a user message to the thread and queues a run answering it. The
// run is executed by Run; follow it with Events.
func (h *Harness) Send(ctx context.Context, user, threadID, text string, o RunOptions) (*store.Run, error) {
	if o.Model == "" {
		return nil, fmt.Errorf("%w: model is required", ErrInvalidRequest)
	}
	if !h.cfg.ModelAllowed(o.Model) {
		return nil, fmt.Errorf("%w: model %q is not allowed", ErrInvalidRequest, o.Model)
	}
	mode := ""
	if o.Interactive {
		mode = "interactive"
	}
	run, _, err := h.Store.CreateRun(ctx, store.NewRun{
		RunID: uuid.NewString(), ThreadID: threadID, UserLogin: user, Model: o.Model, Userinfo: o.Userinfo,
		Message:         store.Message{ID: o.MessageID, Role: store.RoleUser, Content: text},
		AutoTitle:       autoTitle(text),
		Skills:          o.Skills,
		ReasoningEffort: o.ReasoningEffort,
		InteractionMode: mode,
	})
	if err != nil {
		return nil, err
	}
	if err := h.Queue.Enqueue(ctx, run.ID); err != nil {
		return nil, err
	}
	return run, nil
}

// Events streams the run's AG-UI events with seq > after, then follows it live
// until RUN_FINISHED or RUN_ERROR. Each event carries its seq (resume point).
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

// harnessOptions is the agent's feature configuration (prompt sections, hooks,
// compaction, approvals).
func harnessOptions(cfg *config.Config, st store.Store, reg *skills.Registry, artSvc *artifacts.Service, wsSvc *workspace.Service, mgr sandbox.Manager, memSvc *memory.Service) agent.Harness {
	prof := newProfile(cfg, reg)
	return agent.Harness{
		Sections: []agent.PromptSection{
			prof.skillsSection(reg.PromptSection),
			projectdocs.Section(mgr),
			agent.Legacy(artifacts.PromptSection, ""),
			agent.Part(prompts.Git, agent.Legacy(wsSvc.PromptSection, prompts.GitCheckpoints)),
			agent.Legacy(func(context.Context, agent.Input) string { return harnesstools.Instructions }, ""),
			prof.memorySection(memory.PromptSection(memSvc)),
			goals.PromptSection(st),
		},
		Context:               []agent.PromptSection{goals.ContextSection(st), prof.memorySection(memory.RecallSection(memSvc))},
		Tail:                  []agent.PromptSection{goals.TurnsSection(st)},
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
		ToolHooks:             []agent.ToolHook{artSvc},
		RunStart:              []agent.RunStartHook{wsSvc},
		Answers:               artSvc,
		Images:                artifacts.Images{Svc: artSvc, Vision: cfg.SupportsVision},
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

func nilProvider[T tools.Provider](p T) tools.Provider {
	var zero T
	if any(p) == any(zero) {
		return nil
	}
	return p
}
