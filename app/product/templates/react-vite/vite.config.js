import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// PREVIEW_BASE is set when the app runs behind the preview proxy ("/preview/<token>/").
export default defineConfig({
  base: process.env.PREVIEW_BASE || "/",
  plugins: [react()],
  server: { host: true, port: 5173, strictPort: true, allowedHosts: true },
});
