import { defineConfig } from 'astro/config';

// Fully static output (dist/), served by nginx. Stylesheets and scripts stay external files so the
// Content-Security-Policy written by 40-telemetry.sh can stay script-src 'self'.
export default defineConfig({
  site: 'https://7ar.chat',
  output: 'static',
  build: { inlineStylesheets: 'never' },
  vite: { build: { assetsInlineLimit: 0 } },
});
