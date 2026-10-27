import { describe, expect, it } from "vitest";
import html from "../../index.html?raw";
import mainGo from "../../main.go?raw";
import { ANSI_KEYS, monacoTheme, terminalPalette, terminalTheme, xtermTheme } from "./terminal";
import { longHex, PALETTE_KEYS, TERMINAL_THEMES } from "./terminalThemes";
import { THEMES } from "../ui/theme";

// Vitest hands .css imports to its CSS pipeline, which yields "" in tests.
const fs = (globalThis as any).process.getBuiltinModule("node:fs");
const css: string = fs.readFileSync(new URL("../styles.css", import.meta.url), "utf8");

// Each [data-theme="…"] block in styles.css as token -> value.
const blocks: Record<string, Record<string, string>> = {};
for (const [, name, body] of css.matchAll(/\[data-theme="([\w-]+)"\]\s*\{([^}]*)\}/g))
  blocks[name] = Object.fromEntries([...body.matchAll(/(--[\w-]+|color-scheme)\s*:\s*([^;]+);/g)].map((m) => [m[1], m[2].trim()]));

// Runs index.html's inline boot script against a saved theme value.
function boot(saved: string | null) {
  const script = html.match(/<script>([\s\S]*?)<\/script>/)![1];
  const root = { dataset: {} as Record<string, string>, style: {} as Record<string, string> };
  new Function("localStorage", "document", script)({ getItem: () => saved }, { documentElement: root });
  return root;
}

describe("theme tokens", () => {
  it("ships Dark, Light and Deep Blue", () => {
    expect(Object.keys(blocks).sort()).toEqual(["dark", "deep-blue", "light"]);
    expect(THEMES.map((t) => t.id).sort()).toEqual(["dark", "deep-blue", "light"]);
  });

  it("defines in every theme each token the dark theme defines", () => {
    const want = Object.keys(blocks.dark);
    expect(want.length).toBeGreaterThan(20);
    for (const name of Object.keys(blocks)) expect(Object.keys(blocks[name]).sort(), name).toEqual([...want].sort());
  });

  it("gives Light its own values", () => {
    for (const k of ["--ground", "--surface", "--fg", "--muted", "--line", "color-scheme"])
      expect(blocks.light[k], k).not.toBe(blocks.dark[k]);
  });

  it("offers Dark, Deep Blue and Light in the theme picker", () => {
    expect(THEMES.filter((t) => t.picker).map((t) => t.id)).toEqual(["dark", "deep-blue", "light"]);
  });
});

describe("theme boot script", () => {
  it("boots a saved light theme into Light", () => {
    expect(boot("light").dataset.theme).toBe("light");
  });

  it("falls back to Dark when nothing valid is saved", () => {
    expect(boot(null).dataset.theme).toBe("dark");
    expect(boot("solarized").dataset.theme).toBe("dark");
    expect(boot("constructor").dataset.theme).toBe("dark");
  });

  it("paints each theme's ground before the app loads", () => {
    for (const name of Object.keys(blocks)) expect(boot(name).style.background, name).toBe(blocks[name]["--ground"]);
  });

  it("matches main.go's window background to the dark ground", () => {
    const [, r, g, b] = mainGo.match(/BackgroundColour:\s*application\.NewRGB\((\d+),\s*(\d+),\s*(\d+)\)/)!;
    const hex = "#" + [r, g, b].map((n) => Number(n).toString(16).padStart(2, "0")).join("");
    expect(hex).toBe(blocks.dark["--ground"]);
  });
});

const HEX = /^#[0-9a-f]{6}([0-9a-f]{2})?$/i;
const tokens = (name: string) => (k: string) => blocks[name][k] ?? "";

describe("terminal themes", () => {
  it("ships 28 themes, each defining every palette field", () => {
    expect(TERMINAL_THEMES).toHaveLength(28);
    expect(new Set(TERMINAL_THEMES.map((t) => t.id)).size).toBe(28);
    for (const th of TERMINAL_THEMES) {
      expect(Object.keys(th.colors).sort(), th.id).toEqual([...PALETTE_KEYS].sort());
      for (const k of PALETTE_KEYS) expect(th.colors[k], `${th.id} ${k}`).toMatch(HEX);
    }
  });

  it("renders Dracula with its own colours under the Light UI theme", () => {
    const dracula = terminalPalette("dracula", [])!;
    const theme = xtermTheme(tokens("light"), dracula);
    expect(theme.background).toBe("#282a36");
    expect(theme.foreground).toBe("#f8f8f2");
    expect(theme.red).toBe("#ff5555");
    expect(theme.selectionBackground).toBe(dracula.selection);
    expect(theme.background).not.toBe(blocks.light["--term-bg"]);
    // The UI around it stays Light.
    expect(monacoTheme(tokens("light")).base).toBe("vs");
  });

  it("matches the UI theme by default", () => {
    expect(terminalPalette("", [])).toBeUndefined();
    expect(terminalPalette("no-such-theme", [])).toBeUndefined();
    expect(xtermTheme(tokens("light")).background).toBe(blocks.light["--term-bg"]);
  });

  it("makes the background see-through over an image, keeping its colour", () => {
    expect(terminalTheme({ terminalTheme: "dracula" }, false, tokens("dark")).background).toBe("#282a36");
    expect(terminalTheme({ terminalTheme: "dracula" }, true, tokens("dark")).background).toBe("#282a3600");
    expect(terminalTheme({}, true, tokens("light")).background).toBe(blocks.light["--term-bg"].slice(0, 7) + "00");
  });

  it("uses a custom theme, and ignores one whose colours are not hex", () => {
    const colors = { ...terminalPalette("nord", [])!, red: "#123456" };
    expect(terminalPalette("custom-1", [{ id: "custom-1", name: "Mine", type: "dark", colors }])?.red).toBe("#123456");
    const bad = { ...colors, red: "url(javascript:x)" };
    expect(terminalPalette("custom-1", [{ id: "custom-1", name: "Mine", type: "dark", colors: bad }])).toBeUndefined();
  });
});

describe("xterm theme", () => {
  it("has 16 ANSI colours plus background, foreground, cursor and selection", () => {
    for (const name of Object.keys(blocks)) {
      const theme = xtermTheme((k) => blocks[name][k] ?? "");
      expect(ANSI_KEYS).toHaveLength(16);
      for (const k of [...ANSI_KEYS, "background", "foreground", "cursor", "selectionBackground"] as const)
        expect(theme[k], `${name} ${k}`).toMatch(/^#[0-9a-f]{6}([0-9a-f]{2})?$/i);
    }
  });
});

describe("longHex", () => {
  it("writes minified token colours in the long form settings accept", () => {
    expect(longHex("#fff3")).toBe("#ffffff33");
    expect(longHex("#a1C")).toBe("#aa11CC");
    expect(longHex("#18181b")).toBe("#18181b");
    expect(longHex("#ffffff33")).toBe("#ffffff33");
  });
});
