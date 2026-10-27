import { readdirSync, readFileSync } from "node:fs";
import { join, relative } from "node:path";
import { defineConfig, type Plugin } from "vite";
import { configDefaults } from "vitest/config";
import react from "@vitejs/plugin-react";
import tailwind from "@tailwindcss/vite";

// The dev server needs inline scripts (React refresh) and its own HMR socket,
// which the Content Security Policy in index.html forbids; builds keep it.
const stripCspInDev = {
  name: "strip-csp-in-dev",
  apply: "serve" as const,
  transformIndexHtml: (html: string) =>
    html.replace(/<meta\s+http-equiv="Content-Security-Policy"[^>]*>/, ""),
};

// spice-html5 (LGPL-3.0) ships as its own files, copied as they are from
// src/vendor/spice-html5 with its licence and patch note, and loaded only
// when a SPICE tab opens (src/tabs/spice.ts). It is never bundled or
// minified into the app's chunks, so it stays a separate, replaceable library.
const SPICE_DIR = "src/vendor/spice-html5";
const copySpice: Plugin = {
  name: "copy-spice-html5",
  apply: "build",
  generateBundle() {
    for (const f of readdirSync(SPICE_DIR, { recursive: true, withFileTypes: true })) {
      if (!f.isFile()) continue;
      const path = join(f.parentPath, f.name);
      this.emitFile({ type: "asset", fileName: `vendor/spice-html5/${relative(SPICE_DIR, path)}`, source: readFileSync(path) });
    }
  },
};

// Wails serves the frontend from a fixed dev port and embeds the static build.
export default defineConfig({
  plugins: [react(), tailwind(), stripCspInDev, copySpice],
  clearScreen: false,
  server: {
    port: 1420,
    strictPort: true,
  },
  build: {
    // The webviews themselves (WKWebView and WebKitGTK at Safari 15 or later,
    // WebView2): ES2021 plus top-level await, which noVNC's lazily loaded
    // chunk uses (core/util/browser.js); under es2021 Vite only warned and left it.
    target: ["safari15", "chrome100"],
    outDir: "dist",
  },
  // Agent worktrees under .claude/ are other checkouts of this repo.
  test: { exclude: [...configDefaults.exclude, ".claude/**"] },
});
