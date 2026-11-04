# Features, skills and profiles

## Skills

A skill is a folder with a `SKILL.md` (frontmatter `name` and `description`, then the instructions)
and optional `scripts/`, `reference/` files. The model sees an index of them in its system prompt,
reads one with `load_skill`, and its folder appears read-only under `/skills/<name>/` in the sandbox
(installed lazily, only for the skills a run loads or names in a command).

A skill that needs an account of the user's (a Dokploy server, any HTTP API) can declare it in a `connect:` block; the
server then makes the calls with the user's sealed credentials, which never reach the sandbox or the model. See
[docs/marketplace.md](marketplace.md#skills-with-an-account).

Skills are discovered from, lowest to highest precedence (a later source replaces a skill of the same name):

| Source | Where | Notes |
| --- | --- | --- |
| `builtin` | embedded in the binary (`features/skills/skillsfs/builtin`) | `docx`, `pptx`, `xlsx`, `pdf`, `charts`; `Options.BuiltinSkills` replaces them |
| `extra` | `Options.ExtraSkills` (an `fs.FS`) | the embedding product's own skills |
| `user` | `SKILLS_DIR`, `~/.har/skills`, `~/.agents/skills`, `~/.claude/skills` (in that order) | one folder per skill; the last two only with the local backend |
| `project` | `<repo>/.har/skills`, `<repo>/.agents/skills`, `<repo>/.claude/skills` (in that order) | the host's repository with the local backend; with a remote backend the sandbox's own `/workspace/.har/skills`, `.agents/skills`, `.claude/skills`, read through the sandbox API per thread and merged over the global skills for that thread's runs. Fenced as untrusted text; symbolic links are not followed; at most 64 skills of 500 files / 8 MiB each |

```
~/.har/skills/
  release-notes/
    SKILL.md          # ---\nname: release-notes\ndescription: Draft release notes from merged PRs.\n---\n…
    scripts/collect.py
```

The folder name is the skill name. `RunOptions.Skills: []string{"release-notes"}` attaches a skill to a
run (its `SKILL.md` goes into the system prompt and its files are installed before the first sandbox
command); the built-ins are only listed and installed on demand. An optional `requires: [other-skill]`
frontmatter list installs dependencies with a skill. Packages are validated (entry count, depth, file
and total size, safe paths) when loaded; an invalid folder is logged and skipped.

## Features and profiles

`harness.New` builds a small core (store, models, sandboxes, infrastructure, the agent loop, the runtime) and
installs **features** on top of it. A feature is a set of tools, prompt sections, hooks and jobs that belong
together. A host chooses its set with `Options.Features`; left nil, the profile named by `AGENT_PROFILE` is used.

| Profile | Constructor | Features (in order) |
|---|---|---|
| work (default) | `harness.ProfileWork()` | Sandbox, Codecheck(false), Skills, MCP, Plan, ProjectDocs, Artifacts, Workspace, Titles, Verify, Goals, Memory, ToolProfile(false), Permissions(false) |
| coding | `harness.ProfileCoding()` | the same with Codecheck(true), Explore, ToolProfile(true), Permissions(true) |
| minimal | `harness.ProfileMinimal()` | Sandbox, Plan |

`AGENT_PROFILE=work|coding|minimal` picks the profile when `Options.Features` is nil; the tool list and prompt
of work and coding are byte-identical to what they were before features existed. Features install in list
order, and that order is the order of tools and prompt sections. A feature that builds on another says so
(`Workspace` needs `Artifacts`, `Explore` and `Codecheck` need `Sandbox`) and `New` fails with a clear error
if it is missing or listed twice.

Compose your own set from the constructors (`Sandbox()`, `Skills()`, `Memory()`, `Goals()`...), drop what you
do not want, add your own:

```go
features := append(harness.ProfileMinimal(), harness.Memory(), harness.Titles(), myAudit)
h, err := harness.New(harness.Options{Config: cfg, Features: features})
```

### Writing a feature

```go
type Feature interface {
	Name() string
	Install(*harness.Builder) error
}
```

`harness.FeatureFunc(name, func(b *harness.Builder) error)` turns a function into one. `Install` reads the
environment from the builder (`b.Config`, `b.Store`, `b.Models`, `b.Sandboxes`, `b.Infra`, `b.Options`) and
contributes:

| Builder method | Contributes |
|---|---|
| `AddTools(providers...)`, `WrapTools(f)` | tool providers; a wrapper over the whole registry (filters, auditing) |
| `Section(f)`, `ContextSection(f)`, `TailSection(f)` | static prompt section; dynamic section in the final context block; per-run reminder at the end of each request |
| `BeforeTool(h)`, `ToolHook(h)`, `RunStart(h)`, `StepNote(f)` | gate tool calls; observe tool results; run before a run's first step; add a `<system-reminder>` note to the next model call when news appears mid-run (not persisted) |
| `SetAnswers`, `SetImages`, `SetChecker`, `Configure(f)` | final-answer review, image attachment, deliverable checker (one each); a free-form mutator of `agent.Harness` |
| `EndOfRun(h)`, `Job(j)`, `SetPurger(p)`, `Background(f)` | end-of-run `runtime.Hook`; periodic job (run by one runtime at a time); retention job; goroutine started once `New` succeeded |
| `SandboxTools(f)`, `PrepareSandbox(f)`, `OnSandboxCreate(f)` | chain onto the sandbox tools' options (`OnAcquire`, `BeforeCommand`, `CheckWrite`...); run when a sandbox is created |
| `RequireSandbox()`, `Require(names...)`, `StoreAs[T](b)` | fail with a clear error when no backend is configured, a feature is missing, or the store lacks an extension |
| `Provide[T](b, v)`, `Lookup[T](b)`, `b.Has(name)` | publish a service for later features; look one up |
| `After(f)` | run once every feature is installed |

The built-in features publish their services on the builder (`b.Artifacts`, `b.Workspace`, `b.Skills`,
`b.Installer`, `b.Memory`, `b.Verifier`; nil until installed). The wiring of the built-ins lives in
`features_core.go` and `features_state.go`; they use the same `Builder` a host's feature does. Example, a tool
plus a prompt section plus a hook (this is `TestACustomFeatureAddsAToolAPromptSectionAndAHook`):

```go
ping := harness.FeatureFunc("ping", func(b *harness.Builder) error {
	b.AddTools(longtest.ExtraTool("ping", func(context.Context, string) (string, error) { return "pong", nil }))
	b.Section(func(context.Context, agent.Input) string { return "Use ping to check the line." })
	b.ToolHook(auditHook) // agent.ToolHook: AfterTool(ctx, run, toolCallID, toolName)
	return nil
})
h, err := harness.New(harness.Options{Config: cfg, Features: append(harness.ProfileMinimal(), ping)})
```

## Agent factory

`harness.FactoryFeature` turns the harness into a factory that takes work from GitHub webhooks and cron, runs agents on
it unattended, delivers pull requests, and merges them only after CI and an independent reviewer model agree, under
per-task budgets, loop guards and autonomy policies. `harness-web` enables it with `FACTORY=1`. See
[docs/factory.md](factory.md).

## Sandbox per run

The sandbox is optional for each run: `RunOptions.Sandbox` (JSON `sandbox` on `POST /api/threads/{id}/runs`) is
`auto` (default: the sandbox tools are offered when a backend is configured and the sandbox is created at the
first command), `off` (no sandbox tools, the no-sandbox prompt, `Backend.Create` is never called) or `on`
(like `auto`, but `Send` fails with `ErrInvalidRequest` when the harness has no sandbox). The choice is kept in
the run record and inherited by the thread's resumed runs. `SANDBOX_BACKEND` names the backend: `local`, `opensandbox`,
`none` or one registered with `backends.Register`. See [docs/embedding.md](embedding.md).

