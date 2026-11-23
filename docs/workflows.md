# Workflows

A project keeps **workflows**: its procedures (SOPs) as a versioned graph of steps. A **run** carries one out: each
step becomes a factory task (or a human gate, or a child run), in dependency order, with the steps that do not depend
on each other running together. Team mode only; the routes are served when `product.Options.Workflows` is set
(`harness-web` sets it in team mode), and runs need the agent factory (`FACTORY=1`): without it a workflow can be
designed but not run (501).

| Piece | Where |
|---|---|
| vocabulary, validation, templating, the four builtin templates, the `Store` and `Engine` ports | `core/workflows` |
| mem, file (`workflows.json` beside `team.json`) and Postgres stores (migration `0600_workflows.sql`) | `adapters/store/workflowstore` |
| the engine: starts steps as factory tasks, follows them, rework, gates, controls | `features/workflows` |
| routes, inbox gates, feed events | `app/product/workflows.go`, `app/product/inbox.go` |
| the tab: list, templates, designer, runs view | `web/src/pages/Workflows.tsx`, `web/src/components/workflows/`, project tab `?tab=workflows` |

## The graph

A workflow has a name, **inputs** (asked when a run starts) and **nodes** with ids and `dependsOn` edges.

| Kind | What it does |
|---|---|
| `agent` | a role (a builtin one or one of the project's Roles) works on a prompt; `model`, `policy` and `budget` override the task's; `outputs` names the artifacts the agent reports as `### name` sections; `useRepo` works on the run's branch of the project's repository, `waitMerge` waits for the merge (by default the step is done once the pull request is open) |
| `script`, `build`, `preview` | a shell command that succeeds when it exits 0 |
| `gate` | blocks until a member of `approvers` (logins, `role:member`, `role:owner`; project owners may always decide) approves, sends back with feedback, or adds context |
| `subworkflow` | runs another published workflow of the project as a child run, with templated inputs (nesting is bounded and recursion refused) |

Prompts and commands are templated: `{{inputs.name}}`, `{{nodes.<id>.result}}`, `{{nodes.<id>.outputs.<name>}}`,
`{{requirement.title}}`, `{{requirement.body}}`. The task of a step is also given the results of the steps it depends
on (and the notes added at a gate), the feedback of a rework, and the linked requirement.

**Failure policy** (`onFailure`): `fail` (the default) fails the run; `continue` lets the dependents go on; `rework`
sends the run back to an earlier step it depends on with the failure text, at most `maxRounds` times (3 by
default), then fails the run. A gate's send-back uses the same edge (its target, or the step before it).

**Validation** (on save, publish and `POST .../validate`): no cycles, existing references (dependencies, rework
targets, `{{...}}` variables, subworkflows), roles exist for the project, agents have a prompt, commands a command,
gates approvers and something to review.

**Versions.** Every save bumps `version` and keeps the previous one (the last 10). A saved workflow is a `draft`;
`publish` validates it and marks it `published`; a later save makes it a draft again. Only a published workflow runs,
and a run keeps the graph it started with. Restoring a version makes it a new draft.

**Templates** (read-only, copied into the project as drafts): backend delivery (plan, implement, test, review gate,
pull request), frontend delivery (plan, implement, preview build, review gate), product proposal (research, draft,
human gate), bug fix (reproduce, fix, test, pull request).

## Runs

A run is `running`, `paused`, `done`, `failed` or `cancelled`; a step is `pending`, `running`, `waiting` (a gate),
`done`, `failed`, `skipped` or `cancelled`. A step's task carries the labels `workflow:<run id>` and `wfnode:<node id>`
and the source `workflow`; the factory dispatcher, budgets, policies and the PR loop work on it as on any task. The
steps that use the repository share the branch `workflow/<run id>`.

The engine adds no scheduler. The factory's `OnTaskState` hook calls `TaskMoved`, which advances the run, and a
15 second sweep catches what a restart or another process missed (`Engine.Run`). Changes to a run are saved from the
version read, so two processes cannot overwrite each other.

Controls: **pause** (no new step starts), **resume**, **cancel** (running tasks and child runs are cancelled),
**rerun from a node** (it and everything downstream is invalidated, running tasks cancelled; works on a finished run
too), **retry** a failed step, **take over** (a person gives the result; the task is cancelled), and at a gate
**approve**, **send back** (with feedback; the target step runs again) and **add context**.

## HTTP

```
GET    /api/workflow-templates
GET    /api/projects/{pid}/workflows
POST   /api/projects/{pid}/workflows                           member, 400 {error:{code:"invalid_workflow", issues}}
POST   /api/projects/{pid}/workflows/validate                  member, nothing stored
POST   /api/projects/{pid}/workflows/from-template {template, name?}
GET|PUT|DELETE /api/projects/{pid}/workflows/{wid}             PUT: {…, version?}, a stale version is 409
POST   /api/projects/{pid}/workflows/{wid}/publish|duplicate|restore
GET    /api/projects/{pid}/workflows/{wid}/history
GET    /api/projects/{pid}/workflow-runs?workflowId=&state=
POST   /api/projects/{pid}/workflow-runs {workflowId, inputs?, requirementId?}
GET    /api/projects/{pid}/workflow-runs/{rid}                 the run and who may decide its gates
POST   /api/workflow-runs/{id}/pause|resume|cancel|rerun|retry|takeover
POST   /api/workflow-runs/{id}/nodes/{nid}/approve|send-back|add-context
```

Waiting gates the caller may decide are in `GET /api/needs-review` (`gates`), so the Inbox shows them with approve,
send back and add context. The live feed carries `workflow.changed`, `workflow.run` and `workflow.gate`.

## Limits

Script, build and preview steps run as a factory task whose agent runs the command in its sandbox and reports an
`EXIT_CODE:` line (a missing line fails the step); there is no direct sandbox exec yet. Steps that share the run's
branch each open (or update) the same pull request, so CI and the reviewer judge it once per step that uses the repository.
