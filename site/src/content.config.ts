import { defineCollection } from 'astro:content';
import { glob } from 'astro/loaders';
import { z } from 'astro/zod';

// One Markdown file per documentation page: src/content/docs/<group>/<page>.md becomes /docs/<group>/<page>/.
// The groups and the order of the sidebar are in src/docs/nav.ts.
const docs = defineCollection({
  loader: glob({ pattern: '**/*.md', base: './src/content/docs' }),
  schema: z.object({
    title: z.string(),
    description: z.string(),
  }),
});

export const collections = { docs };
