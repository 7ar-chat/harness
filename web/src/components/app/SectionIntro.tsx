// A one-line subtitle on top of each project section: what it is for, with a "?" to the matching help article.
import { HelpButton } from "./help";

const INTRO: Record<string, { title: string; text: string; slug?: string }> = {
  insights: { title: "Insights", text: "What this project's agents delivered, and what it cost.", slug: "insights" },
  recipes: { title: "Recipes", text: "Ready-made schedules, tasks and triggers: pick one and fill in a few words.", slug: "recipes" },
  notifications: { title: "Notifications", text: "Who hears about what in this project. Add channels to tell the whole team." },
  board: { title: "Board", text: "Sessions and tasks as cards, grouped by where they stand.", slug: "sessions" },
  overview: { title: "Sessions", text: "Every session with an agent in this project.", slug: "sessions" },
  requirements: { title: "Requirements", text: "Goals you write down once; agents break them into tasks.", slug: "requirements" },
  tasks: { title: "Tasks & triggers", text: "Tasks are work you hand off to an agent. Triggers create tasks by themselves, on a schedule or from GitHub.", slug: "tasks" },
  workflows: { title: "Workflows & runs", text: "A procedure your team repeats: steps for agents, commands and people. Publish one to run it.", slug: "workflows" },
  roles: { title: "Roles & executors", text: "A role is a named agent setup: instructions, model, tools and skills. Type @ in a session to run as one. An executor is what runs it.", slug: "roles" },
  schedules: { title: "Schedules", text: "Work an agent does on a timetable, such as every morning.", slug: "schedule-a-job" },
  memory: { title: "Memory & docs", text: "What agents remember about this project, and the documents they can read.", slug: "memory" },
  members: { title: "Members & access", text: "Who can see and change this project.", slug: "add-a-teammate" },
  capabilities: { title: "Tools & skills", text: "Tools and skills (MCP servers) your agents can use in this project.", slug: "tools-and-skills" },
  audit: { title: "Audit log", text: "Who did what in this project, newest first." },
  settings: { title: "Project settings", text: "The name, repositories and channels of this project." },
  usage: { title: "Usage & caps", text: "What this project spent, and the monthly cap that limits it.", slug: "budgets" },
};

export function SectionIntro({ section }: { section: string }) {
  const i = INTRO[section];
  if (!i) return null;
  return (
    <header className="space-y-1">
      <h1 className="text-xl font-semibold">{i.title}</h1>
      <p className="flex items-center gap-1.5 text-sm text-muted-foreground" data-testid="section-intro">
        <span>{i.text}</span>
        {i.slug ? <HelpButton slug={i.slug} /> : null}
      </p>
    </header>
  );
}
