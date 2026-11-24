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
| `agent` | a role (a builtin one or one of the project's Roles) works on a prompt; `model`, `policy` and `budget` override the task's; `outputs` names the artifacts the agent reports as `### name` sections; `useRepo` works on the run's branch of the project's repository (`repo` names another repository for that step), `waitMerge` waits for the merge (by default the step is done once the pull request is open) |
| `script`, `build`, `preview` | a shell command that succeeds when it exits 0; it runs directly in the run's sandbox (see "Command steps") |
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
steps that use the repository share the branch `workflow/<run id>`, and a step that names another repository with `repo`
works on its own branch there: `workflow/<run id>-<repository slug>`. A run opens **one branch and one pull request per
repository**, never one `workflow/<run>` pull request for steps on different repositories; the base branch of the run is
only used for the run's own repository.

The engine adds no scheduler. The factory's `OnTaskState` hook calls `TaskMoved`, which advances the run, and a
15 second sweep catches what a restart or another process missed (`Engine.Run`). Changes to a run are saved from the
version read, so two processes cannot overwrite each other.

Controls: **pause** (no new step starts), **resume**, **cancel** (running tasks and child runs are cancelled),
**rerun from a node** (it and everything downstream is invalidated, running tasks cancelled; works on a finished run
too), **retry** a failed step, **take over** (a person gives the result; the task is cancelled), and at a gate
**approve**, **send back** (with feedback; the target step runs again) and **add context**.

## Command steps

A `script`, `build` or `preview` step runs its command **directly in the run's sandbox**: no model call and no spend.
The sandbox is one per run and repository (a step with `useRepo` finds the repository checked out on the step's branch in
`/workspace/repo`, a step without it runs in `/workspace`), shared by the command steps of the run and destroyed when the
run reaches a final state. The command runs with a timeout (15 minutes), its output is saved into the step's log while it
runs (the last 64 KB, shown in the step panel), and the exit code is the result: 0 succeeds, anything else (or the timeout)
fails the step with the end of the output. A `preview` step starts the project's dev server through the preview mechanism
(`.har/project.yaml`) and records the address in the step (`url`); a project that cannot show a preview just runs the
step's command.

Policy still applies. The step's `policy` (or the factory default) is applied to the command as to an agent's
`execute_command`: *supervised* asks a person first (the step waits, shows in the inbox like a gate with the command, and is
approved or sent back by a project owner or an approver), secret reads, destructive commands and protected paths are
refused, and the project's deny and ask rules apply. Without a sandbox backend the step falls back to a factory task
whose agent runs the command and reports an `EXIT_CODE:` line.

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

The sandboxes of command steps are filed in the server's memory: after a restart a step that was running starts again in a
new sandbox (the repository is checked out again). Steps that share a repository's branch each open (or update) the same
pull request, so CI and the reviewer judge it once per step that uses that repository.
