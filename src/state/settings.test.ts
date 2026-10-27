import { beforeEach, describe, expect, it, vi } from "vitest";

const go = vi.hoisted(() => ({
  imports: [] as [string, number][],
  sets: [] as unknown[],
  groups: {} as Record<string, Record<string, unknown>>,
  failGet: false,
}));
vi.mock("../api", () => ({
  settingsImportLegacy: async (theme: string, fontSize: number) => {
    go.imports.push([theme, fontSize]);
    // Go imports only while no setting exists.
    if (!go.groups.appearance) go.groups.appearance = { id: "appearance", theme, fontSize };
  },
  settingsGet: async (id: string) => {
    if (go.failGet) throw new Error("bridge down");
    return go.groups[id] ?? { id };
  },
  settingsSet: async (g: { id: string }) => {
    go.sets.push(g);
    go.groups[g.id] = g;
  },
}));

const store = new Map<string, string>();
vi.stubGlobal("localStorage", {
  getItem: (k: string) => store.get(k) ?? null,
  setItem: (k: string, v: string) => void store.set(k, v),
});

beforeEach(() => {
  vi.resetModules();
  store.clear();
  go.imports.length = 0;
  go.sets.length = 0;
  go.failGet = false;
  go.groups = {};
});

describe("settings", () => {
  it("hands Go the legacy theme and font size once, and loads once", async () => {
    store.set("theme", "light");
    store.set("termFontSize", "15");
    const s = await import("./settings");
    await s.loadSettings();
    await s.loadSettings();
    expect(go.imports).toEqual([["light", 15]]);
    expect(s.settings()).toMatchObject({ theme: "light", fontSize: 15 });
  });

  it("copies Go's theme and language into localStorage", async () => {
    store.set("theme", "dark");
    go.groups = { appearance: { id: "appearance", theme: "light" }, language: { id: "language", language: "de" } };
    const s = await import("./settings");
    await s.loadSettings();
    expect(store.get("theme")).toBe("light");
    expect(store.get("language")).toBe("de");
  });

  it("keeps the other appearance fields when the theme changes", async () => {
    const custom = [{ id: "custom-1", name: "Mine", type: "dark", colors: {} }];
    go.groups = {
      appearance: { id: "appearance", theme: "dark", fontSize: 16, terminalTheme: "dracula", customThemes: custom, backgroundOpacity: 40 },
    };
    const s = await import("./settings");
    await s.setAppearance({ theme: "light" });
    expect(go.sets).toEqual([
      { id: "appearance", theme: "light", fontSize: 16, terminalTheme: "dracula", customThemes: custom, backgroundOpacity: 40 },
    ]);
    expect(store.get("theme")).toBe("light");
  });

  // A group sent without the fields it failed to load would erase them,
  // custom themes included, here and on every synced computer.
  it("refuses to save the appearance when the settings did not load", async () => {
    go.groups = { appearance: { id: "appearance", theme: "dark", customThemes: [{ id: "custom-1" }] } };
    go.failGet = true;
    const s = await import("./settings");
    await expect(s.setAppearance({ theme: "light" })).rejects.toThrow();
    expect(go.sets).toEqual([]);
    // The next save loads again.
    go.failGet = false;
    await s.setAppearance({ theme: "light" });
    expect(go.sets).toEqual([{ id: "appearance", theme: "light", customThemes: [{ id: "custom-1" }] }]);
  });

  it("saves a language change to Go and the copy t() reads at the next start", async () => {
    const s = await import("./settings");
    await s.setLanguage("ja");
    expect(go.sets).toEqual([{ id: "language", language: "ja" }]);
    expect(store.get("language")).toBe("ja");
  });

  it("tells listeners when the appearance changes", async () => {
    const s = await import("./settings");
    await s.loadSettings();
    const seen: (string | undefined)[] = [];
    s.onSettingsChange(() => seen.push(s.settings().terminalTheme));
    await s.setAppearance({ terminalTheme: "nord" });
    expect(seen).toEqual(["nord"]);
  });
});
