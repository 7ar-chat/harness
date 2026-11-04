import { defineConfig } from "astro/config";

// PREVIEW_BASE is set when the site runs behind the preview proxy ("/preview/<token>/").
export default defineConfig({
  base: process.env.PREVIEW_BASE || "/",
  server: { host: true, port: 4321 },
  vite: { server: { allowedHosts: true } },
});
