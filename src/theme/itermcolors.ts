// Reads iTerm2's .itermcolors files: an XML plist with a flat dict of the
// 16 ANSI colours, background, foreground, cursor and selection, each a
// dict of Red/Green/Blue Component reals from 0 to 1. Parsed in the page
// (KTD41); a scan by key, not a general plist parser.
//
// Adapted from uniterm (https://github.com/ys-ll/uniterm,
// frontend/src/composables/itermcolorsParser.ts), Copyright 2025 uniTerm,
// licensed under the Apache License, Version 2.0. Changes: it reports the
// colours a file lacks and rejects a file with none; no export.

import type { Palette } from "./terminalThemes";

const ITERM_KEY: Palette = {
  background: "Background Color",
  foreground: "Foreground Color",
  cursor: "Cursor Color",
  selection: "Selection Color",
  black: "Ansi 0 Color",
  red: "Ansi 1 Color",
  green: "Ansi 2 Color",
  yellow: "Ansi 3 Color",
  blue: "Ansi 4 Color",
  magenta: "Ansi 5 Color",
  cyan: "Ansi 6 Color",
  white: "Ansi 7 Color",
  brightBlack: "Ansi 8 Color",
  brightRed: "Ansi 9 Color",
  brightGreen: "Ansi 10 Color",
  brightYellow: "Ansi 11 Color",
  brightBlue: "Ansi 12 Color",
  brightMagenta: "Ansi 13 Color",
  brightCyan: "Ansi 14 Color",
  brightWhite: "Ansi 15 Color",
};

const byte = (n: number) =>
  Math.round(Math.min(1, Math.max(0, n)) * 255)
    .toString(16)
    .padStart(2, "0");

// One `<key>Name</key><dict>…</dict>` block's colour as #rrggbb, or null.
function color(xml: string, key: string): string | null {
  const block = new RegExp(`<key>${key}</key>\\s*<dict>([\\s\\S]*?)</dict>`, "i").exec(xml);
  if (!block) return null;
  const part = (name: string) => {
    const m = new RegExp(`<key>${name} Component</key>\\s*<real>([\\d.eE+-]+)</real>`, "i").exec(block[1]);
    const n = m ? Number(m[1]) : NaN;
    return Number.isFinite(n) ? n : null;
  };
  const [r, g, b] = [part("Red"), part("Green"), part("Blue")];
  return r === null || g === null || b === null ? null : `#${byte(r)}${byte(g)}${byte(b)}`;
}

/** The palette an .itermcolors file holds. A colour it lacks keeps `base`'s
 *  and is listed in `missing`. Throws when the file holds no colour. */
export function parseItermColors(xml: string, base: Palette): { colors: Palette; missing: (keyof Palette)[] } {
  if (!/<plist[\s>]/.test(xml)) throw new Error("not an .itermcolors file");
  const colors = { ...base };
  const missing: (keyof Palette)[] = [];
  for (const k of Object.keys(ITERM_KEY) as (keyof Palette)[]) {
    const c = color(xml, ITERM_KEY[k]);
    if (c) colors[k] = c;
    else missing.push(k);
  }
  if (missing.length === Object.keys(ITERM_KEY).length) throw new Error("the file holds no terminal colours");
  return { colors, missing };
}
