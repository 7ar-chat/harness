// A one-line subtitle on top of each project section: what it is for, with a "?" to the matching help article.
import { HelpButton } from "./help";

const INTRO: Record<string, { text: string; slug?: string }> = {
  board: { text: "Sessions and tasks as cards, grouped by where they stand.", slug: "sessions" },
  overview: { text: "Every session with an agent in this project.", slug: "sessions" },
  requirements: { text: "Goals you write down once; agents break them into tasks.", slug: "requirements" },
  tasks: { text: "Tasks are work you hand off to an agent. Triggers create tasks by themselves, on a schedule or from GitHub.", slug: "tasks" },
  workflows: { text: "A procedure your team repeats: steps for agents, commands and people. Publish one to run it.", slug: "workflows" },
  roles: { text: "A role is a named agent setup: instructions, model, tools and skills. Type @ in a session to run as one. An executor is what runs it.", slug: "roles" },
  schedules: { text: "Work an agent does on a timetable, such as every morning.", slug: "schedule-a-job" },
  memory: { text: "What agents remember about this project, and the documents they can read.", slug: "memory" },
  members: { text: "Who can see and change this project.", slug: "add-a-teammate" },
  capabilities: { text: "Tools and skills (MCP servers) your agents can use in this project.", slug: "tools-and-skills" },
  audit: { text: "Who did what in this project, newest first." },
  settings: { text: "The name, repositories and channels of this project." },
  usage: { text: "What this project spent, and the monthly cap that limits it.", slug: "budgets" },
};

export function SectionIntro({ section }: { section: string }) {
  const i = INTRO[section];
  if (!i) return null;
  return (
    <p className="flex items-center gap-1.5 text-sm text-muted-foreground" data-testid="section-intro">
      <span>{i.text}</span>
      {i.slug ? <HelpButton slug={i.slug} /> : null}
    </p>
  );
}
