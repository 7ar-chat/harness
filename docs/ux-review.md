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

Not captured (the capture reported them as missed and they were listed in `missed.txt`): the executor editor and the new-task dialog in the first run (a selector and a timing issue in the script, fixed before the second run; the new-task dialog was captured in light and phone). The digest card is shown scrolled to the top of Insights, not centred on the card.

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
| 21 | Task detail vs Report | Spend $0.01 on the task and $0.03 on the report | polish | Not a UI bug: the sample data has a second run priced by another scenario | not a defect |

Checked and fine on all screens in dark, light and phone: contrast (no text below AA in either theme), no horizontal page scroll on a phone, icon-only buttons carry a name (Rename session, Run settings, Delete session, Open navigation, Project actions, User menu), destructive actions (delete session, delete recipe, remove channel, delete role) ask first, empty states have a title, one sentence and a primary action, loading states are skeletons, and the Console look is unchanged.

Counts: found 21, fixed 12 (confusing 9, polish 3), left 8 (1 confusing, 7 polish), 1 not a defect. No blocker was found.

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

(All paths are under `/private/tmp/claude-501/-Users-ghazi-projects-harness/689d5953-4b78-4369-984a-e47c2f2a6d41/scratchpad/`.)
