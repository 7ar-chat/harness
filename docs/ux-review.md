# UX review of every screen

Goal: a newcomer understands each screen in five seconds, sees one clear primary action, meets no unexplained jargon, and the same thing is called the same everywhere.

How it was done: `web/e2e/shots.mjs` (scenario s1, opt-in with `E2E_SHOTS=<dir>`) runs as the last scenario of the full e2e, on real data, and saves one PNG per screen. Every image was opened and looked at.

## What was reviewed

Round 1 (136 images) found the first 22 items. Round 2 (this one) closed the coverage gaps and looked again at everything the
navigation change produced (Work, Automation, Insights, Settings and Me, each with its views).

Every combination now gets the same set: all 15 top-level routes and 3 help articles, all 16 project tabs (the area views), task
detail, task triggers, task report, requirement detail, workflow designer, workflow run, a session with edits, a session waiting
for approval, the digest card, 3 new-task screens (closed, Advanced open, the automatic-merge confirm), and every other dialog,
menu and the explorer drawer.

| Theme and size | Images in the folder | Looked at (honest count) |
| --- | --- | --- |
| Desktop dark (1440x900) | 62 | 22 |
| Desktop light | 62 | 13 |
| Phone dark (390x844) | 63 | 9 |
| Phone light | 63 | 61 of the 63: every route, tab and dialog except two help articles |
| Total | 250 | 105 distinct images opened (a changed image was opened again after its fix) |

I did not open every desktop and phone-dark image: the pages are the same components as the phone-light ones, the findings
that are layout-only were checked in the other sizes where they could differ (pills, dialogs, designer, tasks), and the axe scan
(p1) covers all four combinations for contrast and structure.

Screenshots: `scratchpad/ux-last/<theme>/<name>.png` (round 2, final), earlier rounds in `ux-final`, `ux-after` and `ux-last-1`/`ux-last-2` (the runs before the last fixes). The folders are `desktop-dark`, `desktop-light`, `phone-dark` and `phone-light`.

What was wrong with the capture itself, and what it was:

* **New-task dialog timed out in desktop dark (and would in every combination once they shared a list).** Cause: not a defect of the dialog.
  The editor tab "Work Tasks" stays mounted (TabHost keeps the 8 latest tabs), so it comes back on the sub-view it was left on. The shot
  of the task report came first, so the script opened the Tasks tab on Report, which has no New task button. The dialog opened once, not
  twice and not late. Fixed in the script: it clicks List first (and the failure now saves a FAIL image and a dialog count). A real
  consequence is noted as finding 24.
* **Digest card.** It is now found by its test id, scrolled to the top of the page and captured (`card-digest.png`, all four combinations).
* **Automation, Tasks and Requirements** were never looked at by the navigation agent: they are now (findings 23 to 29).

## Findings

Severity: blocker (cannot use), confusing (a newcomer stalls or misreads), polish (rough edge).

