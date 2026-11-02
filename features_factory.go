package harness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/features/factory/budget"
	"github.com/7ar-chat/harness/features/factory/delegate"
	"github.com/7ar-chat/harness/features/factory/forge"
	"github.com/7ar-chat/harness/features/factory/lessons"
	factorypolicy "github.com/7ar-chat/harness/features/factory/policy"
	"github.com/7ar-chat/harness/features/factory/review"
	"github.com/7ar-chat/harness/features/factory/tasks"
	"github.com/7ar-chat/harness/features/factory/triggers"
	"github.com/7ar-chat/harness/modelinfo"
)

// FeatureFactory is the name of the Factory feature.
const FeatureFactory = "factory"

// FactoryOptions configures the Factory feature.
type FactoryOptions struct {
	// Store holds tasks, triggers and thread links (factorystore.NewMem or NewPG). Required.
	Store factory.Store

	// Forge is the code host. Without one the factory runs tasks that need no
	// pull request (an answer is the result); with one it also opens, watches
	// and merges pull requests.
	Forge factory.Forge
	// Auth mints repository tokens for cloning and pushing and for the forge client (required with Forge).
	Auth factory.RepoAuth
	// ReviewModel is the model of the reviewer that gates every merge. Set, it
	// must differ from the model that wrote the change (the gate refuses to approve
	// its own author's work). Empty, the default model reviews: weaker, because a
	// model grades its own work, and every approval says so (review.SelfReviewNote).
	ReviewModel string
	// NoReview merges on green CI alone. It is a deliberate opt-out.
	NoReview bool
	// Protected are path globs no automated change may touch (review.DefaultProtected when nil).
	Protected []string
	// RequireTests flags diffs that change source without a test.
	RequireTests bool
	// ReviewRubric is appended to the reviewer's default rubric.
	ReviewRubric string
	// MergeMethod is how pull requests merge (default squash).
	MergeMethod factory.MergeMethod
	// AllowNoChecks lets a repository without CI merge.
	AllowNoChecks bool
	// LoopEvery is how often the pull request loop polls (default 30s); Settle is how long checks must stay
	// green before the PR is judged (default 60s).
	LoopEvery, Settle time.Duration

	// Price returns model prices for the budgets (nil: tokens are counted, dollars are not).
	Price func(model string) (modelinfo.Info, bool)
	// DailyBudget caps all factory spend per Window (zero: no cap); PerProject applies it to each project.
	DailyBudget factory.Budget
	PerProject  bool
	// DefaultBudget is the budget of tasks that name none.
	DefaultBudget factory.Budget

	// DefaultPolicy is the autonomy policy of tasks that name none (default factory.PolicySandboxed).
	DefaultPolicy string
	// MaxConcurrent bounds the tasks running at once (default 4).
	MaxConcurrent int
	// MaxAttempts is the default attempts per task (default 3).
	MaxAttempts int
	// Backoff is the delay before a failed attempt is retried (default 30s doubling, capped at 15m);
	// PollEvery is how often the dispatcher looks for ready tasks (default 5s).
	Backoff   func(attempt int) time.Duration
	PollEvery time.Duration

	// Lessons distils what finished tasks taught into the project's memory, so
	// later runs recall it. LessonsModel (default: the default model) writes them.
	// Needs the Memory feature, installed earlier in the list.
	Lessons      bool
	LessonsModel string

	// OnSpawn is told of every thread the factory creates for a task (the task's run and its delegated
	// sub-agents), before its first run is queued. A team host links the thread to the task's project here
	// so the project's members can watch the agent work. An error is logged, never fatal.
	OnSpawn func(ctx context.Context, threadID string, t factory.Task, req factory.SpawnRequest)

	// Escalator hears of tasks that need a human (nil: the log only).
	Escalator factory.Escalator
	// Observers hear of finished tasks (the lessons distiller is one).
	Observers []factory.Observer
	// WebhookSecret signs GitHub deliveries; empty rejects every delivery.
	WebhookSecret []byte
	// Roles overrides the role catalogue (nil: delegate.DefaultRoles).
	Roles map[string]delegate.Role

	// Run says whether this process runs the dispatcher, the scheduler and the
	// pull request loop. They are safe to run in several processes over a
	// shared Store; a web-only process may leave them off.
	Run bool

	// Policy adds the autonomy policy engine: every tool call of a task's thread is classified and allowed,
	// asked about (supervised) or denied (force-push, secrets, protected paths, merges...) by the task's
	// policy. Ordinary chat threads are untouched. PolicyConfig adds project rules; PolicyProtectedBranches
	// supplies the repository's protected branches.
	Policy                  bool
	PolicyConfig            factorypolicy.Config
	PolicyProtectedBranches func(ctx context.Context, repo string) []string

	// ExtraBeforeTool are policy hooks installed ahead of the budget guard.
	ExtraBeforeTool []agent.BeforeToolHook
}

