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
| `features/factory/learning` | the self-improvement loop: run ledger, memory scoring and curation, failure mining, prompt experiments, the improver (see below) |
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

Roles are not only the five builtin ones. A project (or a user) defines roles of its own (`core/roles`, stored by
`adapters/store/rolestore`, served under `/api/projects/{pid}/roles` and `/api/me/roles`): a prompt, a model, a reasoning
effort, tool limits (read-only, allow and deny lists, MCP servers), skills to load, a permission mode, an enabled flag and a
version history (the last 10). `delegate`, `delegate_many` and a task's `role` ask the registry first
(`Options.Roles`, supplied by the product as a `roles.Resolver`: personal over project over builtin; a project role of
the same name replaces a builtin; a disabled one hides it) and fall back to the builtin catalogue. A delegated registry role
does not copy its prompt into the thread: the sub-run records a reference (`SpawnRequest.RoleRef`) and the roles feature
renders the prompt and applies the tool limits when it executes, in whichever process that is. A chat run can take a role
too (`RunOptions.Role`, `role` in `POST /threads/{id}/runs`, the composer's `@`): the role's model, effort, permission
mode and skills replace the run's, its prompt joins the system prompt and its tool limits filter the tool set for that
run only.

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
| `FACTORY_RETRY_BACKOFF_SECONDS` | a fixed delay before a failed attempt is retried (default: 30 s doubling, capped at 15 min); for tests |
| `FACTORY_RUN=0` | do not run the dispatcher, scheduler and PR loop in this process (in `harness-worker` it is the other way round: they run only with `FACTORY_RUN=1`) |
| `FACTORY_LESSONS=1` | distil lessons from finished tasks into project memory (needs the memory feature) |
| `FACTORY_LEARNING=1` | record each run's context and each task's outcome, score memory entries by it ([Learning and self-improvement](#learning-and-self-improvement)) |
| `FACTORY_LEARNING_CURATE=1`, `FACTORY_LEARNING_VARIANTS=file.json` | retire or promote memory entries by their record; run a prompt experiment |
| `FACTORY_IMPROVE_REPO=owner/name`, `FACTORY_IMPROVE_OWNER` | turn recurring failures into improvement pull requests (never auto-merged); also `_PROJECT`, `_BASE_BRANCH`, `_MODEL`, `_MIN_COUNT`, `_MAX_OPEN`, `_MAX_PER_DAY`, `_BUDGET_USD`, `_FROZEN` |

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

## Learning and self-improvement

The factory learns facts (lessons) on its own. This feature closes the loop: it measures whether what the agents were
given helped, and changes the setup only behind an eval gate and a pull request. Switch it on with `FACTORY_LEARNING=1`
(`FactoryOptions.Learning`); the rest builds on it. It needs no schema change: attribution is stored as task events and
the counters in the memory entries' metadata.

| Step | What it does | Switch |
|---|---|---|
| **Ledger** | each run's recall is recorded as a `context` event (memory entry ids, model, prompt variant); when the task ends an `outcome` event adds the result, cost, attempts and the skills the thread used | `FACTORY_LEARNING=1` |
| **Dataset** | `learning.Collect` flattens finished tasks into rows (task, context, result, CI failures, review rejects, cost, duration). Served as JSON lines at `GET /api/projects/{pid}/factory/learning` | with the ledger |
| **Memory scoring** | each recalled entry counts a use, a win or a loss per finished task (`uses`, `wins`, `losses` in its metadata, never twice for a task). A blocked task is provisional and does not count | with the ledger |
| **Curation** | every 6 h, an automatically written entry (a lesson) with at least 6 verdicts is compared with the project's success rate: clearly below it loses importance and, when far below, is retired (soft-deleted, restorable); clearly above gains importance and the tag `proven`. Entries a user wrote or pinned are never touched | `FACTORY_LEARNING_CURATE=1` |
| **Failure mining** | tasks that failed, or blocked on a failure, are grouped by `factory.Fingerprint`; `?view=clusters` lists the recurring ones with their cost. `learning.CandidatesOf` turns them into regression-case outlines for `bench/candidates/` | with the ledger |
| **Prompt experiments** | `FACTORY_LEARNING_VARIANTS=file.json` (`{"name":"extra instructions"}`) gives each task the control or one variant by a hash of its id; `?view=experiment` compares them and names a winner only with enough tasks, a one-sided p under 0.05 and a cost under 1.5x the control | `FACTORY_LEARNING_VARIANTS` |
| **Eval gate** | `bench -gate before.json after.json` accepts or rejects a change on measured results (see below) | none, a CLI |
| **Improver** | daily, turns each recurring failure into a task for the coder role: find the cause, make the smallest change (a lesson, a skill under `app/product/skills/`, a prompt or tool description), add the regression case, run the gate, open a pull request | `FACTORY_IMPROVE_REPO` |

### The eval gate

```
bench -tier hard -n 3 -out before.json     # on the base branch
bench -tier hard -n 3 -out after.json      # with the change
bench -gate before.json after.json         # exit 0 accept, 1 reject, 2 inconclusive
bench -gate -gate-require-gain ...         # accept only a measurable improvement
```

It rejects when a task's pass rate fell by more than a third or the overall rate fell significantly (one-sided
two-proportion test, 10 %). It is inconclusive, and so does not accept, when a task has fewer runs than `-gate-min-runs`
(3) or a task of the baseline is **missing** from the new report: shrinking the suite is the easiest way to raise a
score. With `-gate-require-gain` it also needs a higher pass rate, or the same rate with 5 % fewer uncached tokens.

### Guardrails of the improver

* **It never merges.** Its tasks have `AutoMerge` off: the independent reviewer reviews the pull request and a human merges it.
* **It cannot edit what judges it.** While `FACTORY_IMPROVE_REPO` is set, `learning.DefaultFrozen` (`bench/*.go`,
  `bench/swebench/**`, the learning, review, policy and budget packages) is added to the protected paths of both the
  review gate (a hard failure) and the policy engine (the write is denied), for **every** factory task, not only the
  improver's. Override the list with `FACTORY_IMPROVE_FROZEN`. Adding a case under `bench/candidates/` stays allowed.
* **It is rate limited.** At most `FACTORY_IMPROVE_MAX_OPEN` (2) improvement tasks in flight and
  `FACTORY_IMPROVE_MAX_PER_DAY` (3) per 24 h, each capped at `FACTORY_IMPROVE_BUDGET_USD` (5), 2 attempts, sandboxed
  policy, lowest priority. A failure is proposed once (`Source.Key = improve:<signature>`).
* **The failure text it is shown is data**, fenced and capped; the task spec says so.

Required: `FACTORY_IMPROVE_OWNER` (the login the tasks run as), the GitHub App, and `FACTORY_LEARNING=1`.

What it does **not** do: it does not generate runnable bench tasks (they need a fixture and hidden tests, so a mined
failure becomes a candidate outline that the improver or a person completes), it does not detect that two lessons
contradict each other, and its attribution is statistical, not causal: an entry is judged against the project's
baseline over many tasks, so a few tasks say little (hence the minimum of 6 verdicts and the confidence interval).

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
| `FACTORY=1` and the `FACTORY_*` settings: `FACTORY_POLICY`, `FACTORY_PROTECTED`, `FACTORY_REQUIRE_TESTS`, `FACTORY_REVIEW_MODEL` / `FACTORY_NO_REVIEW`, `FACTORY_DAILY_BUDGET_*`, `FACTORY_DAILY_PER_PROJECT`, `FACTORY_MAX_ATTEMPTS`, `FACTORY_LESSONS`, `FACTORY_LEARNING*`, `FACTORY_IMPROVE_*` | the hooks, policy and budget guard of a run are built from them where the run executes |
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

## External coding agents (executors)

A role (`core/roles`) may name an *executor*: a CLI coding agent that runs inside the run's sandbox in place of the
internal loop (`claude-code`, `codex`, `opencode`, `omp` (Oh My Pi), or a command template a project's owner defines). The
`run_executor` tool (`features/executors`, installed with `harness.Executors`, before the factory) runs it
non-interactively, streams its output as live tool output, enforces the timeout and the run's cancellation (the
process dies with the sandbox call), reads its final summary and `git diff --stat`, books the cost the CLI reports
(Claude Code's `total_cost_usd`, token counts for Codex; a CLI that reports nothing books an unpriced run) against
the task budget, and refuses to start once the budget is spent.

* The 7ar harness (the internal loop) stays the recommended and default runner: a role with no executor uses it, and it is
  the only one that uses the project's memory and counts every token against the budget. Executors are optional extras.
* Policy: the policy engine classifies `run_executor` as an exec, so a *supervised* task asks a human first and the
  others do not. The CLI's level follows the task: read-only for a read-only role or plan run, edit under
  supervised and sandboxed, full (no prompts inside the sandbox) under autonomous. A `policy` event (the level) and an
  `executor` event (outcome, usage) are recorded on the task. The review gate and the protected-path and secret checks
  inspect the diff whoever wrote it. `delegate` to an executor role is refused under the supervised policy.
* A factory task of a role with an executor runs as its *driver*: the internal loop is told to call `run_executor`,
  verify, commit and open the pull request.
* Credentials: each user seals their own API key and base URL per executor (`PUT /api/me/executors/{name}/credentials`,
  write-only), stored as capability installs named `exec_<name>` sealed with `HARNESS_SECRET_KEY`. They are passed in
  the environment of that one process (only the variables the executor declares), redacted from everything the run
  prints, and never written to disk. The credential broker of the sandbox adds headers to HTTPS requests of the
  sandbox; it is not used here because the CLIs read their key from the environment.
* Installing a missing CLI needs `install=true` on the call and `HARNESS_EXECUTORS_INSTALL=1` on the server.
* **Availability.** `GET /api/executors` carries `availability` for each executor: `ready` (the binary is in the default
  sandbox image), `install` (it is not, and a run installs it on first use), `missing` (it is not and nothing installs
  it) or `unknown` (no sandbox backend, or the probe has not answered). It comes from one `command -v` per binary in a
  short-lived sandbox, cached for 10 minutes; a request never waits more than 3 seconds for it. The Settings card
  shows it as a badge, with the environment variables the executor reads.
* **Failures with a fix.** A run that fails because the CLI is missing, or because it could not authenticate (command not
  found, 401, invalid API key...), reports `problem:` with what to do ("add your key in Settings > Executors", and which
  variables it reads) in the tool result and in the task's `executor` event.
* **Cost.** A CLI that reports no tokens or dollars (omp, opencode, custom commands) is booked as one *unpriced* run:
  the task event says "cost not reported by this CLI" and the Task detail and Usage rows say so. No number is made up
  from the wall time. Claude Code (dollars and tokens) and omp (tokens, and dollars when its model has a price) report
  usage; Codex reports tokens only.
* **What was verified against the real CLIs.** `features/executors/clitest` runs the real binaries (claude 2.1.331,
  codex-cli 0.172.0, omp 19.1.1) through `Provider.Run`, with the builtin command lines and parsers, against scripted fake
  model servers on 127.0.0.1 (Anthropic Messages SSE, OpenAI Responses SSE, OpenAI chat-completions SSE), in a throwaway
  HOME and a temporary git repository, with a clean environment. It is opt-in (`HARNESS_REAL_CLIS=<dir>`, see the package
  doc) and needs no key. The fixtures in `core/executors/testdata/real` are the real stdout of those runs. Per CLI:
  * Verified end to end, at the edit level, at the full level (autonomous), at the read-only level, with a rejected key,
    with a timeout (the CLI and the command it started are killed) and with a cancel: the CLI uses its own file or shell
    tool, the file appears, the final summary, token usage and (claude) `total_cost_usd` are parsed, the changed files and
    the diff stat (untracked files included) are reported, no raw JSON reaches the progress.
  * Read-only: Claude Code `--permission-mode plan` refuses the write ("Cannot write to ... while in plan mode", a
    `permission_denied` system line, `permission_denials` in the result; the exit code stays 0, so the parser adds a note to
    the summary); Codex `--sandbox read-only` makes the OS sandbox refuse the write (exit 0; `codex exec --json` emits no
    item for the refused command, so the proof is the file, not the progress); omp `--tools read,grep,glob` does not offer
    `bash` or `write` (a call fails with "Tool bash not found", exit 0). Not verified: Codex's Linux sandbox (landlock or
    bubblewrap) inside the sandbox container; the run above used macOS seatbelt.
  * Codex does NOT read `OPENAI_API_KEY` or `OPENAI_BASE_URL` in `codex exec`: it then asks the real OpenAI API with no
    key (401, "Missing bearer or basic authentication"). The builtin therefore maps them: `CODEX_API_KEY` is
    `${CODEX_API_KEY:-$OPENAI_API_KEY}` and a base URL becomes `-c openai_base_url=...`. Verified with a custom base URL.
    The default model of codex 0.172 (`gpt-6.2-sol`) uses a "code mode" tool shape (one `exec` JavaScript tool in an
    `additional_tools` input item); the fake server does not script it, so the runs pin `gpt-5.1-codex` in the CLI's
    config. The events the parser reads (agent_message, command_execution, turn.completed, turn.failed, error) are the same
    whatever the model, but a run with the default model is not covered. Its `error` events are mostly transient
    ("Reconnecting... 2/5") and no longer fail a run; only `turn.failed`, or a stream that ends in an error, does.
    `file_change`, `mcp_tool_call` and `web_search` items are parsed from the documented shapes and were not seen in a run.
  * omp: `omp -p --mode json` prints a stable JSONL event stream (session header, agent_start, message_start/update/end,
    tool_execution_start/end, turn_end, agent_end). The `omp-json` parser reads the assistant `message_end` events for
    the text and usage (`totalTokens`, `cost.total`) and `stopReason: "error"` for a failure (exit 1, `errorMessage`).
    `OPENAI_BASE_URL` is NOT honored by omp's builtin openai provider (it asked api.openai.com); a custom endpoint needs
    a `models.yml` provider in the CLI's own config, so the base-URL variables declared for omp only help providers that
    read them. The runs configure the fake provider in `~/.omp/agent/models.yml` and `config.yml` (`modelRoles.default`).
  * A rejected key: Claude Code retries a 401 ten times with backoff (about three minutes) and then prints
    "Invalid API key · Fix external API key" (`CLAUDE_CODE_MAX_RETRIES` shortens it); with no login it prints "Not logged
    in · Please run /login" at once; Codex retries five times (about 6 s) and ends with `turn.failed` "unexpected status 401
    Unauthorized: ..."; omp prints "401 Incorrect API key provided: ..." and exits 1. `Diagnose` recognises all of them
    (tested on the real messages).
  * The first request of Claude Code is a `HEAD /api/hello` probe; its agent loop uses `POST /v1/messages` only (no
    count_tokens or models call was seen). A process the CLI starts in its own session (codex, omp commands) survives a
    kill of the process group: the clitest sandbox kills the descendants as a container does; `adapters/sandbox/local`
    kills the process group only.
  * Not verified: OpenCode (not installed here, its output is read as text), a real model, the Claude Code stream with
    `--include-partial-messages`, very long runs, and Windows.
* `omp` (Oh My Pi, github.com/can1357/oh-my-pi, `bun install -g @oh-my-pi/pi-coding-agent`) is run as
  `omp -p --mode json ... </dev/null` (stdin is closed because omp reads a non-terminal stdin to EOF before it starts).
  A headless omp cannot answer an approval prompt, so the edit and full levels both use `--approval-mode yolo` and the
  sandbox is the boundary.

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
