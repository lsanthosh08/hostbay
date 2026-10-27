import type { ITheme } from "@xterm/xterm";
import type { editor } from "monaco-editor";
import { PALETTE_KEYS, TERMINAL_THEMES, type Palette } from "./terminalThemes";

// xterm and Monaco take their colours from the theme tokens in styles.css,
// read again on every theme change (see onThemeChange in ui/theme.tsx).

/** Reads one CSS custom property (or property) of the current theme. */
export type TokenReader = (name: string) => string;

export const cssTokens: TokenReader = (name) =>
  getComputedStyle(document.documentElement).getPropertyValue(name).trim();

/** xterm's ANSI palette keys, in colour-number order (--ansi-0 … --ansi-15). */
export const ANSI_KEYS = [
  "black",
  "red",
  "green",
  "yellow",
  "blue",
  "magenta",
  "cyan",
  "white",
  "brightBlack",
  "brightRed",
  "brightGreen",
  "brightYellow",
  "brightBlue",
  "brightMagenta",
  "brightCyan",
  "brightWhite",
] as const;

/** A custom terminal theme, kept in the synced appearance settings. */
export type CustomTheme = { id: string; name: string; type: "dark" | "light"; colors: Palette };

const HEX = /^#[0-9a-f]{6}([0-9a-f]{2})?$/i;

/** The palette a terminal theme ID names, or undefined for "Match UI
 *  theme" (""), an unknown ID, or a custom theme whose colours are not all
 *  hex (sync can carry one from another computer). */
export function terminalPalette(id: string | undefined, custom: CustomTheme[] | undefined): Palette | undefined {
  const colors = custom?.find((c) => c.id === id)?.colors ?? TERMINAL_THEMES.find((t) => t.id === id)?.colors;
  return colors && PALETTE_KEYS.every((k) => HEX.test(colors[k] ?? "")) ? colors : undefined;
}

/** xterm's theme: the chosen palette, or with none the UI theme's tokens. */
export function xtermTheme(read: TokenReader = cssTokens, palette?: Palette): ITheme {
  if (palette) {
    const { selection, ...colors } = palette;
    return { ...colors, cursorAccent: palette.background, selectionBackground: selection };
  }
  const theme: ITheme = {
    background: read("--term-bg"),
    foreground: read("--term-fg"),
    cursor: read("--term-cursor"),
    cursorAccent: read("--term-bg"),
    selectionBackground: read("--term-selection"),
  };
  ANSI_KEYS.forEach((k, i) => (theme[k] = read(`--ansi-${i}`)));
  return theme;
}

/** A terminal's xterm theme from the appearance settings. Over a background
 *  image its background turns see-through but keeps its colour, which xterm
 *  reports to programs that ask for it (OSC 11). */
export function terminalTheme(
  s: { terminalTheme?: string; customThemes?: CustomTheme[] },
  image: boolean,
  read: TokenReader = cssTokens
): ITheme {
  const theme = xtermTheme(read, terminalPalette(s.terminalTheme, s.customThemes));
  return image ? { ...theme, background: (theme.background ?? "#000000").slice(0, 7) + "00" } : theme;
}

export function monacoTheme(read: TokenReader = cssTokens): editor.IStandaloneThemeData {
  return {
    base: read("color-scheme") === "light" ? "vs" : "vs-dark",
    inherit: true,
    rules: [],
    colors: {
      "editor.background": read("--surface"),
      "editor.foreground": read("--fg"),
      "editorLineNumber.foreground": read("--muted"),
      "editorLineNumber.activeForeground": read("--fg"),
      "editorCursor.foreground": read("--accent"),
      "editor.selectionBackground": read("--selection"),
      "editor.lineHighlightBackground": read("--raised"),
      "editorWidget.background": read("--raised"),
      "editorWidget.border": read("--line"),
    },
  };
}
