# UX review of every screen

Goal: a newcomer understands each screen in five seconds, sees one clear primary action, meets no unexplained jargon, and the same thing is called the same everywhere.

How it was done: `web/e2e/shots.mjs` (scenario s1, opt-in with `E2E_SHOTS=<dir>`) runs as the last scenario of the full e2e, on real data, and saves one PNG per screen. Every image was opened and looked at.

## What was reviewed

| Theme | Images | Contents |
| --- | --- | --- |
| Desktop dark (1440x900) | 56 | all 15 top-level routes and 3 help articles, 16 project tabs plus task detail, task triggers, task report, requirement detail, workflow designer, workflow run, a session with edits, a session waiting for approval, and 14 dialogs, menus and cards |
| Desktop light | 37 | the same screens that carry the most layout, plus every dialog |
| Phone dark (390x844) | 38 | key routes and tabs, sessions, every dialog, explorer, welcome and tour |
| Phone light | 5 | dashboard, board, tasks, help, approval |
| Total | 136 | |

After the fixes the whole set was shot again (139 images) and the changed screens were looked at once more (see Before and after).

Not captured in the first run (listed in `missed.txt`): the executor editor (the script looked on the wrong page; fixed, and looked at in the second run) and the new-task dialog in dark desktop (a timing issue, captured in light and phone). The digest card is shown scrolled to the top of Insights, not centred on the card.

Screenshots: before `scratchpad/ux-final/<theme>/<name>.png`, after `scratchpad/ux-after/<theme>/<name>.png`, where the folders are `desktop-dark`, `desktop-light`, `phone-dark` and `phone-light`.

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
| 13 | Role editor | About 14 controls in one scroll (name, title, description, instructions, model, effort, tool profile, permission mode, executor, allowed, denied, skills, MCP, enabled) | confusing | Left: every field is used by the e2e and the sample flow; a split into an "Advanced" group is the next step | left |
| 14 | Workflow designer | 11 controls in the toolbar (back, name, history, duplicate, check, save, publish, 6 step kinds, tidy) | polish | Left: a designer is a tool screen; the primary action (Publish) is the only filled button | left |
| 15 | Recipe wizard | "When to run" shows a raw cron value under "Custom" | polish | Left: the preview line below it ("Every night at 02:00" and the next three runs) explains it | left |
| 16 | Project actions menu | "Import AGENTS.md" is greyed with no reason | polish | Left (greyed when the project has no repository); a title tooltip would help | left |
| 17 | Dashboard | "Getting started" appears as a card and as a pill in the sidebar | polish | Left: the pill is the collapsed form, it keeps progress visible while working | left |
| 18 | Page widths | Profile, Help, Memory and Roles each use a different content width and alignment | polish | Left: consistent per page, would need a layout pass | left |
| 19 | Requirements list | The right pane says "Open a requirement to see what works on it. It opens in its own tab." | polish | Left | left |
| 20 | Connectors | "Account –" next to "Server not set up" reads as a broken value | polish | Left | left |
| 21 | Executor editor | 13 fields (name, title, description, command, program, install command, three flag sets, credential variables, output, enabled), words like flags and program | confusing | Left: it is an advanced, optional screen behind "New executor", the help text explains each field; grouping under Advanced is the next step | left |
| 22 | Task detail vs Report | Spend $0.01 on the task and $0.03 on the report | polish | Not a UI bug: the sample data has a second run priced by another scenario | not a defect |

Checked and fine on all screens in dark, light and phone: contrast (no text below AA in either theme), no horizontal page scroll on a phone, icon-only buttons carry a name (Rename session, Run settings, Delete session, Open navigation, Project actions, User menu), destructive actions (delete session, delete recipe, remove channel, delete role) ask first, empty states have a title, one sentence and a primary action, loading states are skeletons, and the Console look is unchanged.

Counts: found 22, fixed 12 (confusing 9, polish 3), left 9 (2 confusing, 7 polish), 1 not a defect. No blocker was found.

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
