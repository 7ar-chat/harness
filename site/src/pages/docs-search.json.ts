import { getCollection, render } from 'astro:content';
import { flat } from '../docs/nav';

// Build-time search index: one record per page and per h2/h3 heading, loaded by src/scripts/docs.js.
// Fields: t = title, g = group, u = url, d = description (pages only), h = parent page title (headings only).
export async function GET() {
  const entries = new Map((await getCollection('docs')).map((e) => [e.id, e]));
  const out: { t: string; g: string; u: string; d?: string; h?: string }[] = [];
  for (const p of flat) {
    const entry = entries.get(p.slug)!;
    out.push({ t: entry.data.title, g: p.group.label, u: p.url, d: entry.data.description });
    const { headings } = await render(entry);
    for (const h of headings) {
      if (h.depth === 2 || h.depth === 3) out.push({ t: h.text, g: p.group.label, u: `${p.url}#${h.slug}`, h: entry.data.title });
    }
  }
  return new Response(JSON.stringify(out), { headers: { 'Content-Type': 'application/json' } });
}
