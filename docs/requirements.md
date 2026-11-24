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
POST   /api/projects/{pid}/requirements/import/issues         member  {label?}: the same from the repository's own host: GitHub, or a registered GitLab / Gitea (the caller's token); source kinds github_issue, gitlab_issue, gitea_issue
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

## Importing from TAPD

TAPD (Tencent's project tool) is a requirements source next to the git hosts. The project's owner connects one TAPD
workspace; members import its open stories.

```
GET    /api/projects/{pid}/requirements/tapd         viewer+  {connected, workspaceId, baseUrl, user, hasPassword, hasToken, openStatuses, available}
PUT    /api/projects/{pid}/requirements/tapd         owner    {workspaceId, baseUrl?, user?, password?, token?, openStatuses?}: checks the credentials against TAPD, then saves them sealed
DELETE /api/projects/{pid}/requirements/tapd         owner
POST   /api/projects/{pid}/requirements/import/tapd  member   → {imported, skipped, requirements, source: workflow|filter, truncated}
```

The credentials are sealed with the server key (`HARNESS_SECRET_KEY`, domain `harness-tapd:`), stored per project in
`core/projdata` (table `work_project_secrets`, migration 1400) and never returned or logged; a blank password on save
keeps the stored one. The base URL is for private TAPD deployments: https only, and a loopback, private, link-local or
metadata address (or a name that resolves to one) is refused by the guarded transport of `core/netguard`.
`HARNESS_TAPD_ALLOW_PRIVATE=1` lets the operator point it at an intranet address (the e2e test does).

**Mapping.** name becomes the title; description (HTML) becomes plain text (tags dropped, never interpreted; script and
style content removed; `<br>`, block ends and list items become line breaks and `- `; entities decoded); `priority_label`
(else `priority`) becomes the priority (urgent 4, high 3, middle 2, low 1) and a `priority:<label>` label; owner,
creator and the TAPD status go in a footer of the body; every imported story is a `backlog` requirement. The source is
`tapd_story` with `repo` = the workspace id and `id` = the story id, so the dedupe key is `tapd:{workspace}:{id}`; the
import is repeatable and skips known stories. One call reads at most 2000 stories (200 per page) and imports at most
500, newest first; `truncated` says more were left.

**What "open" means.** A story is open when its `status` is not one of the end statuses of the project's story
workflows, which `GET /workflows/last_steps?system=story&workspace_id=` answers. If the owner entered a list of
open statuses ("planning, developing": the English status names), that list wins and the workflow endpoint is not
called. If the endpoint is unavailable for the account and no list is set, the import answers 409
`tapd_no_statuses` and the page tells the owner to enter a list; nothing is guessed. The page says, after each
import, which of the two decided.

### What was confirmed and what is assumed

Confirmed from the official documentation (checked November 2026):

- `GET https://api.tapd.cn/stories` with `workspace_id` (required), `limit` (default 30, max 200), `page` (from 1),
  `order`, `fields`; the answer is `{"status":1,"data":[{"Story":{...}}],"info":"success"}`; Story has `id`, `name`,
  `status`, `description`, `owner`, `creator`, `priority`, `priority_label`, `workitem_type_id`...
  (open.tapd.cn/document/api-doc/API文档/api_reference/story/get_stories.html)
- Authentication: HTTP Basic `api_user:api_password`, or an OAuth `Authorization: Bearer <access_token>`
  (same page, and the workflow pages below).
- `GET /workflows/last_steps?system=story&workspace_id=` returns `{"status":1,"data":{"<english status>":"<name>"}}`,
  the end statuses of all story workflows of the project when `workitem_type_id` is omitted; `GET /workflows/status_map`
  returns English to Chinese status names (o.tapd.tencent.com/document/api-doc/API文档/api_reference/workflow/get_workflow_last_steps.html).
  The older docs host these on `apiv2.tapd.tencent.com`; the open.tapd.cn pages use `api.tapd.cn`, which is the default.

Assumed (the documentation does not say):

- The separator of an enumerated `status` filter. The import therefore never filters on the server: it reads pages and
  decides openness itself from each story's `status`.
- The story page address `https://www.tapd.cn/{workspace}/prong/stories/view/{id}` (known from TAPD's web app, not
  stated in the API documentation). For a private deployment the host of the base URL is used instead of `www.tapd.cn`.
- That the status values of a story are the English names that `last_steps` lists (`resolved`, `status_3`...), compared
  case-insensitively.
- That an error is the envelope with `status` other than 1, or HTTP 401 and 403, or `status` 401 and 403 in the
  envelope, for bad credentials.
- That `last_steps` may answer `[]` for an empty set (handled).

## Weekly digest

A project's owner can turn on a weekly digest (Insights tab, "Weekly digest"): the day, the hour and the time zone. At
that time the project's insights report of the last seven days (the same page as the exportable HTML report) is
generated by a job, stored (table `work_digests`, migration 1400, the last 26 per project) so its link keeps working,
and announced as the notification event `digest.ready` through the notification engine: to the channels the project
chose for it (Notifications tab), and to each member where their own preferences say (the app by default).
A week without activity (nothing delivered or spent) is skipped. "Send one now" generates a digest on request, even for
an empty week (rate limited to one every 3 seconds).

```
GET  /api/projects/{pid}/digest                     viewer+  {setting:{enabled,weekday,hour,timezone,nextAt,lastSentAt}, digests:[{id,createdAt,from,to,summary,trigger,url}], canEdit, notified}
PUT  /api/projects/{pid}/digest                     owner    {enabled, weekday 0-6 (Sunday 0), hour 0-23, timezone (IANA)}
POST /api/projects/{pid}/digest/send                owner    → {digest}
GET  /api/projects/{pid}/digest/{id}/report[?download=1]   viewer+: the stored page (no script, CSP default-src 'none')
```

**Replica safety.** Every replica runs the job once a minute. A digest is generated only by the replica that wins
`ClaimSlot(project, slot)`, one `INSERT ... ON CONFLICT DO NOTHING` on `(project_id, slot)` where the slot is the local
date of the due time; a failed generation releases the claim so the next tick retries. A slot is sent when due, within
12 hours after its time (a server that was down catches up; a later start does not send a stale digest), and never for a
time before the digest was switched on or reconfigured.

**Time.** The slot is the most recent time at or before now that falls on the chosen weekday and hour in the chosen zone,
computed in the local calendar, so daylight saving changes move the instant, not the date: an hour that does not exist
that day resolves to the instant Go gives it, an hour that happens twice is one instant, and a week has one slot either
way. The range is the seven calendar days before it (169 or 167 hours across a clock change).
