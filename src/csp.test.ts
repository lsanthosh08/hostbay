import { expect, it } from "vitest";
import html from "../index.html?raw";

// The CSP allows the inline theme script by hash; editing the script without
// updating the hash would silently block it.
it("allows the inline theme script by its hash", async () => {
  const script = html.match(/<script>([\s\S]*?)<\/script>/)![1];
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(script));
  const hash = btoa(String.fromCharCode(...new Uint8Array(digest)));
  const csp = html.match(/http-equiv="Content-Security-Policy"\s+content="([^"]*)"/)![1];
  expect(csp).toContain(`script-src 'self' 'sha256-${hash}'`);
});

// KTD19: RDP and VNC run over Wails GoStreams, so the page may open no
// loopback WebSocket, and connect-src allows this app's own origin only.
it("allows no loopback socket", () => {
  const csp = html.match(/http-equiv="Content-Security-Policy"\s+content="([^"]*)"/)![1];
  expect(csp).not.toContain("ws://127.0.0.1");
  expect(csp).toMatch(/connect-src 'self';/);
});

// SPICE video (VP8) and audio (Opus) play from MediaSource blob: URLs the
// app creates itself; media-src allows those and nothing remote.
it("allows only same-origin and blob: media", () => {
  const csp = html.match(/http-equiv="Content-Security-Policy"\s+content="([^"]*)"/)![1];
  expect(csp).toMatch(/media-src 'self' blob:;/);
});
