import { de } from "./de";
import { en } from "./en";
import { es } from "./es";
import { fr } from "./fr";
import { ja } from "./ja";
import { ko } from "./ko";
import { pt } from "./pt";
import { ru } from "./ru";
import { vi } from "./vi";
import { zhCN } from "./zh-CN";
import { zhTW } from "./zh-TW";

// Flat dot keys; each locale is a table like en.ts (R68, KTD41).
export type MessageKey = keyof typeof en;

/** The interface languages, by tag, with each one's own name. */
export const LANGUAGES = [
  { id: "zh-CN", name: "简体中文" },
  { id: "zh-TW", name: "繁體中文" },
  { id: "en", name: "English" },
  { id: "ja", name: "日本語" },
  { id: "ko", name: "한국어" },
  { id: "de", name: "Deutsch" },
  { id: "es", name: "Español" },
  { id: "fr", name: "Français" },
  { id: "ru", name: "Русский" },
  { id: "pt", name: "Português" },
  { id: "vi", name: "Tiếng Việt" },
] as const;

const TABLES: Record<string, Partial<Record<MessageKey, string>>> = {
  "zh-CN": zhCN,
  "zh-TW": zhTW,
  ja,
  ko,
  de,
  es,
  fr,
  ru,
  pt,
  vi,
};

/** The language to start in: the saved choice when it is supported, else
 *  the first supported OS language, else English. Chinese maps to
 *  Simplified or Traditional by script or region. */
export function pickLanguage(saved: string | null, os: readonly string[]): string {
  const supported = (tag: string) => LANGUAGES.some((l) => l.id === tag);
  if (saved && supported(saved)) return saved;
  for (const tag of os) {
    const lower = tag.toLowerCase();
    if (lower.startsWith("zh")) return /hant|-tw|-hk|-mo/.test(lower) ? "zh-TW" : "zh-CN";
    const base = lower.split("-")[0];
    if (supported(base)) return base;
  }
  return "en";
}

function startLanguage(): string {
  let saved: string | null = null;
  try {
    saved = localStorage.getItem("language");
  } catch {
    // no storage: follow the OS
  }
  return pickLanguage(saved, typeof navigator === "undefined" ? [] : navigator.languages ?? [navigator.language]);
}

/** The language this window runs in. It is chosen once at start, from the
 *  copy of the setting in localStorage, so a change applies after restart. */
export const language = startLanguage();
const table = TABLES[language] ?? {};
if (typeof document !== "undefined") document.documentElement.lang = language;

/** `t` for a count: uses the key's ".one" variant, when en.ts has one, for exactly 1. */
export function tn(key: MessageKey, n: number, vars?: Record<string, string | number>): string {
  const one = `${key}.one` as MessageKey;
  return t(n === 1 && one in en ? one : key, { count: n, ...vars });
}

/** The string for `key` in the current language, or in English when the
 *  table lacks it, with {name} placeholders filled from `vars`. A key
 *  missing from en.ts shows itself (and fails src/i18n/i18n.test.ts). */
export function t(key: MessageKey, vars?: Record<string, string | number>): string {
  const s: string = table[key] ?? (en as Record<string, string>)[key] ?? key;
  return vars ? s.replace(/\{(\w+)\}/g, (m, k: string) => (k in vars ? String(vars[k]) : m)) : s;
}