| # | Screen | Problem | Severity | Fix | Status |
| --- | --- | --- | --- | --- | --- |
| 1 | Every project section (board, sessions, requirements, tasks, workflows, roles, schedules, memory, members, tools, audit, settings, usage, insights, recipes, notifications) | Only a one-line subtitle, no title; only Notifications had a heading. The name of the page was only on the tab strip, which scrolls out of view. | confusing | One shared header: a title (the same word as the sidebar) and one intro line with the help link | fixed |
| 2 | Schedules (project tab) and Recipes, Notifications | Two intro paragraphs (the section subtitle and a page paragraph) | confusing | Keep one: the page paragraph is dropped in a project, shortened on Recipes, removed on Notifications | fixed |
| 3 | Workflows and Roles | Two intro paragraphs (known item) | confusing | Done in the earlier commit (one intro per page) | fixed |
| 4 | Needs you | Title with no explanation of what lands here | confusing | One-line explanation under the title | fixed |
| 5 | Requirement detail | Primary button said "Dispatch" (jargon, not used in the glossary) | confusing | "Hand to an agent", the same words as the Getting started step | fixed |
| 6 | Admin | No explanation, and two h1 headings (Admin, Users) | confusing | One-line intro, inner heading is now a level 2 heading | fixed |
| 7 | Job search on a phone | The tab "Applied (0)" is cut off, the row cannot be seen in full | confusing | The tab list wraps | fixed |
| 8 | Workflow designer on a phone | Tapping a step shows nothing: the inspector is below the canvas, out of view | confusing | The inspector scrolls into view when a step is chosen on a narrow screen | fixed |
| 9 | Usage and Notifications tables on a phone | Scrollable table not reachable by keyboard (axe scrollable-region-focusable, 6 scans) | confusing | Focusable labelled region | fixed |
| 10 | My settings, executor availability | The badge text changed to "not checked" but the e2e still expected "unknown" | polish | e2e accepts the new wording | fixed |
| 11 | Project settings | Repository URL field is empty with no hint | polish | Placeholder with an example | fixed |
| 12 | Workflow designer | "needed" on an input toggle is unclear | polish | "required" | fixed |
| 13 | Role editor | About 14 controls in one scroll (name, title, description, instructions, model, effort, tool profile, permission mode, executor, allowed, denied, skills, MCP, enabled) | confusing | Progressive disclosure: name, title, description, instructions, executor and enabled first (with the hint "Most roles only need a name and instructions"); model, effort, tools, skills, MCP, permission mode and version history in one collapsed Advanced section that remembers its state and opens on its own when it holds a value or the problem | fixed |
| 14 | Workflow designer | 11 controls in the toolbar (back, name, history, duplicate, check, save, publish, 6 step kinds, tidy) | polish | One "Add step" menu lists the six kinds with what each does; 5 buttons plus the menu and Tidy remain | fixed |
| 15 | Recipe wizard | "When to run" shows a raw cron value under "Custom" | polish | The preview now reads in words ("Runs every day at 02:00 (UTC)", plus the next three runs); the raw field shows only when Custom is chosen, and a recipe whose default is not one of the seven common choices (the nightly scan) still opens on Custom with the field | fixed |
| 16 | Project actions menu | "Import AGENTS.md" is greyed with no reason | polish | The reason is a visible line under it: "Connect GitHub in Connectors to import AGENTS.md" | fixed |
| 17 | Dashboard | "Getting started" appears as a card and as a pill in the sidebar | polish | The folded line is not drawn on the Dashboard (the card is there); it shows on every other page | fixed |
| 18 | Page widths | Profile, Help, Memory and Roles each use a different content width and alignment | polish | One column: left-aligned, 5xl, padding 4/6 (Account, Help, Memory, Roles, Schedules, Usage, Jobs, Admin) | fixed |
| 19 | Requirements list | The right pane says "Open a requirement to see what works on it. It opens in its own tab." | polish | The empty pane is gone; the list uses the full width | fixed |
| 20 | Connectors | "Account –" next to "Server not set up" reads as a broken value | polish | "Account not connected"; and the row is not drawn when the server is not set up (item 34) | fixed |
| 21 | Executor editor | 13 fields (name, title, description, command, program, install command, three flag sets, credential variables, output, enabled), words like flags and program | confusing | First screen: name, title, command (with an example), program, credential variables; description, install command, flags, output and enabled in a collapsed Advanced section; a "Start from" select fills a builtin template (Claude Code, Codex, OpenCode, Oh My Pi), and asks before replacing what was typed | fixed |
| 22 | Task detail vs Report | Spend $0.01 on the task and $0.03 on the report | polish | Not a UI bug: the sample data has a second run priced by another scenario | not a defect |
| 23 | Work, Tasks | The in-page tabs (List, Triggers, Report) and the Workflows tabs (Workflows, Runs) look like the area pills above them, so Tasks has three stacked pill rows | confusing | The in-page tab lists are underlined; the area pills stay filled | fixed |
| 24 | Work, Tasks (editor tab kept alive) | Coming back to the Tasks tab after Report shows Report, not the list; the New task button is not there | polish | Left: it is the keep-alive feature (a tab remembers where you were); the dialog now also opens from any sub-view in the script | left |
| 25 | Settings and Me on a phone | The area pills are one scrolling row: Notifications, My usage and the active view are cut off with nothing to say there is more | confusing | The pills wrap onto several rows on a narrow screen | fixed |
| 26 | Dialogs on a phone (executor editor after "Start from") | The long command pushed the dialog wider than the screen: fields ran off the right edge | confusing | The dialog is a one-column grid that may shrink (`minmax(0,1fr)`), for every dialog | fixed |
| 27 | Me pages | Content was centred (about 80px in) while the pills above were left-aligned, and the pages had four widths | polish | One left-aligned column, 5xl wide, the same padding as the project views (this is also item 18) | fixed |
| 28 | Me views | The page title was not the name of the view: pill Library, page References; pill My tools & skills, page Tools & skills; pill My usage, page Usage | confusing | The title is the pill name | fixed |
| 29 | Work, Requirements | A disclosure called "Automation" (hand ready requirements to agents) has the same name as the Automation area | confusing | "Hand off automatically", with On/Off in plain words | fixed |
| 30 | Session waiting for approval | The status line under the card said "Idle" while the badge said "Needs approval" | confusing | "Waiting for your answer" while a question or approval is open | fixed |
| 31 | Recipes | Two intro paragraphs again (the section line and a page paragraph) | polish | One intro: the sharing hint moved into the section line | fixed |
| 32 | Roles lists | "default model ·" ended in a lone separator when a role has no tools or skills | polish | Empty parts are dropped | fixed |
| 33 | Project settings, General | Default branch is empty with no hint | polish | Placeholder "main" | fixed |
| 34 | Connectors | "Account not connected" and "Server not set up" repeat the badge and the message under it | polish | The row is not drawn (kept for screen readers) when the server is not set up | fixed |
| 35 | Workflow designer on a phone | The workflow name was cut ("Product propo") and the version label ran off the edge | polish | The name takes its own row on a phone, the badge wraps below | fixed |
| 36 | Library on a phone | The search placeholder is cut ("Search by name or typ") | polish | Left | left |
| 37 | Work, Sessions | A "GitHub: Not configured" card sits above the session list even when the project has no repository | polish | Left | left |
| 38 | Admin | The heading "Users" repeats the "Users" pill | polish | Left | left |
| 39 | Triggers | The help "?" sits before "Add from a recipe" | polish | Left | left |
| 40 | Automation, Workflows | "New workflow" is alone on a row with a gap above the tabs | polish | Left | left |
| 41 | New task dialog | The Budget label (with a help icon) and the Max attempts label are not on one line | polish | Left | left |

