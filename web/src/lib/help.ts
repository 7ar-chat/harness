// The help center's articles: short, plain language, bundled with the app (no network). Bodies are Markdown.
export type HelpGroup = "Start here" | "Concepts" | "How to" | "Reference";

export interface Article {
  slug: string;
  title: string;
  group: HelpGroup;
  /** One line shown in the list. */
  summary: string;
  /** Extra words the search matches. */
  keywords?: string;
  body: string;
}

export const GROUPS: HelpGroup[] = ["Start here", "Concepts", "How to", "Reference"];

export const ARTICLES: Article[] = [
  {
    slug: "the-basics",
    title: "The four ideas to know",
    group: "Start here",
    summary: "Project, session, task and Needs you. That is all you need to begin.",
    keywords: "welcome onboarding getting started overview",
    body: `7ar lets AI agents work on your code while you stay in charge. Four ideas cover almost everything.

## Project
A project is a repository your agents work in. Its sessions, tasks, memory and teammates all live inside it. Start one from a template, or import a repository from GitHub.

## Session
A session is a conversation with an agent. You ask, it reads and edits files, runs commands and answers. You can watch it work, reply, or leave and come back.

## Task and requirement
A task is work you hand off: write what you want, and an agent does it on its own and opens a pull request for you to review. A requirement is a bigger goal you write down; agents break it into tasks.

## Needs you
When an agent wants to run something risky, asks a question, or finishes work to review, it shows up under **Needs you** (the bottom panel, or the Needs you page). Everything else can run without you.

**First steps:** create a project, start a session, then hand off a task.`,
  },
  {
    slug: "where-things-are",
    title: "Where things are",
    group: "Start here",
    summary: "Four areas for a project, one for you. Each has a few views.",
    keywords: "navigation menu explorer areas tabs views find where",
    body: `The explorer on the left is short on purpose. Every place is an **area**, and an area has a few **views** you switch between with the tabs at the top of its page.

| Area | Views |
| --- | --- |
| **Work** | Board, Requirements, Tasks (with their triggers) |
| **Automation** | Workflows (and their runs), Schedules, Recipes |
| **Insights** | Overview, Usage & caps, Activity (who did what) |
| **Settings** | General, Members & access, Roles & executors, Tools & skills, Memory & docs, Notifications |
| **Me** | Library, Connectors, Memory, My roles, My schedules, My tools & skills, My insights, My usage |

**Me** is yours across every project. **My assistant** is your personal agent. The **Account** link at the bottom of the explorer is for your account.

Looking for something by name? Press **Ctrl/Cmd + K** and type it: every view is in the palette. Ready-made setups such as job search are not areas, see [Use cases](help:use-cases).`,
  },
  {
    slug: "use-cases",
    title: "Use cases",
    group: "Concepts",
    summary: "Ready-made setups built on top of the platform, such as job search.",
    keywords: "use case job search jobs cv templates ready made",
    body: `7ar is a platform: projects, sessions, tasks, workflows and memory. A **use case** is a ready-made setup built on top of it for one purpose. It is not a part of the platform itself, so it does not have a place in the explorer.

Today there is one: **Job search assistant**. It reads your CV, finds and tracks openings, and asks you only what it cannot know.

You find use cases on the **Dashboard**, in the **Use cases** row, and in the command palette (**Ctrl/Cmd + K**, then type "use case"). A use case only shows up when your server has it turned on. More will be added the same way.`,
  },
  {
    slug: "sessions",
    title: "Sessions",
    group: "Concepts",
    summary: "A conversation with an agent that works in your project.",
    keywords: "chat conversation agent thread",
    body: `A **session** is one conversation with an agent. The agent has a private workspace with a copy of your project's files.

- Type a message and press Enter. The agent answers and may edit files or run commands.
- The tabs beside the chat show its **Files**, **Plan**, **Diff** (what it changed) and **Preview**.
- A session in a project is shared with the project's members. A session outside any project is personal.
- A coloured dot in the explorer shows the state: running, waiting for you, failed, or idle.

Type **@** in the message box to bring in a role, such as a reviewer.`,
  },
  {
    slug: "tasks",
    title: "Tasks",
    group: "Concepts",
    summary: "Work you hand off. An agent does it unattended and opens a pull request.",
    keywords: "factory pull request pr hand off unattended review",
    body: `A **task** is a piece of work with a title and a spec (what to do, in your words). When you create it, an agent picks it up, works on its own and, if your project has a code host, opens a pull request.

Tasks move through states: queued, running, in review, done. You only get pulled in when something needs you: a pull request to review, or a task that is blocked.

Good specs are short and concrete: say what should be true when the task is done.

Tasks can be created by hand, by a **trigger** (on a schedule or from a GitHub event), or from a requirement.`,
  },
  {
    slug: "requirements",
    title: "Requirements",
    group: "Concepts",
    summary: "A goal written down once; agents break it into tasks.",
    keywords: "issue goal backlog",
    body: `A **requirement** is a goal or a feature request that is bigger than one task. Write it down (or import the open issues of your repository), mark it ready, and agents can turn it into tasks.

The board shows each requirement and the sessions working on it, so you can see how far it has come.`,
  },
  {
    slug: "roles",
    title: "Roles",
    group: "Concepts",
    summary: "A saved way of working: a prompt, a model and a set of tools.",
    keywords: "role coder reviewer planner tester prompt",
    body: `A **role** describes how an agent should behave: its instructions, its model and the tools it may use. Built in roles include coder, planner, reviewer, tester and explorer.

Create your own under **Roles & executors**. A role can be picked for a task, or called in a chat with **@name**.

A role also says which **executor** runs it.`,
  },
  {
    slug: "executors",
    title: "Executors",
    group: "Concepts",
    summary: "What actually runs the agent. The 7ar harness is the recommended one.",
    keywords: "executor claude code codex cli harness engine runner api key",
    body: `An **executor** is the program that does the work for a role or task.

- **7ar harness** (recommended): runs inside 7ar. Approvals, budgets, memory and policies all work, and you see every step.
- **External executors** (for example a coding CLI): run the tool of another vendor with your own API key from Account. Use them when you want that tool's behaviour.

If you are unsure, leave it on the 7ar harness.`,
  },
  {
    slug: "workflows",
    title: "Workflows",
    group: "Concepts",
    summary: "A flow of steps (agents, decisions, gates) that runs the same way every time.",
    keywords: "workflow run step node graph template",
    body: `A **workflow** is a small diagram of steps: an agent does research, another writes, a person approves. Publish it and run it as often as you like. Each **run** shows the state of every step.

The fastest way to begin is a **template**: open the Workflows tab and copy one, then edit the prompts.

Steps can wait for a person. That wait is called a **gate**.`,
  },
  {
    slug: "gates",
    title: "Workflow gates",
    group: "Concepts",
    summary: "A step that waits for a person to say yes or no.",
    keywords: "gate approval decision wait human in the loop",
    body: `A **gate** is a point in a workflow where the run stops until a person decides. Use one before anything you want to look at first: publishing, merging, sending.

A waiting gate appears under **Needs you**. Open it, read what the earlier steps produced, then approve or reject. The run goes on from there.`,
  },
  {
    slug: "reviews",
    title: "Reviews and automatic merge",
    group: "Concepts",
    summary: "Who checks a change before it merges, and why a second model matters.",
    keywords: "review reviewer independent model automatic merge auto merge FACTORY_REVIEW_MODEL",
    body: `Before a change is merged, a reviewer reads it. Where **automatic merge** is on, the review and green checks are the only gate: nobody looks at the change in between.

## Independent review
The review is **independent** when it is done by a different model from the one that wrote the change. A model tends to approve its own work, so a second model catches more.

## Not independent
When no second model is set, the model that writes a change also reviews it. Tasks still work, but treat automatic merge with care: leave it off and merge the pull request yourself.

## Reviews are off
Where the administrator switched review off, a change merges on green checks alone.

## What to do
Ask an administrator to set a second review model (the server setting \`FACTORY_REVIEW_MODEL\`). Until then, keep **Merge automatically** off for work you have not read.`,
  },
  {
    slug: "policies",
    title: "Policies: supervised, sandboxed, autonomous",
    group: "Concepts",
    summary: "How much an agent may do without asking you.",
    keywords: "policy supervised sandboxed autonomous permission approval safety",
    body: `A **policy** decides how often an agent must ask before it acts.

| Policy | What it means | Example |
| --- | --- | --- |
| **Supervised** | Every risky call asks you first. | The agent wants to run \`rm -rf build\`: you approve or deny. |
| **Sandboxed** | The agent works freely in its own copy; only push and merge ask. | It edits and tests as it likes, and asks before pushing a branch. |
| **Autonomous** | Nothing asks; only protected rules deny. | A nightly dependency update that opens a pull request by itself. |

Start with **supervised** while you learn what the agent does. Move to **sandboxed** for everyday work. Use **autonomous** only for work you trust and cap with a budget.

Leave the policy on **Server default** if you are not sure: your administrator chose it.`,
  },
  {
    slug: "budgets",
    title: "Budgets and spend caps",
    group: "Concepts",
    summary: "A limit on what a task, a person or a project can spend.",
    keywords: "budget cap spend usd cost money limit tokens",
    body: `Agents cost money while they think. A **budget** puts a ceiling on it.

- **Task budget**: the most one task may spend, all attempts included. The task stops and asks you when it reaches it.
- **Monthly cap**: a limit for a person or a project. When it is reached, new runs are refused until you raise it.

Set the monthly cap under **Usage & caps**. The status bar at the bottom shows what you have spent this month.`,
  },
  {
    slug: "memory",
    title: "Memory",
    group: "Concepts",
    summary: "Facts agents remember between sessions, which you can review.",
    keywords: "memory remember knowledge agents.md library docs",
    body: `**Memory** is what agents remember about your project: how to run the tests, a decision, a convention. It is added to later sessions so you do not repeat yourself.

New memories wait for your review (a number appears beside **Memory & docs**). Keep the ones that are right and reject the rest. **Pinned** memories are always used.

You can import an \`AGENTS.md\` file into the memory and export it as \`MEMORY.md\`.`,
  },
  {
    slug: "tools-and-skills",
    title: "Tools and skills",
    group: "Concepts",
    summary: "Extra abilities for your agents, like searching docs or using GitHub.",
    keywords: "capabilities mcp server tools skills connections marketplace",
    body: `**Tools and skills** (also called capabilities) give agents abilities beyond editing files: search your documentation, use a code host, talk to a database.

Under the hood they are MCP servers. Turn one on, fill in what it needs (an account or a key), and agents in that project can use it. A tool that is missing setup says so.

**Connectors** are different: they are your own accounts (like GitHub) that 7ar can use for you.`,
  },
  {
    slug: "triggers",
    title: "Triggers",
    group: "Concepts",
    summary: "Create tasks by themselves, on a schedule or from a GitHub event.",
    keywords: "trigger cron github event automatic recurring",
    body: `A **trigger** creates tasks without you. Pick when (a schedule such as every night at 2, or a GitHub event such as a new issue) and write the task it should create.

Use triggers for routine work: a nightly dependency check, a weekly clean up. Pair them with a budget.`,
  },
  {
    slug: "approvals",
    title: "Understand approvals",
    group: "How to",
    summary: "What an approval asks, and how to answer it safely.",
    keywords: "approve deny tool risky command permission",
    body: `When an agent wants to do something your policy says needs a look, it stops and asks. You see **Wants to run \`<tool>\`** with **Approve** and **Deny**.

1. Open the session (or answer right on the dashboard card, or in the bottom panel).
2. Read the command or the file change.
3. **Approve** to let it go ahead, **Deny** to refuse. The agent adapts to a denial and tries another way.

If you do not understand a request, deny it and ask the agent to explain. Nothing is lost.

You can allow less asking by changing the [policy](help:policies).`,
  },
  {
    slug: "hand-off-a-task",
    title: "Hand off a task and review the pull request",
    group: "How to",
    summary: "Write it, let an agent work, review the result.",
    keywords: "task pull request pr review hand off",
    body: `1. Open your project, then **Tasks & triggers**, then **New task**.
2. Give it a **title** and a **spec**: what should be true when it is done.
3. Leave the options on their defaults. Open **Advanced** only if you need a model, a budget or a limit on attempts.
4. Press **Create task**. An agent starts working.
5. When it is ready, the task appears under **Needs you**. Open it, read the summary and the pull request, and merge or send feedback.

Tip: small, clear tasks work best.`,
  },
  {
    slug: "workflow-from-template",
    title: "Set up a workflow from a template",
    group: "How to",
    summary: "Copy a template, change the prompts, publish, run.",
    keywords: "workflow template publish run",
    body: `1. Open your project, then **Workflows & runs**.
2. Under the templates, choose one and press **Use**. A copy opens in the designer.
3. Click a step to change its prompt in the panel on the right.
4. Press **Publish**.
5. Press **Run**. Watch the steps change state. If there is a [gate](help:gates), approve it when it waits under **Needs you**.`,
  },
  {
    slug: "schedule-a-job",
    title: "Schedule recurring work",
    group: "How to",
    summary: "Have an agent do something every morning or every week.",
    keywords: "schedule cron recurring daily weekly",
    body: `1. Open **Schedules** in your project (or **My schedules** for a personal one).
2. Press **New schedule**.
3. Write what the agent should do and choose when: every hour, every morning, every week.
4. Save. Use **Run now** once to see how it goes. Each run is listed with a link to its session.

Switch it off with the toggle when you no longer need it.`,
  },
  {
    slug: "add-a-teammate",
    title: "Add a teammate",
    group: "How to",
    summary: "Invite people by email and choose what they may do.",
    keywords: "member invite team role viewer owner access",
    body: `1. Open your project, then **Members & access**.
2. Enter an email address and choose a role: **viewer** (read only), **member** (can work) or **owner** (can manage).
3. Press **Invite**. They get access when they sign in.

Only project owners can add or remove people.`,
  },
  {
    slug: "use-roles",
    title: "Use @ roles in a chat",
    group: "How to",
    summary: "Ask a reviewer, a planner or your own role for help inside a session.",
    keywords: "mention at role reviewer",
    body: `In the message box of a session, type **@**. A list of roles appears. Pick one, then write your message: the reply is given by that role.

Create roles under **Settings**, then **Roles & executors**. See [Roles](help:roles).`,
  },
  {
    slug: "glossary",
    title: "Glossary: the words 7ar uses",
    group: "Reference",
    summary: "One plain line for each word: policy, budget, executor, workflow, gate, trigger and more.",
    keywords: "glossary terms words meaning definition dictionary what does mean jargon policy budget executor workflow gate trigger recipe",
    body: `| Word | In plain words |
| --- | --- |
| **Project** | A repository your agents work in. See [the four ideas](help:the-basics). |
| **Session** | A conversation with an agent. See [Sessions](help:sessions). |
| **Task** | Work you hand off. An agent does it alone and opens a pull request. See [Tasks](help:tasks). |
| **Policy** | How often an agent must ask before it acts: supervised, sandboxed or autonomous. See [Policies](help:policies). |
| **Budget** | The most a task, a person or a project may spend. See [Budgets](help:budgets). |
| **Executor** | The program that does the agent's work, for example the 7ar harness or Claude Code. See [Executors](help:executors). |
| **Role** | A saved set of instructions for an agent, such as reviewer or planner. See [Roles](help:roles). |
| **Workflow** | Several steps in a row that you design once and run many times. See [Workflows](help:workflows). |
| **Gate** | A step in a workflow that waits until a person says yes or no. See [Gates](help:gates). |
| **Trigger** | Something that starts a task by itself, such as a new issue or a schedule. See [Triggers](help:triggers). |
| **Recipe** | A ready-made task, schedule or workflow you can add with a few answers. |
| **Needs you** | The list of things an agent is waiting on: approve, answer or review. See [Approvals](help:approvals). |`,
  },
  {
    slug: "shortcuts",
    title: "Keyboard shortcuts",
    group: "Reference",
    summary: "Move around fast without the mouse.",
    keywords: "keyboard shortcuts keys hotkeys command palette",
    body: `| Keys | What it does |
| --- | --- |
| **Ctrl/Cmd + K** | Open the command palette: jump anywhere, run any action |
| **Ctrl/Cmd + P** | Switch project |
| **Ctrl/Cmd + B** | Show or hide the explorer |
| **Ctrl/Cmd + J** | Show or hide the bottom panel |
| **Ctrl + W** or **Alt + W** | Close the current tab |
| **Ctrl + Tab** or **Alt + ]** | Next tab |
| **Ctrl + Shift + Tab** or **Alt + [** | Previous tab |
| **?** | Show the shortcuts |
| **Esc** | Close a dialog or sheet |`,
  },
  {
    slug: "faq",
    title: "FAQ and troubleshooting",
    group: "Reference",
    summary: "Common questions and what to do when something is stuck.",
    keywords: "faq troubleshooting problem stuck error help why",
    body: `## An agent seems stuck
Open its session. If it shows **Needs approval** or **Needs answer**, it is waiting for you. Otherwise press stop and send a new message.

## I cannot see the Tasks tab working
The agent factory has to be enabled on your server. Ask your administrator.

## A run was refused
A spend cap may be reached. Open **Insights**, then **Usage & caps**, and raise it, or wait for next month.

## Where did my files go?
Each session works in its own copy. Look in the session's **Files** and **Diff** tabs. Work you want to keep goes into a pull request.

## Can I undo what an agent did?
Changes live in the session's copy until they are pushed. Deny a push and nothing leaves.

## Show me the welcome again
Open the user menu (bottom of the explorer) and choose **Getting started**, or **Take the tour**.`,
  },
];

