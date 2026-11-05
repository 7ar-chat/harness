# Job search

The agent can look for work for a person and apply for them, around the clock, inside limits the person sets. It is
generic: nothing is specific to one person. The person's search is **data** (a profile), the finding is **plain HTTP**
(no model), and the judging and form filling are **the agent's**.

## A mode of its own

Job search is a **mode next to coding**, not a feature of it. Each user has one job-search conversation (the Jobs page,
"Talk to the job agent", or `POST /api/me/jobs/thread`; the hourly rounds run in it too). In that conversation, and only
there:

* the system prompt is the **jobs** prompt variant (`prompts.Jobs`): a job-application agent with the round routine, not the
  coding agent working in a repository;
* the `jobs_*` tools are offered, and the code explorer is not;
* the browser section drops its draft-then-ask flow (the routine and the profile's mode decide that).

Every other conversation keeps whatever `AGENT_PROFILE` says (production: `coding`) and is only told where the job search
lives. The mechanism is general: `Builder.RunVariant` lets any feature give a conversation its own prompt variant.

`HARNESS_JOBS_DIR` (default `/data/jobs` on the `harness-jobs` volume in production) turns the feature on. Empty: off.

## The person gives a CV, not filters

Nobody fills in a list. "Set up from my CV" (the Jobs page, which calls `POST /api/me/jobs/thread`) starts the first-time routine
in the job conversation: the agent reads the CV and website from the person's documents, **builds the profile itself**
(roles, seniority, skills, places), then asks once only what no document says (where they can work and visa needs, a
salary floor, who to avoid, queue or auto). It then **discovers** companies and boards on its own, every day, from that
profile (`jobs_track_company`) and widens or narrows the roles as it learns what fits.

What it learns is kept in an **answer bank** (`answers.json`, per user): `jobs_ask` records a question once (the same words
are the same question), `jobs_remember` saves what the person said in the chat, `jobs_answers` reads the bank before any
form. A question **never blocks the search**: the job is queued with what is missing, the agent goes on with the next one,
the person answers on the Jobs page (or in the conversation), and the next round resumes it (a fresh answer wakes the
agent; so does the daily discovery).

## The profile

One per user (the agent keeps it up; the Jobs page shows it, editable, under "What the agent learned"): roles, places, remote only, salary floor, work
authorisation, companies to watch, a blacklist (company names and domains), dealbreakers in free text, and how freely
the agent acts:

| | |
|---|---|
| `enabled` | the search runs by itself (below) |
| `mode` | `queue` (default): the agent prepares and a person approves every application. `auto`: it applies alone to jobs scored at least `minScore` (4.5) |
| `dailyCap` | applications in 24 hours (default 10, at most 50) |

Changing `enabled`, `mode`, `minScore` or `dailyCap` through the agent needs `userConfirmed`: the agent sets it only when the
person asked in the conversation.

## What runs by itself

Every `HARNESS_JOBS_SCAN_MINUTES` (60), for each user with `enabled`:

1. **Scan.** The public APIs of Greenhouse, Lever and Ashby are polled for the user's `companies`; postings whose title
   matches a role (word-start match, no excluded word), place and blacklist become `new` jobs. A job that left its board
   becomes `closed`. No model is called.
2. If there are `new` or `approved` jobs, an answer the person gave since the last round, or the daily discovery is due, the agent is started in the user's conversation "Job search" (at most 12 rounds a day; a round that finds the thread still busy waits for the next).

The agent scores each new job 1 to 5 (`jobs_update`): `skipped`, `queued` (waits for the person) or, in `auto` mode at or
above `minScore`, `approved`. For approved jobs it calls `jobs_claim`, fills the form in the browser from the user's documents,
submits, and records `jobs_applied` after the page shows a confirmation, or `jobs_release` when it could not finish.

## What the code enforces

`jobs_claim` (and nothing else) lets an application start, and refuses it when the job is not `approved`, the company is on the
blacklist, the person has not approved it and the score is below the auto score, the daily cap is used up, or the company was
applied to in the last 30 days (unless the person approved this job). A claim not finished in 30 minutes is released. Setting a
job to `approved` requires the person (`userApproved`, or the Apply button) or, in `auto` mode, the score.

**What it cannot enforce:** the browser tools are not gated by the claim. A model that ignores its instructions could still click Submit
on a form. The claim is the control for the well-behaved path and the audit trail (`jobs.json`); do not run `auto` mode with a
model you do not trust to follow instructions, and read the Applied list.

## Limits

* Only Greenhouse, Lever and Ashby boards are scanned. LinkedIn, Indeed and Workday need a login or are blocked for bots; the
  agent leaves those to the person.
* Forms that need an account, a CAPTCHA or a login are released to the person (`queued`) with the link.
* State is JSON files on one volume: one replica.

## API

`GET /api/me/jobs`, `PUT /api/me/jobs/profile`, `POST /api/me/jobs/boards`, `POST /api/me/jobs/scan`, `POST /api/me/jobs/questions/{id}` (answer), `DELETE /api/me/jobs/questions/{id}`, `PATCH /api/me/jobs/{id}`
(the person's decision: `approved`, `skipped`, `queued`, `interview`, `rejected`, `offer`, `withdrawn`).
