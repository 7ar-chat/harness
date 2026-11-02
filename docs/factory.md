# The agent factory

The factory turns the harness from "an agent you chat with" into "agents that take work and deliver it unattended":
a **task** arrives from a trigger (a GitHub webhook, cron, the API), an agent works on it in its own sandbox and
branch, opens a **pull request**, CI and an **independent reviewer** judge it, and it merges (or waits for a human),
all under **budgets**, **loop guards** and a per-task **autonomy policy**.

```
 GitHub webhook ─┐                       ┌──────────── features/factory ────────────┐
 cron ───────────┼─► tasks ─► dispatch ─►│ run (agent, sandbox, branch factory/<id>)│
 POST /tasks ────┘     ▲                 │   ├─ delegate: planner/coder/reviewer/…  │
                       │ Resume(feedback)│   ├─ policy:   per-call allow/ask/deny   │
                       │                 │   └─ budget:   spend, repeat, tool caps  │
                       │                 └──────────────┬──────────────────────────┘
                       │                                ▼ open_pr
                       │   CI red / changes requested ◄─ PR loop ─► CI green ─► review gate ─► merge | human
                       └────────────────────────────────┘                        (different model)
```

Everything is a `harness.Feature`: `harness.FactoryFeature(harness.FactoryOptions{…})`. `harness-web` installs it
with `FACTORY=1` (below).

## Packages

| Package | Role |
|---|---|
| `core/factory` | the vocabulary and ports: `Task`, `State`, `Trigger`, `Store`, `Controller`, `Forge`, `Gate`, `Spawner`, `ThreadInfo`, `Fingerprint` |
| `adapters/store/factorystore` | `factory.Store` in memory (`NewMem`) and Postgres (`NewPG`, migration `0490_factory.sql`); one conformance suite runs on both |
| `features/factory/tasks` | the task service: dispatcher, retries with backoff, the end-of-run hook, `factory.Controller` |
| `features/factory/triggers` | cron scheduler (5-field cron, `@every`) and the GitHub webhook (HMAC, dedup, event normalisation, templates) |
| `features/factory/delegate` | `delegate` / `delegate_many` tools and the role catalogue (planner, coder, reviewer, tester, explorer) |
| `features/factory/forge` | GitHub REST client, `open_pr` / `pr_status` / `ci_log` / `pr_comments` tools, sandbox checkout, the PR loop |
| `features/factory/review` | the merge gate: mechanical checks (protected paths, secrets, prompt injection) then an independent model |
| `features/factory/budget` | spend metering per task and per day, repeated-failure detection, tool denial when exhausted |
| `features/factory/policy` | the autonomy policy: classifies each tool call and allows, asks, or denies it |
| `features/factory/lessons` | distils finished tasks into project memory so later runs recall what cost an attempt |
| `bench/swebench` | SWE-bench style instances as `bench.Task`s, with hidden tests and a resolved/not-resolved evaluator |

## A task's life

`pending` (waits for `DependsOn`) → `ready` → `running` → `review` (a PR is open) → `done`. A task can be `blocked`
(needs a human, with a reason), `failed` or `cancelled`. Every transition is a conditional update, so several
processes sharing a Postgres never run the same task, and every transition appends a `TaskEvent` (the history).

* A run that ends without a pull request on a task with a repository is resumed with "open a pull request".
* A failed run is retried after a backoff (default 30 s doubling, capped at 15 min) up to `MaxAttempts` (default 3).
  **Failing the same way three times** (same normalised error, see `factory.Fingerprint`) blocks the task at once.
* CI red, a merge conflict, review findings or a human's "changes requested" resume the task with that text as its
  next attempt's feedback, on the same branch, at most `MaxFixRounds` (5) times.
* A green PR must stay green for `Settle` (60 s) so a check that registers late cannot be skipped.
* **Merge**: after the gate approves, the PR merges only when the task has `AutoMerge`; otherwise the task blocks with
  "ready to merge: awaiting a human" and the escalator is told. A human merging or closing the PR completes or fails
  the task.

## The reviewer

Every pull request is reviewed by a model before it can merge (`NoReview` opts out, merging on green CI alone).

* **`FACTORY_REVIEW_MODEL` set**: that model reviews, and the gate refuses to approve a change written by the same
  model. This is the independent review.