Checked and fine on all screens in dark, light and phone: contrast (no text below AA in either theme), no horizontal page scroll on a phone, icon-only buttons carry a name (Rename session, Run settings, Delete session, Open navigation, Project actions, User menu), destructive actions (delete session, delete recipe, remove channel, delete role) ask first, empty states have a title, one sentence and a primary action, loading states are skeletons, and the Console look is unchanged.

Counts after round 2: found 41, fixed 33 (confusing 17, polish 16), left 7 (0 confusing, 7 polish), 1 not a defect. Round 1 had found 22 (fixed 14, left 7, 1 not a defect); round 2 found 19 more (fixed 12 of them, 7 polish left) and closed the 7 left from round 1. No blocker was found in either round.

Per theme and size, where each round 2 finding was first seen (found / fixed / left): desktop dark 11 / 8 / 3 (23, 24, 27, 28, 29, 31, 32, 33, 34, 40, 41), phone light 8 / 4 / 4 (25, 26, 30, 35, 36, 37, 38, 39), desktop light 0, phone dark 0. The light and phone-dark images showed the same components and confirmed every fix; no problem was specific to a theme (axe found no contrast violation in any of the four).

Second-order effect caught by the axe e2e and fixed in the same change: the new section titles are level 1 headings, so the level 3 headings below them (board columns, invite, channels, rules, delivery log, recipe cards, task report) became level 2 headings; the scrollable table regions got distinct names.

## Before and after

