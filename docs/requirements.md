# Requirements tracking

A project keeps a list of **requirements**: what it wants done, followed from the idea to the merged pull request
(requirement, then the session or factory task that works on it, then the PR, then done). Team mode only; the routes
are served when `product.Options.Requirements` is set (`harness-web` always sets it in team mode).

| Piece | Where |
|---|---|
| vocabulary and the `Store` port | `core/requirements` |
| mem, file (`requirements.json` beside `team.json`) and Postgres stores (migration `0590_requirements.sql`) | `adapters/store/reqstore` |
| routes, dispatch, completion by merge, the agent's context | `app/product/requirements.go` |
| the page | `web/src/pages/Requirements.tsx`, project tab `?tab=requirements` |

A requirement has a title, a markdown body, a status (`backlog`, `ready`, `in_progress`, `in_review`, `done`,
`rejected`), a priority, labels, a source (manual, or a GitHub issue: repo, number, url), an author, the sessions and
tasks linked to it, its pull request (number, url, state) and a flag (why an in-progress requirement needs a human).

```
GET    /api/projects/{pid}/requirements?status=&label=&q=
POST   /api/projects/{pid}/requirements                       member
GET    /api/projects/{pid}/requirements/{rid}                 requirement + linked tasks and sessions
PATCH  /api/projects/{pid}/requirements/{rid}                 member
DELETE /api/projects/{pid}/requirements/{rid}                 owner
POST   /api/projects/{pid}/requirements/{rid}/dispatch        member  {policy?, budget?}
POST   /api/projects/{pid}/requirements/import/github         member  {label?}: open issues, deduped by issue number
GET|PUT /api/projects/{pid}/requirements/settings             PUT: owner  {autoDispatch, policy?, budget?}
POST   /api/threads/{id}/requirement                          {requirementId}: links the conversation
```

**Dispatch** creates a factory task from the requirement (the project's repository, the given or default policy
and budget) when the factory is on, otherwise starts a project session whose first message is the requirement, and
links what it started. A requirement that is already in progress is refused (409) unless it is flagged.

**Completion by merge** reuses the factory's `OnTaskState` hook, nothing polls: a task started is `in_progress`, a
pull request opened is `in_review` (with the PR), a task done (the PR merged, detected by the PR loop) is `done`
with the PR `merged`. A task that fails, blocks or is cancelled leaves the status and sets the flag. A requirement
is found by its task, or by the session a task is (a session that opens a pull request becomes a task on its thread).
`done` and `rejected` are not reopened by a task.

**Auto-dispatch** is a project setting, off by default. Turned on (owner), a requirement moved (or created) to `ready`
is dispatched at once with the setting's policy and budget. It needs the factory and a budget (a dollar or token
cap); a refusal or failure is written on the requirement as its flag. It never allows automatic merging.

**The agent's context**: a conversation linked to a requirement (`POST /threads/{id}/requirement`, or started by
dispatch) gets a short "Linked requirement" block in each turn's context with the id, title, status and pull
request, so it can name the requirement in its PR. The requirement's text is not put in the prompt by this block.