* **`FACTORY_REVIEW_MODEL` unset**: the default model (`LLM_DEFAULT_MODEL`) reviews. This is allowed, and weaker: a
  model grades its own work and shares its blind spots. Every such approval is posted on the pull request starting with
  "Note: this review was done by the same model that wrote the change, so it is not independent", and the setup
  check reports a warning. Set a second model when the repository matters.

Before any model call the gate checks the
diff mechanically: protected paths (`.github/workflows/**`, `**/.env*`, keys, `Dockerfile`) and secrets in added
lines are **hard** failures (the task blocks for a human); text that tries to instruct the reviewer is a blocker.
The model then returns `{approve, summary, findings[]}`; any blocker or major finding, or a truncated source file,
blocks approval. A diff over 400 KiB or 80 files is not reviewed automatically.

## Autonomy policies

`Task.Policy` (default `sandboxed`):

| Policy | Behaviour |
|---|---|
| `supervised` | reads are free; every write, command, network and git action asks a human |
| `sandboxed` | free inside the workspace; denied: force-push, push to a protected or the base branch, merges, secrets, protected-path writes, publishing, destructive commands outside `/workspace` |
| `autonomous` | like `sandboxed`; kept for tasks that need no approvals at all |

Force-push and secret access are denied in **every** policy. Shell commands are parsed (`&&`, `;`, `|`, `$(…)`,
backticks, `sh -c`, `eval`, `xargs`, `env`, `sudo`…) and classified per simple command; where parsing is uncertain
and the text looks like a push, it is treated as a push. A denial tells the model what to do instead (push your own
branch and call `open_pr`; ask a human). Every denial and ask is a `policy` task event. Ordinary chat threads are
never touched. Extra rules: `FactoryOptions.PolicyConfig{Deny, Ask}` with patterns like `Bash(terraform apply*)`,
`Write(*.lock)`, `Tool(open_pr)`.

The policy is advisory *inside* the sandbox: it stops the model, not a determined process. Real isolation is the
sandbox backend (`opensandbox` with an egress allow-list and credential proxying; not `local`).

## Budgets and guards