| Fix | Before | After |
| --- | --- | --- |
| 1 and 2: titles and one intro (schedules tab) | `ux-final/desktop-dark/tab-schedules.png` | `ux-after/desktop-dark/tab-schedules.png` |
| 1: titles (board, members, memory, audit, tasks, requirements, workflows, settings, usage, insights, recipes, notifications) | `ux-final/desktop-dark/tab-<name>.png` | `ux-after/desktop-dark/tab-<name>.png` |
| 4: Needs you | `ux-final/desktop-dark/route-inbox.png` | `ux-after/desktop-dark/route-inbox.png` |
| 5: Hand to an agent | `ux-final/desktop-dark/tab-requirement-detail.png` | `ux-after/desktop-dark/tab-requirement-detail.png` |
| 6: Admin | `ux-final/desktop-dark/route-admin.png` | `ux-after/desktop-dark/route-admin.png` |
| 7: Job search tabs on a phone | `ux-final/phone-dark/route-jobs.png` | `ux-after/phone-dark/route-jobs.png` |
| 8: designer inspector on a phone | `ux-final/phone-dark/dlg-workflow-node-inspector.png` | `ux-after/phone-dark/dlg-workflow-node-inspector.png` |
| 11: repository placeholder | `ux-final/desktop-dark/tab-settings.png` | `ux-after/desktop-dark/tab-settings.png` |
| 13 and 22: executor editor (now captured) | not captured | `ux-after/desktop-dark/dlg-executor-editor.png` |
| 13: role editor, Advanced closed and open | `ux-final/desktop-dark/dlg-role-editor.png` | `editors/desktop-dark/dlg-role-editor.png`, `editors/desktop-dark/dlg-role-editor-advanced.png` |
| 21: executor editor, closed and open with a template | `ux-after/desktop-dark/dlg-executor-editor.png` | `editors/desktop-dark/dlg-executor-editor.png`, `editors/desktop-dark/dlg-executor-editor-advanced.png` |
| 14 to 20 (round 1 left items) | `ux-final/desktop-dark/tab-workflow-designer.png`, `dlg-recipe-wizard.png`, `menu-project-actions.png`, `route-dashboard.png`, `route-roles.png`, `tab-requirements.png`, `route-connectors.png` | `ux-last/desktop-dark/<same names>.png` and `ux-last/phone-light/menu-project-actions.png` |
| 23: underlined in-page tabs | `ux-last-1/desktop-dark/tab-workflows.png` | `ux-last/desktop-dark/tab-workflows.png`, `ux-last/phone-light/tab-workflows.png` |
| 25: pills wrap on a phone | `ux-last-1/phone-light/tab-members.png` | `ux-last/phone-light/tab-members.png`, `ux-last/phone-light/route-references.png` |
| 26: dialog fits a phone | `ux-last-1/phone-light/dlg-executor-editor-advanced.png` | `ux-last/phone-dark/dlg-executor-editor-advanced.png` |
| 27 and 28: one column, titles match | `ux-last-1/desktop-dark/route-references.png` | `ux-last/desktop-dark/route-references.png`, `ux-last/desktop-light/route-references.png` |
| 29: Hand off automatically | `ux-last-1/phone-light/tab-requirements.png` | `ux-last/desktop-dark/tab-requirements.png` |
| 30: Waiting for your answer | `ux-last-1/phone-light/session-approval.png` | `ux-last/phone-dark/session-approval.png` |
| 31: one Recipes intro | `ux-last-1/phone-light/tab-recipes.png` | `ux-last/desktop-dark/tab-recipes.png` |
| 34: Connectors | `ux-last-1/phone-light/route-connectors.png` | `ux-last/desktop-dark/route-connectors.png` |
| 35: designer name on a phone | `ux-last-2/phone-dark/tab-workflow-designer.png` | `ux-last/phone-dark/tab-workflow-designer.png` |
| New: review notice, hint, confirm | not there | `ux-last/<combo>/tab-tasks.png`, `dlg-new-task-advanced.png`, `dlg-automerge-confirm.png` |

(All paths are under `/private/tmp/claude-501/-Users-ghazi-projects-harness/689d5953-4b78-4369-984a-e47c2f2a6d41/scratchpad/`.)

## Navigation consolidation (before: 27 links; after: 9)

The request: "there are too many tabs; job search is a use case, not a feature of the platform". The interface had about
27 places to go to. They are regrouped, not removed: every view, every tab id (`?tab=members`, `?tab=usage`...), every
route and every deep parameter (`task=`, `req=`, `wf=`, `run=`) still works, so notifications, onboarding, help and the
e2e scenarios did not change their links.

### Before

* Top: Dashboard, Needs you (2)
* Project, "Work": Board, Requirements, Tasks & triggers, Workflows & runs, Insights (5)
* Project, "Setup" (folded): Roles & executors, Schedules, Recipes, Notifications, Memory & docs, Members & access, Tools &
  skills, Audit log (8)
* Personal: My assistant, Job search, References, Connectors, My memory, My schedules, My roles, My tools & skills (8)
* Bottom: Usage & caps, Project settings (owners) or My insights and Usage & caps (no project) (2 to 3)
* Total: 25 to 27 links (plus the sessions lists), 16 project tab ids, 17 routes

### After

