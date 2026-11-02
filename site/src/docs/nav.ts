// The documentation map: groups of the sidebar and the order of the pages. A page is the file
// src/content/docs/<slug>.md; buildNav() fails the build when this list and the files disagree, so a page can
// neither be orphaned nor linked from the sidebar without existing.
import { getCollection } from 'astro:content';

export interface NavGroup {
  id: string;
  label: string;
  blurb: string;
  pages: { slug: string; title: string }[];
}

export const groups: NavGroup[] = [
  {
    id: 'introduction',
    label: 'Introduction',
    blurb: 'What the harness is, how it is layered and which module to import.',
    pages: [
      { slug: 'introduction/what-is-the-harness', title: 'What is the harness' },
      { slug: 'introduction/architecture', title: 'Architecture' },
      { slug: 'introduction/modules', title: 'Module map' },
    ],
  },
  {
    id: 'quickstart',
    label: 'Quickstart',
    blurb: 'Install, embed one conversation, run the terminal UI and the web product.',
    pages: [
      { slug: 'quickstart/install', title: 'Install' },
      { slug: 'quickstart/embed', title: 'Smallest embedding' },
      { slug: 'quickstart/cli', title: 'Run the CLI and TUI' },
      { slug: 'quickstart/web', title: 'Run the web product' },
    ],
  },
  {
    id: 'concepts',
    label: 'Concepts',
    blurb: 'The ideas behind runs, tools, features, state, sandboxes and safety.',
    pages: [
      { slug: 'concepts/threads-runs-events', title: 'Threads, runs and events' },
      { slug: 'concepts/agent-loop', title: 'The agent loop' },
      { slug: 'concepts/tools', title: 'Tools' },
      { slug: 'concepts/features-profiles', title: 'Features and profiles' },
      { slug: 'concepts/store', title: 'The store' },
      { slug: 'concepts/infra', title: 'Infrastructure' },
      { slug: 'concepts/sandboxes', title: 'Sandboxes' },
      { slug: 'concepts/approvals-permissions', title: 'Approvals and permissions' },
      { slug: 'concepts/memory', title: 'Memory' },
      { slug: 'concepts/skills', title: 'Skills' },
      { slug: 'concepts/artifacts-checkpoints', title: 'Artifacts and checkpoints' },
      { slug: 'concepts/compaction', title: 'Compaction' },
      { slug: 'concepts/plan-and-goals', title: 'Plan mode and goals' },
      { slug: 'concepts/verify-and-explore', title: 'Verify and explore' },
      { slug: 'concepts/titles-docs-commands', title: 'Titles, project docs, commands' },
      { slug: 'concepts/mcp', title: 'MCP' },
    ],
  },
  {
    id: 'guides',
    label: 'How-to guides',
    blurb: 'Task-oriented recipes: embed, extend, scale, test, release.',
    pages: [
      { slug: 'guides/embed-website', title: 'Embed in a website' },
      { slug: 'guides/deployment-shapes', title: 'Choose a deployment shape' },
      { slug: 'guides/custom-tool', title: 'Add a custom tool' },
      { slug: 'guides/custom-feature', title: 'Add a custom feature' },
      { slug: 'guides/sandbox-backend', title: 'Write a sandbox backend' },
      { slug: 'guides/postgres-redis', title: 'Postgres and Redis for several processes' },
      { slug: 'guides/connect-mcp', title: 'Connect MCP servers' },
      { slug: 'guides/use-ai', title: 'Use the ai module on its own' },
      { slug: 'guides/use-core-modelinfo', title: 'Use core and modelinfo on their own' },
      { slug: 'guides/use-client-tui', title: 'Use hosts/client and hosts/tui on their own' },
      { slug: 'guides/testing', title: 'Test your agent' },
      { slug: 'guides/team-mode', title: 'Run team mode' },
      { slug: 'guides/releasing', title: 'Releasing and versioning' },
    ],
  },
  {
    id: 'reference',
    label: 'Reference',
    blurb: 'Configuration, command lines, HTTP API, events and packages.',
    pages: [
      { slug: 'reference/configuration', title: 'Configuration files' },
      { slug: 'reference/env-vars', title: 'Environment variables' },
      { slug: 'reference/config-yml', title: 'config.yml keys' },
      { slug: 'reference/cli-harness', title: 'harness (CLI and TUI)' },
      { slug: 'reference/cli-harness-web', title: 'harness-web' },
      { slug: 'reference/cli-harness-worker', title: 'harness-worker' },
      { slug: 'reference/http-api', title: 'HTTP API' },
      { slug: 'reference/client-contract', title: 'The Client contract' },
      { slug: 'reference/events', title: 'Event types' },
      { slug: 'reference/harness-api', title: 'The Go facade' },
      { slug: 'reference/packages', title: 'Package index' },
    ],
  },
  {
    id: 'integrations',
    label: 'Integrations',
    blurb: 'Model providers, sandbox backends, stores, Redis, MCP, OIDC.',
    pages: [
      { slug: 'integrations/llm-providers', title: 'LLM providers' },
      { slug: 'integrations/sandbox-backends', title: 'Sandbox backends' },
      { slug: 'integrations/stores', title: 'Stores' },
      { slug: 'integrations/redis', title: 'Redis' },
      { slug: 'integrations/mcp', title: 'MCP servers' },
      { slug: 'integrations/oidc', title: 'OIDC' },
      { slug: 'integrations/github-telegram', title: 'GitHub and Telegram' },
    ],
  },
  {
    id: 'operations',
    label: 'Operations',
    blurb: 'Security, production checklist, telemetry, upgrades, troubleshooting.',
    pages: [
      { slug: 'operations/security', title: 'Security model' },
      { slug: 'operations/production', title: 'Production checklist' },
      { slug: 'operations/observability', title: 'Observability' },
      { slug: 'operations/upgrade-notes', title: 'Upgrade notes' },
      { slug: 'operations/troubleshooting', title: 'Troubleshooting' },
    ],
  },
];

export interface NavEntry { slug: string; title: string; url: string; group: NavGroup; }

export const flat: NavEntry[] = groups.flatMap((g) =>
  g.pages.map((p) => ({ ...p, url: `/docs/${p.slug}/`, group: g })),
);

export async function checkNav(): Promise<void> {
  const files = new Set((await getCollection('docs')).map((e) => e.id));
  const listed = new Set(flat.map((p) => p.slug));
  const missing = [...listed].filter((s) => !files.has(s));
  const orphan = [...files].filter((s) => !listed.has(s));
  if (missing.length || orphan.length) {
    throw new Error(
      `docs navigation out of sync with src/content/docs: ` +
        `listed but no file: [${missing.join(', ')}]; file but not listed: [${orphan.join(', ')}]`,
    );
  }
}
