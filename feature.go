package harness

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/cloudwego/eino/components/tool"

	"github.com/GhaziBenDahmane/harness/coding/sandboxtools"
	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/core/sandbox"
	"github.com/GhaziBenDahmane/harness/engine/agent"
	"github.com/GhaziBenDahmane/harness/engine/llm"
	"github.com/GhaziBenDahmane/harness/engine/runtime"
	"github.com/GhaziBenDahmane/harness/engine/tools"
	"github.com/GhaziBenDahmane/harness/features/artifacts"
	"github.com/GhaziBenDahmane/harness/features/artifacts/workspace"
	"github.com/GhaziBenDahmane/harness/features/memory"
	"github.com/GhaziBenDahmane/harness/features/skills"
	"github.com/GhaziBenDahmane/harness/features/verify"
)

// A Feature is an optional capability of the harness: a set of tools, prompt
// sections, hooks and background jobs that belong together (permissions,
// memory, skills, goals...). A host picks the features it wants with
// Options.Features (or a named profile: ProfileCoding, ProfileWork,
// ProfileMinimal) and may add its own.
//
// Install is called once, in the order of Options.Features, while the harness
// is being built. It reads the environment from the Builder (configuration,
// store, models, sandboxes, infrastructure), contributes through the Builder's
// methods, and returns an error to abort harness.New: an invalid
// configuration, or a missing capability (see Builder.RequireSandbox,
// Builder.Require and StoreAs).
type Feature interface {
	// Name identifies the feature: unique in a harness, used in errors and by
	// Builder.Has / Builder.Require.
	Name() string
	Install(*Builder) error
}

// FeatureFunc adapts a function to a Feature.
func FeatureFunc(name string, install func(*Builder) error) Feature {
	return funcFeature{name: name, install: install}
}

type funcFeature struct {
	name    string
	install func(*Builder) error
}

func (f funcFeature) Name() string             { return f.name }
func (f funcFeature) Install(b *Builder) error { return f.install(b) }

// Builder is what a Feature sees while the harness is built: the environment
// to read, and the contributions it may make. A feature that builds on another
// finds that feature's services in the exported fields (nil when its feature
// is not installed) or through Lookup.
type Builder struct {
	// Config is the environment-driven configuration.
	Config *config.Config
	// Store is the persistence (Options.Store, default: in memory).
	Store Store
	// Models builds the chat models.
	Models llm.Factory
	// Sandboxes manages the sandboxes; Sandboxes.Enabled() reports whether a
	// backend is configured.
	Sandboxes sandbox.Manager
	// Blobs stores artifact contents.
	Blobs artifacts.Blobs
	// Infra is the infrastructure shared between processes (queue, event log...).
	Infra Infra
	// Options are the options the host passed to New (read only).
	Options Options

	// Services of the built-in features, set by the feature that provides them
	// and nil until it is installed (so install it earlier in the list).
	Artifacts *artifacts.Service
	Workspace *workspace.Service
	Skills    *skills.Registry
	Installer *skills.Installer
	Memory    *memory.Service
	Verifier  *verify.Verifier

	installed map[string]bool
	services  map[reflect.Type]any
	prof      *profile
	current   string // the feature being installed

	providers []tools.Provider
	wrappers  []func(tools.Provider) tools.Provider
	sections  []agent.PromptSection
	context   []agent.PromptSection
	tail      []agent.PromptSection
	before    []agent.BeforeToolHook
	toolHooks []agent.ToolHook
	runStart  []agent.RunStartHook
	answers   agent.AnswerChecker
	images    agent.ImageAttacher
	checker   agent.Checker
	deliver   agent.DeliverableSource
	mutators  []func(*agent.Harness)
	endOfRun  []runtime.Hook
	jobs      []runtime.Job
	purger    runtime.Purger
	sbOpts    []func(*sandboxtools.Options)
	prepare   []sandboxCreateHook
	created   []sandboxCreateHook
	post      []func() error
	starts    []func(ctx context.Context)

	sandboxTools *lateProvider // the sandbox tools, nil without a sandbox
}

// sandboxCreateHook runs when a sandbox is created; the text it returns is
// added to the notice the model sees.
type sandboxCreateHook func(ctx context.Context, threadID string, sb sandbox.Sandbox, recreated bool) string

// --- contributions ---

// AddTools offers tool providers to the agent (nil ones are ignored), in the
// order features are installed, before Options.Tools.
func (b *Builder) AddTools(ps ...tools.Provider) {
	for _, p := range ps {
		if p != nil && !isNilValue(p) {
			b.providers = append(b.providers, p)
		}
	}
}