// The articles that use a term from the glossary end with a link to it.
for (const a of ARTICLES) if (["policies", "budgets", "executors", "workflows", "gates", "triggers"].includes(a.slug)) a.body += "\n\nA word you do not know? See the [glossary](help:glossary).";

export const articleBySlug = (slug: string | undefined) => ARTICLES.find((a) => a.slug === slug);

const norm = (s: string) => s.toLowerCase();

/** Articles that match every word of the query (title, summary, keywords, text), best matches first. */
export function searchArticles(query: string, list: Article[] = ARTICLES): Article[] {
  const words = norm(query).split(/\s+/).filter(Boolean);
  if (!words.length) return list;
  const scored: [Article, number][] = [];
  for (const a of list) {
    const title = norm(a.title);
    const head = norm(`${a.summary} ${a.keywords ?? ""}`);
    const body = norm(a.body);
    let score = 0;
    let all = true;
    for (const w of words) {
      if (title.includes(w)) score += 6;
      else if (head.includes(w)) score += 3;
      else if (body.includes(w)) score += 1;
      else all = false;
    }
    if (all) scored.push([a, score]);
  }
  return scored.sort((x, y) => y[1] - x[1]).map(([a]) => a);
}

export const helpHref = (slug?: string) => (slug ? `/help?a=${encodeURIComponent(slug)}` : "/help");
