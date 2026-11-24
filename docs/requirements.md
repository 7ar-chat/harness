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
GET    /api/projects/{pid}/requirements/tapd         viewer+  {connected, workspaceId, baseUrl, user, hasPassword, hasToken, openStatuses, storyUrl, statusAliases, available}
PUT    /api/projects/{pid}/requirements/tapd         owner    {workspaceId, baseUrl?, user?, password?, token?, openStatuses?, storyUrl?, statusAliases?}: checks the credentials against TAPD, then saves them sealed
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

### What was confirmed and what is configurable

Checked against the official documentation in November 2026. The pages (all under
`https://open.tapd.cn/document/api-doc/API%E6%96%87%E6%A1%A3/api_reference/`):

- Get stories, `story/get_stories.html`: `GET https://api.tapd.cn/stories` with `workspace_id` (required), `limit`
  (default 30, max 200), `page` (from 1), `order`, `fields`; the answer is
  `{"status":1,"data":[{"Story":{...}}],"info":"success"}`. Story has `id`, `name`, `status`, `description`, `owner`,
  `creator`, `priority`, `priority_label`... The `status` in the example is the English name (`"status": "planning"`);
  `with_v_status=1` returns the Chinese label instead and `v_status` filters by a Chinese name. The response has no
  story link field.
- Workflow end statuses, `workflow/get_workflow_last_steps.html`: `GET https://api.tapd.cn/workflows/last_steps?system=story&workspace_id=`
  (`system` is `bug` or `story`; `workitem_type_id` optional: without it the end statuses of every workflow of the
  project) answers `{"status":1,"data":{"closed":"已关闭"}}`: the keys are the English status names, the values the
  Chinese labels.
- Status names, `workflow/get_workflow_status_map.html`: `GET https://api.tapd.cn/workflows/status_map` returns the
  same key/value shape for all statuses (story example: `planning` to `规划中`, `developing`, `resolved`, `rejected`);
  for stories it wants `workitem_type_id`.
- Authentication: HTTP Basic `api_user:api_password` (the `curl -u` examples), or an OAuth access token.

Settled from that:

- Default host: `https://api.tapd.cn`. Every open.tapd.cn page above uses it. `https://apiv2.tapd.tencent.com` is the
  host the Tencent-hosted copy of the documentation (o.tapd.tencent.com, "使用必读") names; an account on that
  deployment sets it as the base URL of the connection.
- Status format: stories carry the English status name, and the keys of `last_steps` are English names too, so
  comparing them (case-insensitively) is correct. The Chinese labels are never needed, unless a person types one.

Not stated by the documentation, so configurable on the connection instead of assumed (both optional, kept sealed with
the connection and shown in the Connect TAPD form):

- Story link pattern (`storyUrl`): the API returns no link, so one is built from a template. Default
  `https://www.tapd.cn/{workspace}/prong/stories/view/{id}` (known from the web app, not documented; on a private base
  URL the host of that URL replaces `www.tapd.cn`). `{id}` is required, `{workspace}` optional; https only, no
  credentials, validated on save.
- Status aliases (`statusAliases`): `name=open` or `name=closed`, comma separated, for workflows whose status names
  differ from the ones the end-status endpoint reports. They win over the workflow's end statuses and over the list of
  open statuses. Names are compared case-insensitively.

Still assumed: the separator of an enumerated `status` filter (the import never filters on the server: it reads pages
and decides openness itself); that an error is the envelope with `status` other than 1, HTTP 401/403, or `status`
401/403 in the envelope; that `last_steps` may answer `[]` for an empty set (handled).

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
POST /api/projects/{pid}/digest/send {force?}      owner    → {digest}; a week with no activity answers 409 quiet_week unless force:true (the page asks "Send anyway?")
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