// Factory is the running agent factory, published by the Factory feature
// (harness.Lookup / Builder.Factory). Hosts mount Webhook, call Tasks to
// create and inspect tasks, and Triggers to schedule work.
type Factory struct {
	Store    factory.Store
	Tasks    *tasks.Service
	Webhook  *triggers.Webhook
	Triggers *triggers.Scheduler
	Loop     *forge.Loop // nil without a Forge
	Budget   *budget.Guard
	Spawner  *FactorySpawner
	Roles    map[string]delegate.Role
}

// Report is the project's factory spend since a time, for dashboards.
func (f *Factory) Report(ctx context.Context, projectID string, since time.Time) (budget.Report, error) {
	return budget.Summary(ctx, f.Store, projectID, since)
}

// WebhookHandler is the GitHub webhook endpoint.
func (f *Factory) WebhookHandler() http.Handler { return f.Webhook }

// Factory returns the Factory service published by the feature (nil when it is not installed).
func (b *Builder) Factory() *Factory {
	f, _ := Lookup[*Factory](b)
	return f
}

// FactoryOf returns the Factory service of a built harness (nil without the feature).
func (h *Harness) FactoryOf() *Factory { return h.factory }

// FactoryFeature installs the agent factory: tasks that agents work on
// unattended, fed by cron and GitHub webhooks, delegated to role agents,
// delivered as pull requests that CI and an independent reviewer gate, under
// per-task budgets, loop guards and autonomy policies.
func FactoryFeature(o FactoryOptions) Feature {
	return builtin{FeatureFactory, func(b *Builder) error {
		if o.Store == nil {
			return errors.New("factory: Options.Store is required")
		}
		if o.Forge != nil {
			if o.Auth == nil {
				return errors.New("factory: a Forge needs Auth to clone and push")
			}
			if err := b.RequireSandbox(); err != nil {
				return fmt.Errorf("factory: pull requests need a sandbox: %w", err)
			}
		}
		cfg := b.Config
		roles := o.Roles
		if roles == nil {
			roles = delegate.DefaultRoles()
		}
		defaultPolicy := o.DefaultPolicy
		if defaultPolicy == "" {
			defaultPolicy = factory.PolicySandboxed
		}
		spawner := &FactorySpawner{Store: o.Store, DefaultModel: cfg.LLMDefaultModel, Price: o.Price, OnSpawn: o.OnSpawn}
		st := b.Store

		observers := append([]factory.Observer(nil), o.Observers...)
		if o.Lessons {
			if err := b.Require(FeatureMemory); err != nil {
				return fmt.Errorf("factory: Lessons needs the memory feature: %w", err)
			}
			model := o.LessonsModel
			if model == "" {
				model = cfg.LLMDefaultModel
			}
			observers = append(observers, lessons.New(lessons.Options{
				Models: b.Models, Model: model, Memory: st, Store: o.Store, SupportsReasoning: cfg.SupportsReasoning,
			}))
		}

		// The guard needs the service as its controller and the service needs the guard as its gate: the gate
		// reads the guard through this variable, which is set right after both exist.
		var guard *budget.Guard
		// A task that ended for good has no further use for its sandbox.
		observers = append(observers, sandboxReleaser{h: func() *Harness { return spawner.H }, st: o.Store})

		svc := tasks.New(tasks.Options{
			Store: o.Store, Spawner: spawner,
			Admit: func(ctx context.Context, taskID string) (bool, string) { return guard.Check(ctx, taskID) },
			Messages: func(ctx context.Context, threadID string) (string, error) {
				msgs, err := st.ListMessages(ctx, threadID)
				if err != nil {
					return "", err
				}
				return lastAssistantText(msgs), nil
			},
			Models: cfg.LLMModels, DefaultModel: cfg.LLMDefaultModel,
			Escalator: o.Escalator, Observers: observers,
			Roles: func(role string) (string, bool, bool) {
				r, ok := roles[role]
				return r.Instructions, r.ReadOnly, ok
			},
			DefaultPolicy: defaultPolicy, DefaultMaxAttempts: o.MaxAttempts, DefaultBudget: o.DefaultBudget,
			MaxConcurrent: o.MaxConcurrent, Backoff: o.Backoff, PollEvery: o.PollEvery,
			CancelRun: func(ctx context.Context, runID string) error {
				run, err := st.GetRun(ctx, runID)
				if err != nil {
					return err
				}
				return spawner.H.Cancel(ctx, run.UserLogin, runID)
			},
			RunStatus: func(ctx context.Context, runID string) (string, error) {
				run, err := st.GetRun(ctx, runID)
				if errors.Is(err, store.ErrNotFound) {
					return "", nil
				}
				if err != nil {
					return "", err
				}
				return run.Status, nil
			},
			RunError: func(ctx context.Context, runID string) (string, error) {
				run, err := st.GetRun(ctx, runID)
				if err != nil {
					return "", err
				}
				return run.ErrorCode + " " + run.Error, nil
			},
		})

		guard = budget.New(budget.Options{
			Store: o.Store, Controller: svc, Price: o.Price,
			Daily: o.DailyBudget, PerProject: o.PerProject, Escalator: o.Escalator,
			Cancel: func(ctx context.Context, runID string) error {
				run, err := st.GetRun(ctx, runID)
				if err != nil {
					return err
				}
				return spawner.H.Cancel(ctx, run.UserLogin, runID)
			},
		})

		f := &Factory{Store: o.Store, Tasks: svc, Budget: guard, Spawner: spawner, Roles: roles}

		// Order matters. The budget hook meters and may block a task before the
		// tasks hook decides what the run's end means for it.
		b.EndOfRun(guard.Hook())
		b.EndOfRun(svc.Hook())
		b.Job(svc.Job())
		if o.Policy {
			// The policy must see a call first, so its denial wins over every other hook. Its approval gate is
			// the engine's own (the first BeforeTool hook, installed before any feature), which the supervised
			// policy asks through; the engine exists only when Configure runs, so the policy is built there.
			b.Configure(func(hs *agent.Harness) {
				var inner agent.BeforeToolHook
				if len(hs.BeforeTool) > 0 {
					inner = hs.BeforeTool[0]
				}
				pe := factorypolicy.New(factorypolicy.Options{
					Store: o.Store, Protected: o.Protected, ProtectedBranches: o.PolicyProtectedBranches,
					DefaultPolicy: defaultPolicy, Config: o.PolicyConfig, Inner: inner,
					Escalate: func(ctx context.Context, taskID, reason string) {
						t, err := o.Store.GetTask(ctx, taskID)
						if err != nil {
							logFactory("escalating a policy violation failed", taskID, err)
							return
						}
						if o.Escalator != nil {
							o.Escalator.Escalate(ctx, *t, reason)
						}
					},
				})
				hs.BeforeTool = append([]agent.BeforeToolHook{pe}, hs.BeforeTool...)
			})
		}
		for _, h := range o.ExtraBeforeTool {
			b.BeforeTool(h)
		}
		b.BeforeTool(guard.BeforeTool())
		b.ToolHook(guard.ToolHook())
		b.StepNote(guard.StepNote())

		b.AddTools(delegate.New(delegate.Options{
			Spawner: spawner, Store: o.Store, Roles: roles, Models: cfg.LLMModels,
			Task: func(ctx context.Context, id string) (*factory.Task, error) { return o.Store.GetTask(ctx, id) },
			OnSpend: func(ctx context.Context, taskID string, d factory.Spend) {
				if _, err := o.Store.AddSpend(ctx, taskID, d); err != nil && !errors.Is(err, factory.ErrNotFound) {
					logFactory("recording delegated spend failed", taskID, err)
				}
			},
		}))
		b.Section(delegate.Section(o.Store, roles))
		b.Section(delegate.Roster(roles))

		var onEvent func(context.Context, factory.Event)
		if o.Forge != nil {
			var gate factory.Gate
			if !o.NoReview {
				reviewer := o.ReviewModel
				if reviewer == "" {
					reviewer = cfg.LLMDefaultModel
				}
				gate = review.New(review.Options{
					Forge: o.Forge, Models: b.Models, Model: reviewer, RequireDifferentModel: o.ReviewModel != "",
					SupportsReasoning: cfg.SupportsReasoning, Protected: o.Protected,
					RequireTests: o.RequireTests, Rubric: o.ReviewRubric,
				})
			}
			f.Loop = forge.NewLoop(forge.LoopOptions{
				Forge: o.Forge, Store: o.Store, Controller: svc, Gate: gate,
				MergeMethod: o.MergeMethod, AllowNoChecks: o.AllowNoChecks, Every: o.LoopEvery, Settle: o.Settle,
			})
			onEvent = f.Loop.OnEvent
			checkout := &forge.Checkout{Auth: o.Auth, Store: o.Store}
			// The agent holds no push credential: open_pr asks the host to push the task's branch.
			b.AddTools(forge.NewTools(forge.ToolsOptions{Forge: o.Forge, Store: o.Store, Controller: svc,
				Push: func(ctx context.Context, threadID string, protected []string) (forge.PushResult, error) {
					sb, err := b.Sandboxes.Get(ctx, threadID)
					if errors.Is(err, sandbox.ErrNoSandbox) {
						// A sandbox exists once the agent ran its first command; open_pr never creates one.
						return forge.PushResult{}, errors.New("nothing to push: you have not run any command in the repository yet; make and commit your change first")
					}
					if err != nil {
						return forge.PushResult{}, fmt.Errorf("reaching the thread's sandbox: %w", err)
					}
					return checkout.Push(ctx, threadID, sb, protected...)
				}}))
			b.OnSandboxCreate(func(ctx context.Context, threadID string, sb sandbox.Sandbox, _ bool) string {
				return checkout.Setup(ctx, threadID, sb)
			})
			b.Section(checkout.Section())
		}

		f.Webhook = triggers.NewWebhook(triggers.Options{Store: o.Store, Controller: svc, Secret: o.WebhookSecret, OnEvent: onEvent})
		f.Triggers = triggers.NewScheduler(triggers.Options{Store: o.Store, Controller: svc})

		b.OnReady(func(h *Harness) {
			spawner.H = h
			h.factory = f
		})
		if o.Run {
			b.Background(func(ctx context.Context) { _ = svc.Run(ctx) })
			b.Background(func(ctx context.Context) { _ = f.Triggers.Run(ctx) })
			if f.Loop != nil {
				b.Background(f.Loop.Run)
			}
		}
		return Provide(b, f)
	}}
}

func logFactory(msg, taskID string, err error) {
	slog.Warn("factory: "+msg, "task_id", taskID, "error", err)
}

// sandboxReleaser frees the sandbox of a task's thread when the task is done, failed or cancelled. A task
// blocked for a human keeps its sandbox: a human answer resumes the same attempt there.
type sandboxReleaser struct {
	h  func() *Harness
	st factory.Store
}

// TaskFinished implements factory.Observer.
func (r sandboxReleaser) TaskFinished(ctx context.Context, t factory.Task, _ []factory.TaskEvent) {
	h := r.h()
	if h == nil || !t.State.Terminal() {
		return
	}
	ids := map[string]bool{}
	if t.ThreadID != "" {
		ids[t.ThreadID] = true
	}
	// Every attempt had a thread of its own; their sandboxes would otherwise wait to be expired.
	if evs, err := r.st.ListEvents(ctx, t.ID, 0); err == nil {
		for _, e := range evs {
			if id, _ := e.Data["threadId"].(string); id != "" && e.Kind == factory.EventRun {
				ids[id] = true
			}
		}
	}
	for id := range ids {
		h.ReleaseSandbox(ctx, id)
	}
}