// WrapTools wraps the whole tool registry (filters, auditing...). Wrappers
// apply in installation order, the first one innermost.
func (b *Builder) WrapTools(w func(tools.Provider) tools.Provider) {
	b.wrappers = append(b.wrappers, w)
}

// Section adds a static system-prompt section (rendered in order).
func (b *Builder) Section(s agent.PromptSection) { b.sections = append(b.sections, s) }

// ContextSection adds a dynamic section, rendered in the prompt's final
// context block (recalled memories, the current goal...).
func (b *Builder) ContextSection(s agent.PromptSection) { b.context = append(b.context, s) }

// TailSection adds a section rendered once per run into the reminder at the
// end of every request (keeps the system prompt a cacheable prefix).
func (b *Builder) TailSection(s agent.PromptSection) { b.tail = append(b.tail, s) }

// BeforeTool adds a hook that gates tool calls before they run.
func (b *Builder) BeforeTool(h agent.BeforeToolHook) { b.before = append(b.before, h) }

// ToolHook adds an observer of tool results.
func (b *Builder) ToolHook(h agent.ToolHook) { b.toolHooks = append(b.toolHooks, h) }

// RunStart adds a hook run before the first step of every run.
func (b *Builder) RunStart(h agent.RunStartHook) { b.runStart = append(b.runStart, h) }

// SetAnswers sets the reviewer of final answers; only one feature may.
func (b *Builder) SetAnswers(a agent.AnswerChecker) error {
	if b.answers != nil {
		return errors.New("an answer checker is already set")
	}
	b.answers = a
	return nil
}

// SetImages sets how attached images reach vision models; only one feature may.
func (b *Builder) SetImages(i agent.ImageAttacher) error {
	if b.images != nil {
		return errors.New("an image attacher is already set")
	}
	b.images = i
	return nil
}

// SetChecker sets the deliverable checker and the source of the deliverables
// restored after a compaction; only one feature may.
func (b *Builder) SetChecker(c agent.Checker, d agent.DeliverableSource) error {
	if b.checker != nil {
		return errors.New("a deliverable checker is already set")
	}
	b.checker, b.deliver = c, d
	return nil
}

// Configure changes the agent's configuration after every feature contributed
// (an escape hatch for what the other methods do not cover). Mutators run in
// installation order.
func (b *Builder) Configure(f func(*agent.Harness)) { b.mutators = append(b.mutators, f) }

// EndOfRun adds a hook run at the end of every run, before Options.Hooks.
func (b *Builder) EndOfRun(h runtime.Hook) { b.endOfRun = append(b.endOfRun, h) }

// Job adds a periodic maintenance job (run by one runtime at a time when the
// Infra has a Coordinator).
func (b *Builder) Job(j runtime.Job) { b.jobs = append(b.jobs, j) }

// SetPurger sets the retention job; only one feature may.
func (b *Builder) SetPurger(p runtime.Purger) error {
	if b.purger != nil {
		return errors.New("a purger is already set")
	}
	b.purger = p
	return nil
}

// Background starts f in a goroutine once the harness is built (New returned
// without error); ctx is never cancelled by the harness, f ends on its own.
func (b *Builder) Background(f func(ctx context.Context)) { b.starts = append(b.starts, f) }

// SandboxTools changes the options of the sandbox tools (execute_command,
// read_file...): hook into the lifecycle of a sandbox or of a command. Each
// feature chains onto what the previous ones set; it applies only when the
// "sandbox" feature is installed and a backend is configured.
func (b *Builder) SandboxTools(f func(*sandboxtools.Options)) { b.sbOpts = append(b.sbOpts, f) }

// PrepareSandbox registers a function run when a sandbox is created, before
// the OnSandboxCreate ones (restoring history before restoring files).
func (b *Builder) PrepareSandbox(f func(ctx context.Context, threadID string, sb sandbox.Sandbox, recreated bool) string) {
	b.prepare = append(b.prepare, f)
}

// OnSandboxCreate registers a function run when a sandbox is created; a
// non-empty result is added to the notice the model sees.
func (b *Builder) OnSandboxCreate(f func(ctx context.Context, threadID string, sb sandbox.Sandbox, recreated bool) string) {
	b.created = append(b.created, f)
}

// After registers f to run once every feature is installed (to build what
// depends on the other features' contributions).
func (b *Builder) After(f func() error) { b.post = append(b.post, f) }

// --- capabilities and services ---

// Has reports whether the named feature is installed (so far).
func (b *Builder) Has(name string) bool { return b.installed[name] }

