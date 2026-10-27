import { createContext, useContext, useEffect, useState, type ReactNode } from "react";
import { loadSettings, setAppearance } from "../state/settings";

// UI themes are [data-theme] token blocks in styles.css. index.html's boot
// script applies the saved one before React loads; keep its list in step.
export const THEMES = [
  { id: "dark", picker: true },
  { id: "deep-blue", picker: true },
  { id: "light", picker: true },
] as const;
export type Theme = (typeof THEMES)[number]["id"];

const isTheme = (v: string | null): v is Theme => THEMES.some((t) => t.id === v);

// xterm and Monaco read the tokens once, so they restyle on this signal.
const listeners = new Set<() => void>();
export function onThemeChange(fn: () => void) {
  listeners.add(fn);
  return () => {
    listeners.delete(fn);
  };
}

type Ctx = { theme: Theme; setTheme: (t: Theme) => void; toggle: () => void };
const ThemeCtx = createContext<Ctx>({ theme: "dark", setTheme: () => {}, toggle: () => {} });
export const useTheme = () => useContext(ThemeCtx);

export function ThemeProvider({ children }: { children: ReactNode }) {
  // localStorage's copy paints first; the synced theme follows (KTD37).
  const [theme, show] = useState<Theme>(() => {
    const saved = localStorage.getItem("theme");
    return isTheme(saved) ? saved : "dark";
  });
  useEffect(() => {
    loadSettings().then(
      (s) => isTheme(s.theme ?? null) && show(s.theme as Theme),
      () => {}
    );
  }, []);
  const setTheme = (t: Theme) => {
    show(t);
    setAppearance({ theme: t }).catch(() => {});
  };
  useEffect(() => {
    const root = document.documentElement;
    root.dataset.theme = theme;
    // The boot script's inline background would outlive a theme switch.
    root.style.background = "";
    localStorage.setItem("theme", theme);
    listeners.forEach((fn) => fn());
  }, [theme]);
  // Cycles through the themes the picker offers.
  const toggle = () => {
    const shown = THEMES.filter((t) => t.picker);
    setTheme(shown[(shown.findIndex((t) => t.id === theme) + 1) % shown.length].id);
  };
  return <ThemeCtx.Provider value={{ theme, setTheme, toggle }}>{children}</ThemeCtx.Provider>;
}
