import { afterEach, describe, expect, it, vi } from "vitest";
import { pickLanguage, t, tn } from ".";
import { en } from "./en";

it("returns the English string for a key", () => {
  expect(t("hosts.empty.title")).toBe("No hosts yet");
});

it("substitutes {vars}", () => {
  expect(t("connect.connecting", { name: "web1" })).toBe("Connecting to web1");
});

it("falls back to the key when it is missing", () => {
  expect(t("no.such.key" as never)).toBe("no.such.key");
});

// A typo in a t("…") key would show the raw key on screen; fail CI instead.
it("defines every key that src/ passes to t()", () => {
  const files = import.meta.glob(["../**/*.{ts,tsx}", "!../bindings/**", "!../**/*.test.ts"], {
    query: "?raw",
    import: "default",
    eager: true,
  }) as Record<string, string>;
  expect(Object.keys(files).length).toBeGreaterThan(20);
  const missing: string[] = [];
  for (const [file, src] of Object.entries(files))
    for (const m of src.matchAll(/\bt\(\s*["'`]([^"'`]+)["'`]/g)) if (!(m[1] in en)) missing.push(`${file}: ${m[1]}`);
  expect(missing).toEqual([]);
});

const locales = import.meta.glob<Record<string, string>>(["./*.ts", "!./en.ts", "!./index.ts", "!./*.test.ts"], {
  eager: true,
  import: "*",
});
const tables = Object.fromEntries(
  Object.entries(locales).map(([file, mod]) => [file.slice(2, -3), Object.values(mod)[0] as unknown as Record<string, string>])
);
const vars = (s: string) => [...s.matchAll(/\{(\w+)\}/g)].map((m) => m[1]).sort();

describe("locales", () => {
  it("has a table for each of the ten languages beside English", () => {
    expect(Object.keys(tables).sort()).toEqual(["de", "es", "fr", "ja", "ko", "pt", "ru", "vi", "zh-CN", "zh-TW"]);
  });

  // A locale need not translate every key (the rest show in English), but
  // what it has must match en.ts: a renamed key or placeholder fails here.
  it("uses only en keys, with the same {placeholders}", () => {
    const drift: string[] = [];
    for (const [loc, table] of Object.entries(tables)) {
      expect(Object.keys(table).length, loc).toBeGreaterThan(50);
      for (const [key, s] of Object.entries(table)) {
        if (!(key in en)) drift.push(`${loc}: ${key} is not in en.ts`);
        else if (vars(s).join() !== vars((en as Record<string, string>)[key]).join()) drift.push(`${loc}: ${key} placeholders`);
        if (!s.trim()) drift.push(`${loc}: ${key} is empty`);
      }
    }
    expect(drift).toEqual([]);
  });
});

describe("language at start", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.resetModules();
  });

  async function startIn(saved: string | null) {
    vi.resetModules();
    vi.stubGlobal("localStorage", { getItem: (k: string) => (k === "language" ? saved : null), setItem() {} });
    return import(".");
  }

  it("reads the language copy, and shows English for a key the table lacks", async () => {
    const { t, language } = await startIn("de");
    expect(language).toBe("de");
    expect(t("common.cancel")).toBe(tables.de["common.cancel"]);
    const absent = Object.keys(en).find((k) => !(k in tables.de)) as keyof typeof en;
    expect(absent).toBeDefined();
    expect(t(absent)).toBe(en[absent]);
  });

  it("labels the session views in German at start", async () => {
    await startIn("de");
    // The views load Monaco, which needs a DOM; the labels do not.
    vi.doMock("../monaco", () => ({}));
    const { VIEW_INFO } = await import("../shell/SessionLayer");
    expect(VIEW_INFO.database.label).toBe("Datenbank");
    expect(VIEW_INFO.services.label).toBe(tables.de["view.services"]);
  });

  it("follows the OS on a first launch", () => {
    expect(pickLanguage(null, ["de-DE", "en-US"])).toBe("de");
    expect(pickLanguage(null, ["th-TH"])).toBe("en");
    expect(pickLanguage(null, [])).toBe("en");
    expect(pickLanguage(null, ["pt-BR"])).toBe("pt");
    expect(pickLanguage(null, ["zh-Hans-CN"])).toBe("zh-CN");
    expect(pickLanguage(null, ["zh-CN"])).toBe("zh-CN");
    expect(pickLanguage(null, ["zh-Hant-TW"])).toBe("zh-TW");
    expect(pickLanguage(null, ["zh-HK"])).toBe("zh-TW");
  });

  it("keeps a saved choice over the OS, and ignores an unsupported one", () => {
    expect(pickLanguage("fr", ["de-DE"])).toBe("fr");
    expect(pickLanguage("xx", ["de-DE"])).toBe("de");
  });
});

describe("tn", () => {
  it("uses the singular variant for exactly one and fills the count", () => {
    expect(tn("vpnImport.saved", 1)).toBe(en["vpnImport.saved.one"]);
    expect(tn("vpnImport.saved", 3)).toBe("Imported 3 VPN profiles");
    expect(tn("vpnImport.saved", 0)).toBe("Imported 0 VPN profiles");
  });
});
