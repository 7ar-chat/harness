// A generated avatar for people without a picture: a soft two-color gradient with a few overlapping circles, all derived from the
// login so a person looks the same everywhere. Pure data (the SVG is drawn by UserAvatar), so it is easy to test.

export interface AvatarBlob {
  cx: number;
  cy: number;
  r: number;
  color: string;
  opacity: number;
}
export interface AvatarArt {
  /** Gradient stops, top-left to bottom-right. */
  from: string;
  to: string;
  blobs: AvatarBlob[];
}

/** FNV-1a, 32 bits. */
export function hashString(s: string): number {
  let h = 0x811c9dc5;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 0x01000193);
  }
  return h >>> 0;
}

/** mulberry32: a small seeded generator returning floats in [0, 1). */
function rng(seed: number): () => number {
  let a = seed;
  return () => {
    a = (a + 0x6d2b79f5) | 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

const hsl = (h: number, s: number, l: number) => `hsl(${Math.round(((h % 360) + 360) % 360)} ${Math.round(s)}% ${Math.round(l)}%)`;

/** The drawing for a seed (a login). Same seed, same art; the 100x100 viewBox is the canvas. */
export function avatarArt(seed: string): AvatarArt {
  const rand = rng(hashString(seed || "?"));
  const hue = rand() * 360;
  // Analogous colors keep it harmonious; mid lightness reads on both light and dark surfaces.
  const from = hsl(hue, 70 + rand() * 15, 58 + rand() * 8);
  const to = hsl(hue + 35 + rand() * 40, 72 + rand() * 12, 48 + rand() * 8);
  const accents = [hue + 160 + rand() * 40, hue - 30 - rand() * 30, hue + 60 + rand() * 30];
  const blobs = accents.map((a, i) => ({
    cx: Math.round(15 + rand() * 70),
    cy: Math.round(15 + rand() * 70),
    r: Math.round(22 + rand() * 24 - i * 3),
    color: hsl(a, 75 + rand() * 10, 68 + rand() * 10),
    opacity: Math.round((0.35 + rand() * 0.3) * 100) / 100,
  }));
  return { from, to, blobs };
}
