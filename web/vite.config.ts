import path from "node:path";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { defineConfig } from "vitest/config";
import type { Plugin } from "vite";

// Tailwind's license banner contains a URL; the Go test (and the CSP mindset) wants none in the CSS.
const stripCssBanners = (): Plugin => ({
  name: "strip-css-banners",
  generateBundle(_, bundle) {
    for (const f of Object.values(bundle)) {
      if (f.type === "asset" && f.fileName.endsWith(".css") && typeof f.source === "string") f.source = f.source.replace(/\/\*![\s\S]*?\*\//g, "");
    }
  },
});

// The server's CSP forbids inline <style> elements. A few libraries inject one at runtime
// (Radix hides native scrollbars, sonner ships its CSS in JS); drop those injections here and
// ship the same rules in src/index.css / the imported sonner stylesheet instead.
const noInlineStyles = (): Plugin => ({
  name: "no-inline-style-elements",
  enforce: "pre",
  transform(code, id) {
    if (!id.includes("node_modules")) return null;
    if (/@radix-ui\/react-(select|scroll-area)\/dist\/index\.mjs/.test(id)) {
      const out = code.replace(/jsx2?s?\(\s*"style",\s*\{\s*dangerouslySetInnerHTML:\s*\{\s*__html:\s*`[^`]*`\s*\},?\s*nonce[^}]*\}\s*\)/g, "null");
      return out === code ? null : { code: out, map: null };
    }
    if (/@git-diff-view\/react\/dist\/esm\/index\.mjs/.test(id)) return { code: code.replaceAll('jsx("style", { "data-select-style": true, ref: ref })', "null"), map: null };
    if (/react-style-singleton\/dist\/es2015\/hook\.js/.test(id)) return { code: code.replace("sheet.add(styles);", ""), map: null };
    if (/sonner\/dist\/index\.mjs/.test(id)) return { code: code.replace("function __insertCSS(code) {", "function __insertCSS(code) { return;"), map: null };
    return null;
  },
});

// The Go server embeds ../hosts/httpapi/ui and serves it at / and /ui/.
export default defineConfig({
  base: "/ui/",
  plugins: [noInlineStyles(), react(), tailwindcss(), stripCssBanners()],
  resolve: { alias: { "@": path.resolve(import.meta.dirname, "src") } },
  build: { outDir: "../hosts/httpapi/ui", emptyOutDir: false, chunkSizeWarningLimit: 2500 },
  server: {
    proxy: {
      "/api": { target: process.env.HARNESS_URL ?? "http://127.0.0.1:8080", changeOrigin: true },
      "/auth": { target: process.env.HARNESS_URL ?? "http://127.0.0.1:8080", changeOrigin: true },
    },
  },
  test: { environment: "jsdom", setupFiles: ["src/test/setup.ts"], include: ["src/**/*.test.ts", "src/**/*.test.tsx"] },
});