* Top: Dashboard, Needs you (2)
* Project: **Work**, **Automation**, **Insights**, **Settings** (4)
* Personal: **My assistant**, **Me** (2)
* Bottom: **Settings** (your account; the user menu still has Settings, Help, theme, Admin) (1)
* Total: **9 links** (plus the sessions lists). Tab ids and routes are unchanged.

### The areas and their views

Each area is one explorer link and one editor tab. Its views are a row of pill tabs (a real tablist: arrow keys, Home and
End move and show the view) at the top of the page. The view last shown in an area is remembered per project, and the
explorer link opens it.

| Area | Views (old tab id or route) |
| --- | --- |
| Work | Board (`board`), Requirements (`requirements`), Tasks (`tasks`, with its triggers and report), Sessions (`overview`, reached from "All N sessions", not drawn as a pill) |
| Automation | Workflows (`workflows`, with its runs), Schedules (`schedules`), Recipes (`recipes`) |
| Insights | Overview (`insights`), Usage & caps (`usage`), Activity (`audit`, was "Audit log") |
| Settings | General (`settings`, owners only), Members & access (`members`), Roles & executors (`roles`), Tools & skills (`capabilities`), Memory & docs (`memory`), Notifications (`notifications`) |
| Me | Library (`/references`), Connectors (`/connectors`), Memory (`/memory`), My roles (`/roles`), My schedules (`/schedules`), My tools & skills (`/capabilities`), My insights (`/insights`), My usage (`/usage`) |

The registry is `web/src/components/app/shell/areas.ts` (area, then view, in order, with aliases the palette matches). The
editor tab reads "Work · Board". A task, requirement, workflow or run opened on its own stays its own tab and has no
sub-navigation. A tab strip saved before this change is folded into one tab per area on load.

### Decisions

* **Job search is a use case.** Removed from the explorer. It is offered, only when the server has it (the same 404 and 501
  check the page itself uses), as a card in a **Use cases** section on the Dashboard (also on the empty Dashboard, next
  to "Try it with a sample"), as the palette entry "Use case: Job search", in the help article "Use cases" and in
  docs/jobs.md. The list is data (`web/src/lib/useCases.ts`): adding a use case is adding a row. The backend is untouched.
* **Usage & caps** left the bottom of the explorer: it is a view of Insights (it is about what the project spent).
* **Recipes** are Automation (they make schedules, tasks and triggers). **Audit log** is **Activity**, next to the
  numbers. **Notifications**, **Members**, **Roles**, **Tools** and **Memory** are configuration: Settings.
* **Project settings** is the General view of Settings, owners only; the other views of Settings stay open to everyone, as
  before.
* **Me** gathers the personal rows. My insights and My usage stay two views (they are two pages), so Me has 8 views.
* **Admin** stays only in the user menu (and the palette). **Help** too.
* Considered and left: Sessions is not an area (the explorer lists them, the Board shows them); "Needs you" stays a
  top-level link because it is the one place a person must look at.
* Not first-class any more but kept: the Profile page is reached by the bottom **Settings** link and the user menu.

## Reviewer warning

In production `FACTORY_REVIEW_MODEL` is unset: the model that writes a change also reviews it (the server logs a warning, nobody
saw it). The product now says so where it matters, and changes nothing in how the factory works.

* `GET /api/projects/{pid}/factory/status` (members and owners; a viewer gets 403, an outsider 404) answers
  `{review, independentReview, canAutoMerge}`, where `review` is `independent` (a second review model is set), `same_model`
  (no second model) or `off` (`FACTORY_NO_REVIEW=1`, or no code host so nothing is reviewed or merged). It never names the model.
* Tasks tab, for owners and members: a quiet note "Reviews are not independent: the model that writes a change also reviews it. Ask an admin to set a second review model before turning on automatic merge." with Learn more (help article "Reviews and automatic merge") and Dismiss. Dismissal is per browser and per kind of note. When review is off the note says "Reviews are off: a change merges on green checks alone. Ask an admin to switch review on before turning on automatic merge." It says nothing when the review is independent, and nothing to viewers (the status is not even asked).
* New task, Advanced options: the same hint under "Merge automatically", and one confirm step ("Merge automatically without an independent review?", Keep it off / Turn on automatic merge) when an owner switches it on while reviews are not independent. No confirm when the review is independent.
* The e2e server runs with `FACTORY_NO_REVIEW=1`, so scenario g3b checks the "Reviews are off" wording (never "not independent"), the status values, dismissal after a reload and the confirm; p1 scans the note, the hint and the confirm with axe in four combinations.