// Require fails unless the named features are installed (earlier in the list).
func (b *Builder) Require(names ...string) error {
	for _, n := range names {
		if !b.installed[n] {
			return fmt.Errorf("feature %q requires feature %q (install it earlier in Options.Features)", b.current, n)
		}
	}
	return nil
}

// RequireSandbox fails unless a sandbox backend is configured (Options.Backend,
// SANDBOX_BACKEND or OPENSANDBOX_URL).
func (b *Builder) RequireSandbox() error {
	if b.Sandboxes == nil || !b.Sandboxes.Enabled() {
		return fmt.Errorf("feature %q needs a sandbox: set Options.Backend, SANDBOX_BACKEND or OPENSANDBOX_URL", b.current)
	}
	return nil
}

// StoreAs returns the Builder's store as T, or an error naming the feature
// when the store does not provide that capability (a feature that needs a
// store extension declares it this way).
func StoreAs[T any](b *Builder) (T, error) {
	v, ok := any(b.Store).(T)
	if !ok {
		var zero T
		return zero, fmt.Errorf("feature %q needs a store implementing %s", b.current, reflect.TypeFor[T]())
	}
	return v, nil
}

// Provide publishes a service for the features installed after this one
// (looked up by its type with Lookup). One service per type.
func Provide[T any](b *Builder, v T) error {
	t := reflect.TypeFor[T]()
	if _, dup := b.services[t]; dup {
		return fmt.Errorf("feature %q: a %s is already provided", b.current, t)
	}
	if b.services == nil {
		b.services = map[reflect.Type]any{}
	}
	b.services[t] = v
	return nil
}

// Lookup returns the service of type T published by an earlier feature.
func Lookup[T any](b *Builder) (T, bool) {
	v, ok := b.services[reflect.TypeFor[T]()].(T)
	return v, ok
}

// --- assembly ---

func newBuilder(o Options, cfg *config.Config, st Store, models llm.Factory, blobs artifacts.Blobs, in Infra) *Builder {
	return &Builder{Config: cfg, Store: st, Models: models, Blobs: blobs, Infra: in, Options: o,
		installed: map[string]bool{}, services: map[reflect.Type]any{}, prof: &profile{allow: map[string]bool{}, removed: map[string]bool{}}}
}

// install runs the features in order.
func (b *Builder) install(features []Feature) error {
	for _, f := range features {
		if f == nil {
			continue
		}
		name := f.Name()
		if strings.TrimSpace(name) == "" {
			return errors.New("harness: a feature has no name")
		}
		if b.installed[name] {
			return fmt.Errorf("harness: feature %q is listed twice", name)
		}
		b.current = name
		if err := f.Install(b); err != nil {
			return fmt.Errorf("harness: feature %q: %w", name, err)
		}
		b.installed[name] = true
	}
	b.current = ""
	for _, f := range b.post {
		if err := f(); err != nil {
			return fmt.Errorf("harness: %w", err)
		}
	}
	return nil
}

// onCreate is the sandbox manager's OnCreate: the prepare hooks, then the
// create hooks; their notices are joined.
func (b *Builder) onCreate(ctx context.Context, threadID string, sb sandbox.Sandbox, recreated bool) string {
	var notes []string
	for _, hooks := range [][]sandboxCreateHook{b.prepare, b.created} {
		for _, h := range hooks {
			if n := h(ctx, threadID, sb, recreated); n != "" {
				notes = append(notes, n)
			}
		}
	}
	return strings.Join(notes, "\n")
}

// lateProvider is a tool provider whose implementation is set once every
// feature has contributed (the sandbox tools).
type lateProvider struct {
	name string
	p    tools.Provider
}

func (l *lateProvider) Name() string { return l.name }

func (l *lateProvider) Tools(ctx context.Context, rc tools.RunContext) ([]tool.BaseTool, func(), error) {
	if l.p == nil {
		return nil, func() {}, nil
	}
	return l.p.Tools(ctx, rc)
}

// isNilValue reports a typed nil pointer wrapped in an interface.
func isNilValue(v any) bool {
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

// applyTo adds what the features contributed to the agent's configuration.
func (b *Builder) applyTo(hs *agent.Harness) {
	hs.Sections, hs.Context, hs.Tail = b.sections, b.context, b.tail
	hs.ToolHooks, hs.RunStart = b.toolHooks, b.runStart
	hs.Images, hs.Answers = b.images, b.answers
	hs.Checker, hs.Deliverables = b.checker, b.deliver
	hs.BeforeTool = append(hs.BeforeTool, b.before...)
	for _, f := range b.mutators {
		f(hs)
	}
}
