import { describe, expect, it } from "vitest";
import { parseItermColors } from "./itermcolors";
import { TERMINAL_THEMES } from "./terminalThemes";

const base = TERMINAL_THEMES[0].colors;

// iTerm2's export layout: a plist dict, keys in alphabetical order, each
// colour a dict of components (alphabetical too) as 0-1 reals.
const ITERM_KEYS: Record<string, [number, number, number]> = {
  "Ansi 0 Color": [0.027451, 0.211765, 0.258824], // #073642
  "Ansi 1 Color": [0.862745, 0.196078, 0.184314], // #dc322f
  "Ansi 10 Color": [0.345098, 0.431373, 0.458824], // #586e75
  "Ansi 11 Color": [0.396078, 0.482353, 0.513725], // #657b83
  "Ansi 12 Color": [0.513725, 0.580392, 0.588235], // #839496
  "Ansi 13 Color": [0.423529, 0.443137, 0.768627], // #6c71c4
  "Ansi 14 Color": [0.576471, 0.631373, 0.631373], // #93a1a1
  "Ansi 15 Color": [0.992157, 0.964706, 0.890196], // #fdf6e3
  "Ansi 2 Color": [0.521569, 0.6, 0], // #859900
  "Ansi 3 Color": [0.709804, 0.537255, 0], // #b58900
  "Ansi 4 Color": [0.14902, 0.545098, 0.823529], // #268bd2
  "Ansi 5 Color": [0.827451, 0.211765, 0.509804], // #d33682
  "Ansi 6 Color": [0.164706, 0.631373, 0.596078], // #2aa198
  "Ansi 7 Color": [0.933333, 0.909804, 0.835294], // #eee8d5
  "Ansi 8 Color": [0, 0.168627, 0.211765], // #002b36
  "Ansi 9 Color": [0.796078, 0.294118, 0.086275], // #cb4b16
  "Background Color": [0, 0.168627, 0.211765], // #002b36
  "Bold Color": [0.576471, 0.631373, 0.631373],
  "Cursor Color": [0.576471, 0.631373, 0.631373], // #93a1a1
  "Cursor Text Color": [0, 0.168627, 0.211765],
  "Foreground Color": [0.513725, 0.580392, 0.588235], // #839496
  "Selection Color": [0.027451, 0.211765, 0.258824], // #073642
};

function exported(skip: string[] = []) {
  const entries = Object.entries(ITERM_KEYS)
    .filter(([k]) => !skip.includes(k))
    .map(
      ([k, [r, g, b]]) => `	<key>${k}</key>
	<dict>
		<key>Alpha Component</key>
		<real>1</real>
		<key>Blue Component</key>
		<real>${b}</real>
		<key>Color Space</key>
		<string>sRGB</string>
		<key>Green Component</key>
		<real>${g}</real>
		<key>Red Component</key>
		<real>${r}</real>
	</dict>`
    )
    .join("\n");
  return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
${entries}
</dict>
</plist>
`;
}

describe("itermcolors import", () => {
  it("imports an iTerm2 export with matching colours", () => {
    const { colors, missing } = parseItermColors(exported(), base);
    expect(missing).toEqual([]);
    expect(colors).toEqual({
      background: "#002b36",
      foreground: "#839496",
      cursor: "#93a1a1",
      selection: "#073642",
      black: "#073642",
      red: "#dc322f",
      green: "#859900",
      yellow: "#b58900",
      blue: "#268bd2",
      magenta: "#d33682",
      cyan: "#2aa198",
      white: "#eee8d5",
      brightBlack: "#002b36",
      brightRed: "#cb4b16",
      brightGreen: "#586e75",
      brightYellow: "#657b83",
      brightBlue: "#839496",
      brightMagenta: "#6c71c4",
      brightCyan: "#93a1a1",
      brightWhite: "#fdf6e3",
    });
  });

  it("keeps the base theme's colour for one the file lacks, and names it", () => {
    const { colors, missing } = parseItermColors(exported(["Ansi 3 Color", "Selection Color"]), base);
    expect(missing.sort()).toEqual(["selection", "yellow"]);
    expect(colors.yellow).toBe(base.yellow);
    expect(colors.selection).toBe(base.selection);
    expect(colors.red).toBe("#dc322f");
  });

  it("rejects a file that is not an itermcolors plist", () => {
    for (const bad of ["", "hello", "<html><body>x</body></html>", '<plist version="1.0"><dict></dict></plist>'])
      expect(() => parseItermColors(bad, base), JSON.stringify(bad)).toThrow();
  });
});
