import { expect, it } from "vitest";

const sources = import.meta.glob(["../**/*.{ts,tsx}", "!../**/*.test.*", "!../**/*.d.ts", "!../bindings/**"], {
  query: "?raw",
  import: "default",
  eager: true,
}) as Record<string, string>;

// The macOS webview answers the browser's own confirm() with false, so a
// destructive action guarded by it never runs, and one guarded by an
// unawaited usePrompt().confirm (a Promise, always truthy) never asks.
// Every confirm() call is the kit's, from usePrompt in that same function,
// and awaited.
it("asks every confirm through the kit's awaited dialog", () => {
  const bad: string[] = [];
  for (const [file, text] of Object.entries(sources)) {
    if (/\bwindow\.confirm\(/.test(text)) bad.push(`${file}: window.confirm`);
    for (const fn of text.split(/^(?=(?:export )?(?:async )?function )/m)) {
      for (const m of fn.matchAll(/(?<![.\w])confirm\(/g)) {
        const line = fn.slice(fn.lastIndexOf("\n", m.index) + 1, fn.indexOf("\n", m.index));
        if (/^\s*(\/\/|\*)/.test(line)) continue;
        const awaited = fn.slice(0, m.index).endsWith("await ") || /\)\.then\(/.test(line);
        if (!awaited || !fn.includes("usePrompt()")) bad.push(`${file}: ${line.trim()}`);
      }
    }
  }
  expect(bad).toEqual([]);
});
