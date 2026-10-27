import { backgroundGet, settingsGet, settingsImportLegacy, settingsSet } from "../api";
import type { KeyOverrides } from "../actions/shortcuts";
import type { CustomTheme } from "../theme/terminal";

// Synced settings live in Go (KTD37). They load once at start; localStorage
// keeps copies of the theme and language, read synchronously before they
// arrive, for the first paint and for t().
export type Appearance = {
  theme?: string;
  fontSize?: number;
  /** "" or unset matches the UI theme (KTD41). */
  terminalTheme?: string;
  customThemes?: CustomTheme[];
  /** The background image's opacity in percent; unset is the default. */
  backgroundOpacity?: number;
};
export type Settings = Appearance & { language?: string; keys?: KeyOverrides };

let current: Settings = {};
let loaded: Promise<Settings> | undefined;

// The terminal restyles when the appearance changes.
const listeners = new Set<() => void>();
export function onSettingsChange(fn: () => void) {
  listeners.add(fn);
  return () => {
    listeners.delete(fn);
  };
}

/** The settings as last loaded or set; empty until loadSettings resolves. */
export const settings = (): Settings => current;

/** Loads the settings once; later calls return the same promise. */
export function loadSettings(): Promise<Settings> {
  loaded ??= (async () => {
    // First start after the upgrade: hand Go what localStorage held. Go
    // does nothing once any setting exists.
    await settingsImportLegacy(localStorage.getItem("theme") ?? "", Number(localStorage.getItem("termFontSize")) || 0);
    const [ap, lang, kb] = await Promise.all([settingsGet("appearance"), settingsGet("language"), settingsGet("keyboard")]);
    current = {
      theme: ap.theme,
      fontSize: ap.fontSize,
      terminalTheme: ap.terminalTheme,
      customThemes: ap.customThemes as CustomTheme[] | undefined,
      backgroundOpacity: ap.backgroundOpacity,
      language: lang.language,
      keys: kb.keys ?? {},
    };
    if (current.theme) localStorage.setItem("theme", current.theme);
    if (current.language) localStorage.setItem("language", current.language);
    listeners.forEach((fn) => fn());
    return current;
  })().catch((e) => {
    loaded = undefined; // the next call tries again
    throw e;
  });
  return loaded;
}

/** Saves an appearance change, keeping the group's other fields. It fails
 *  when they cannot be loaded: a partial group would erase them. */
export async function setAppearance(change: Appearance) {
  await loadSettings();
  const next = { ...current, ...change };
  await settingsSet({
    id: "appearance",
    theme: next.theme,
    fontSize: next.fontSize,
    terminalTheme: next.terminalTheme,
    customThemes: next.customThemes,
    backgroundOpacity: next.backgroundOpacity,
  });
  current = next;
  if (next.theme) localStorage.setItem("theme", next.theme);
  listeners.forEach((fn) => fn());
}

// The terminal background image, a data: URL Go hands over (KTD41); "" is none.
let image = "";
let imageLoaded: Promise<string> | undefined;

/** The background image as last loaded or chosen. */
export const backgroundImage = () => image;

/** Loads the background image once; later calls return the same promise. */
export function loadBackground(): Promise<string> {
  imageLoaded ??= backgroundGet().then(
    (url) => showBackground(url),
    () => ""
  );
  return imageLoaded;
}

/** Shows `url` (from Go's pick, or "" once cleared) behind every terminal. */
export function showBackground(url: string) {
  image = url;
  imageLoaded = Promise.resolve(url);
  listeners.forEach((fn) => fn());
  return url;
}

/** Saves the interface language. t() reads the copy at the next start. */
export async function setLanguage(language: string) {
  await settingsSet({ id: "language", language });
  current = { ...current, language };
  localStorage.setItem("language", language);
}

/** Saves the keybinding overrides (KTD40). */
export async function setKeys(keys: KeyOverrides) {
  await settingsSet({ id: "keyboard", keys });
  current = { ...current, keys };
}