* **Per task, set by whoever creates it.** A task (or a trigger's template) carries its own `budget {maxUsd, maxTokens}`
  over all attempts and delegated runs; there is no deployment-wide default, and a task with no budget has no cap of its
  own. Values must be numbers >= 0 (zero removes the cap on that axis). When a task reaches its cap it blocks and its
  tools are denied (read-only ones stay) so the run can wrap up; at 80 % the run is told.
* **Raising a cap.** A blocked task resumes with a new budget: `POST .../tasks/{tid}/resume {"feedback": "...",
  "budget": {"maxUsd": 5}}`. Only the task's creator or a project owner can change it, and the new cap must be above
  what the task already spent (or 0 to remove it), otherwise the task would block again at once. The change is a
  `budget` event in the task's history.
* **Per day, global**: `FACTORY_DAILY_BUDGET_USD` / `_TOKENS` over a 24 h window (per project with
  `FACTORY_DAILY_PER_PROJECT=1`). The dispatcher checks it before it starts a task. This is the one cap the deployment
  sets; the old `FACTORY_TASK_BUDGET_*` variables are ignored and logged as such.
* A run is capped at 150 tool calls; a run that keeps calling tools after being refused is cancelled.
* A model without a known price is counted in tokens and flagged `unpriced` in the report, never treated as free.
* Inside a run the existing loop guards still apply (identical calls, A-B-A-B alternation, repeated errors).

## Delegation

An agent can call `delegate{role, task}` to run a full sub-agent (own thread, sandbox and prompt) and get a bounded
report, or `delegate_many` to run up to 3 in parallel. Read-only roles run in plan mode. Depth is limited to 2 and a
run to 24 delegations; a delegated run's spend is booked on the task.

## Running it: `harness-web`

Team mode, a GitHub App and Postgres are the production shape (`docs/team.md`). Then:

| Variable | Meaning |
|---|---|
| `FACTORY=1` | turn the factory on |
| `FACTORY_WEBHOOK_SECRET` | GitHub webhook secret; point the App's webhook at `POST /hooks/github`. Without it every delivery is refused |
| `FACTORY_REVIEW_MODEL` | optional: the reviewer model, which must differ from `LLM_DEFAULT_MODEL` (an independent review). Unset: the default model reviews, and says so; `FACTORY_NO_REVIEW=1` merges on CI alone |
| `FACTORY_REQUIRE_TESTS=1` | the reviewer flags source changes without a test |
| `FACTORY_PROTECTED` | extra comma-separated path globs no agent change may touch |
| `FACTORY_POLICY` | default policy (`supervised`, `sandboxed`, `autonomous`) |
| `FACTORY_DAILY_BUDGET_USD`, `_TOKENS`, `FACTORY_DAILY_PER_PROJECT=1` | daily cap (a task's own budget is set by its creator) |
| `FACTORY_MAX_CONCURRENT`, `FACTORY_MAX_ATTEMPTS` | tasks running at once (4), attempts per task (3) |
| `FACTORY_RUN=0` | do not run the dispatcher, scheduler and PR loop in this process (in `harness-worker` it is the other way round: they run only with `FACTORY_RUN=1`) |
| `FACTORY_LESSONS=1` | distil lessons from finished tasks into project memory (needs the memory feature) |

Without `POSTGRES_HOST` the factory keeps its state (tasks, events, triggers, thread links, the spend ledger and seen
webhook deliveries) in `factory.json` next to `team.json` in `LOCAL_STATE_DIR` (default `~/.har/sessions`), so it
survives restarts and redeploys; this needs a persistent volume and a single `harness-web` process. The file is
rewritten atomically on every change to a task, trigger, spend or thread link; events and delivery ids are batched
(at most one rewrite per 200 ms, flushed on shutdown), and a task keeps its newest 500 events. A file that cannot be
parsed stops the server at startup with its path instead of being reset. With `POSTGRES_HOST` the state is in the
database and several processes can share it. With the GitHub App configured the factory opens, watches and merges
pull requests; without it, tasks end with the agent's answer.

### API

```
GET    /api/projects/{pid}/tasks?state=       POST /api/projects/{pid}/tasks {title,spec,role?,model?,policy?,repo?,
GET    /api/projects/{pid}/tasks/{tid}              baseBranch?,budget?,maxAttempts?,autoMerge?,dependsOn?,priority?}
POST   /api/projects/{pid}/tasks/{tid}/cancel | /resume {feedback?, budget?}
GET/POST /api/projects/{pid}/triggers (owner)   DELETE /api/projects/{pid}/triggers/{trid} (owner)
GET    /api/projects/{pid}/factory/report?days=
POST   /hooks/github                           (HMAC-verified; no session)
```

A trigger is `{name, kind: "cron", schedule: "0 2 * * *", template}` or
`{name, kind: "webhook", event: "issues.labeled", match: {label: "agent"}, template}`. Template `title` and `spec`
are Go `text/template`s over the event: `Fix #{{.number}}: {{.title}}\n\n{{.body}}`. Redelivered webhooks and
overlapping cron ticks create one task (`Source.Key` is unique per project).

## Embedding

```go
st := factorystore.NewMem(nil) // or factorystore.NewPG(pool, nil)
h, _ := harness.New(harness.Options{Config: cfg, Features: append(harness.ProfileCoding(),
    harness.FactoryFeature(harness.FactoryOptions{
        Store: st, Run: true, Policy: true,
        Forge: forge.NewGitHub(forge.GitHubOptions{Auth: auth}), Auth: auth,
        ReviewModel: "a-different-model", RequireTests: true, // ReviewModel "" lets the default model review
        DailyBudget: factory.Budget{MaxUSD: 100},
    }))})
f := h.FactoryOf()
f.Tasks.Create(ctx, factory.Task{Owner: "alice", Title: "…", Spec: "…", Repo: "acme/app", AutoMerge: true})
mux.Handle("POST /hooks/github", f.WebhookHandler())
```

## Evaluating the harness

`bench -swebench instances.jsonl [-swebench-ids a,b]` runs SWE-bench style instances (cloned from GitHub at
`base_commit`; the test patch is hidden from the agent and applied only to verify) through the real harness.
`swebench.Evaluate` reports resolved only when **all** `FAIL_TO_PASS` pass and **no** `PASS_TO_PASS` breaks. Needs
`git` and a `python3` with `pytest`. Use it before and after a change: `bench -compare before.json after.json`.

## Visibility and escalation (team mode)

* Every thread the factory spawns for a task (the task's run and its delegated sub-agents) is linked to the task's
  project through `FactoryOptions.OnSpawn`, so project members see it in the thread list and can open it while the
  agent works. `harness-web` wires this to the team store.
* A task that needs a human (`blocked`: ready to merge, budget exhausted, waiting for an approval, the same failure
  three times, a policy violation) publishes a `task.blocked` event on the project's live feed and, when the owner has
  linked Telegram, sends them the title, the reason and a link to the project.
* A `supervised` task whose run stops for an approval blocks as `waiting for approval` without using an attempt.
  Approving it on the thread (`POST /api/threads/{id}/resume`, or the thread's approval card) continues the same
  attempt and the task returns to `running`.

## Verification

`features_factory_test.go` and `features_factory_pr_test.go` drive the facade end to end with scripted models and a
fake code host: a task finishing with its spend metered, repeated identical failures blocking, a force-push refused
under every policy, a supervised task waiting for and resuming after an approval, a pull request going red CI → CI log
fed to the next attempt → green → reviewer → merge at the fixed head, a green approved PR handed to a human without
`AutoMerge` (and finishing when a human merges it), and reviewer findings becoming the next attempt.

## Checking a real setup

The factory has been exercised end to end only against fakes. Before pointing it at a real repository run the
read-only preflight with the same environment as the server:

```
harness-web -factory-check acme/app
[ OK ] webhook secret: set (32 chars)
[FAIL] app permissions: pull_requests is read, the factory needs write
       fix: GitHub > Settings > Developer settings > your App > Permissions: Pull requests: Read and write
...
```

It checks the App's permissions and subscribed events, the installation on the repository, that a token mints and can
push, that the default branch exists (and whether it is protected), the reviewer model (a warning when unset, because the default model then reviews its own work; a failure when set to
the author's own model or to one outside `LLM_MODELS`), the sandbox backend and per-user quota against
`FACTORY_MAX_CONCURRENT`, and whether a daily spend cap is set. It writes nothing to the repository (it mints one installation token). Exit status is 1 when a check
fails; warnings do not fail.

## How a branch gets to GitHub

The agent holds **no push credential**. It commits in `/workspace/repo` on its task branch and calls `open_pr`; the
host then pushes that one branch (`HEAD:refs/heads/<task branch>`, with `--force-with-lease` because the branch is the
factory's own and a rebase after a conflict must be pushable) and opens or updates the pull request. The installation
token appears only in the environment of the clone, fetch and push commands, never in a file, a command line or the
repository's git config, and the commands run with the global and system git configuration disabled. They refuse to
run if the repository's own config redirects git (`url.*`, `http.*`, `credential.*`, a proxy, a changed `origin`), if
the checkout is a linked worktree of another repository, or if it sits inside one. `open_pr` refuses to push the base
branch or a protected branch, and a failed push opens no pull request.

This is what makes the policy's push rules a second layer rather than the only one: even a command the classifier
misses cannot push, because the sandbox holds nothing to push with.

## Security model

The factory runs code an LLM wrote, on behalf of text written by people. What it relies on, and what it does not:

* **A project's tasks run only in that project's repository.** The GitHub App token for a run is minted for the
  task's repository, so the task's repository is fixed to the project's own: a request naming another repository is
  rejected, and every trigger is pinned to the project's repository (a webhook trigger also matches only events from
  it). Without this, a member of one project could write to any repository the App is installed on.
* **Only an owner can allow automatic merging** (`autoMerge`). A member can create tasks, which end in a pull request
  that waits for a human.
* **GitHub issue and comment text is data.** A task created from a webhook has its description framed as written by
  an outside user, with an instruction to ignore requests for secrets, CI changes, other branches or merging. This
  lowers the chance of a prompt injection steering the agent; it does not remove it. The policy, the protected paths
  and the independent reviewer are the layers that hold when it does not.
* **Spend is gated before work starts.** The dispatcher asks the budget guard before it starts a task, so a day's cap
  or a repeated failure stops the task instead of running another attempt. The guard also stops a task at the end of
  the run that crossed a cap.
* **Webhooks are authenticated by HMAC** and deduplicated by delivery id. Without `FACTORY_WEBHOOK_SECRET` every
  delivery is refused.
* **Not covered:** a determined process inside the sandbox (use `opensandbox` with an egress allow-list and the
  credential proxy, never `local`, for untrusted tasks); a malicious project owner (owners can set budgets,
  policies and triggers); and a model that approves its own bad work (an independent reviewer is a different model, not an oracle; with no
  `FACTORY_REVIEW_MODEL` it is the same model, and every approval says so). Keep GitHub branch protection on.

## Deployment shape

The factory runs in the web tier and in the workers, over one Postgres and one Redis.

* **`harness-web`** (`FACTORY=1`) serves the API and the webhook, and by default runs the dispatcher, the cron
  scheduler and the pull request loop. Several replicas sharing Postgres are safe: every claim is a conditional or
  `SKIP LOCKED` update.
* **`harness-worker`** (`FACTORY=1`) installs the same factory in its harness over the worker's own Postgres
  (`POSTGRES_*`; its migrations create the factory tables), so the runs it executes behave as in the web tier. The
  dispatcher, the scheduler and the pull request loop stay **off** in a worker unless it sets `FACTORY_RUN=1`: a
  worker's job is executing runs. Run them in the web tier, or in the one worker that opts in.

A run is finished by the process that executed it. The end-of-run hooks (the task moves on, its spend is metered
and capped), the `open_pr` and `delegate` tools, the autonomy policy and the budget guard therefore act in
whichever process takes the run from the queue, which is why **every worker must run with the same `FACTORY_*`
settings as the web tier**. A worker without `FACTORY=1` that takes a factory run executes the agent with none of
them: no tools, no policy, no metering, and the task stays `running` until the tasks sweep notices.

Share between the web tier and every worker:

| Variables | Why |
|---|---|
| `POSTGRES_*`, `REDIS_*` | the tasks, the queue and the event log |
| `FACTORY=1` and the `FACTORY_*` settings: `FACTORY_POLICY`, `FACTORY_PROTECTED`, `FACTORY_REQUIRE_TESTS`, `FACTORY_REVIEW_MODEL` / `FACTORY_NO_REVIEW`, `FACTORY_DAILY_BUDGET_*`, `FACTORY_DAILY_PER_PROJECT`, `FACTORY_MAX_ATTEMPTS`, `FACTORY_LESSONS` | the hooks, policy and budget guard of a run are built from them where the run executes |
| `GITHUB_APP_*` | the token the host clones and pushes with, and that `open_pr` and the pull request loop call GitHub with. The agent never receives it |
| `LLM_*`, the sandbox settings | a run uses the worker's models and sandboxes |

Only the web tier needs `FACTORY_WEBHOOK_SECRET` (the webhook is served there). `FACTORY_MAX_CONCURRENT` is checked
by the dispatcher, so it matters where the dispatcher runs.

Left out of the worker, because they belong to the product: the project link of a task's threads (the project's
members see them in the web tier's thread list only through the team store) and the escalations to the live feed and
Telegram. In a worker a task that needs a human is only logged; the task itself is blocked in the shared store
either way, and the web tier shows it.

If a worker dies mid-run, its run is interrupted by the runtime sweeper (`docs/embedding.md`, a live process notices
the stale heartbeat) and the tasks sweep, which runs in any process with the factory, records the attempt as failed
and retries it, to be taken by another worker.

## Known limits

* The policy and the secret scan are heuristics. They are a seatbelt, not a boundary.
* The reviewer is a model: it lowers the chance of a bad merge, it does not remove it. Keep `AutoMerge` off for
  repositories where a bad merge is expensive, and keep branch protection on in GitHub.
* Only GitHub is implemented (`factory.Forge` is the seam for another host).
* The PR loop polls each open pull request every 30 s while something is changing, and backs off (doubling to 5 min,
  2 min while it only waits for CI) while nothing is, so many open PRs do not exhaust the installation's API limit.
  It looks at most 25 pull requests per tick. A webhook wakes a PR at once and resets its backoff; with no webhook a
  quiet PR is noticed within the backoff gap.
* Lessons are written to project memory and recalled by the existing memory feature; nothing retrains a model or
  edits prompts and skills automatically.
