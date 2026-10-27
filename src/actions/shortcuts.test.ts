import { describe, expect, it } from "vitest";
import { actions } from "./registry";
import {
  bindingOf,
  findAction,
  formatShortcut,
  holder,
  overridden,
  recording,
  refusal,
  setKey,
  type KeyInput,
  type KeyOverrides,
} from "./shortcuts";

// A key press: `key` is what the layout typed, `code` the physical key
// (US positions unless given).
const key = (k: string, mods: Partial<Omit<KeyInput, "key">> = {}): KeyInput => ({
  key: k,
  code: /^[a-z]$/i.test(k) ? `Key${k.toUpperCase()}` : /^\d$/.test(k) ? `Digit${k}` : k,
  metaKey: false,
  ctrlKey: false,
  shiftKey: false,
  altKey: false,
  ...mods,
});

describe("shortcuts", () => {
  it("opens the palette with Cmd+K on macOS", () => {
    expect(findAction(actions, key("k", { metaKey: true }), "mac", false)?.id).toBe("palette.open");
  });

  it("opens the palette with Ctrl+Shift+P on Windows and Linux", () => {
    expect(findAction(actions, key("P", { ctrlKey: true, shiftKey: true }), "other", false)?.id).toBe("palette.open");
    expect(findAction(actions, key("k", { metaKey: true }), "other", false)).toBeUndefined();
  });

  it("opens the palette from a terminal or an input, since it is global", () => {
    expect(findAction(actions, key("k", { metaKey: true }), "mac", true)?.id).toBe("palette.open");
    expect(findAction(actions, key("P", { ctrlKey: true, shiftKey: true }), "other", true)?.id).toBe("palette.open");
  });

  it("maps Mod+3 to the third tab, by the key's position", () => {
    expect(findAction(actions, key("3", { metaKey: true }), "mac", false)?.id).toBe("tab.3");
    expect(findAction(actions, key("3", { ctrlKey: true }), "other", false)?.id).toBe("tab.3");
    // AZERTY types " on that key unshifted.
    expect(findAction(actions, { ...key('"', { metaKey: true }), code: "Digit3" }, "mac", false)?.id).toBe("tab.3");
  });

  it("lets Ctrl+K reach a focused terminal on Windows and Linux", () => {
    expect(findAction(actions, key("k", { ctrlKey: true }), "other", true)).toBeUndefined();
  });

  it("does not swallow a non-global shortcut typed in a terminal or an input", () => {
    // Ctrl+3 is ESC in a terminal; it switches tabs only outside one.
    expect(findAction(actions, key("3", { ctrlKey: true }), "other", true)).toBeUndefined();
    expect(findAction(actions, key("3", { metaKey: true }), "mac", true)?.id).toBe("tab.3");
  });

  it("never makes Ctrl plus one letter a global default on Windows and Linux", () => {
    const bad = actions.filter((a) => {
      const s = a.shortcut;
      return s?.other && s.global === true && /^Ctrl\+[A-Za-z]$/.test(s.other);
    });
    expect(bad.map((a) => a.id)).toEqual([]);
  });

  it("writes every default as a recorded key would, with no two actions on one key", () => {
    for (const on of ["mac", "other"] as const) {
      const bound = actions.map((a) => a.shortcut?.[on]).filter((b): b is string => !!b);
      for (const b of bound) expect(b).toMatch(/^(Meta\+)?(Ctrl\+)?(Alt\+)?(Shift\+)?([A-Z]|[A-Z][A-Za-z0-9]+)$/);
      expect(new Set(bound).size).toBe(bound.length);
    }
  });

  it("formats a shortcut for each platform", () => {
    expect(formatShortcut("Meta+K", "mac")).toBe("⌘K");
    expect(formatShortcut("Ctrl+Shift+P", "other")).toBe("Ctrl+Shift+P");
    expect(formatShortcut("Meta+Digit1", "mac")).toBe("⌘1");
    expect(formatShortcut("Alt+KeyK", "mac")).toBe("⌥K");
    expect(formatShortcut("Ctrl+Comma", "other")).toBe("Ctrl+,");
  });
});

describe("recording a binding", () => {
  it("stores a letter by the key it types, and Ctrl+Z still matches on AZERTY", () => {
    expect(bindingOf(key("z", { ctrlKey: true }), "other")).toBe("Ctrl+Z");
    // AZERTY: the key that types z sits where QWERTY has W.
    const azerty = { ...key("z", { ctrlKey: true }), code: "KeyW" };
    expect(bindingOf(azerty, "other")).toBe("Ctrl+Z");
  });

  it("stores Option+K on macOS by code, and that press fires it again", () => {
    const optK = { ...key("˚", { altKey: true, metaKey: true }), code: "KeyK" };
    const b = bindingOf(optK, "mac")!;
    expect(b).toBe("Meta+Alt+KeyK");
    const keys = setKey({}, "panel.toggle", "mac", b);
    const bound = actions.map((a) => ({ ...a, shortcut: overridden(a.shortcut, keys[a.id]) }));
    expect(findAction(bound, optK, "mac", false)?.id).toBe("panel.toggle");
  });

  it("never stores a dead key or a modifier alone", () => {
    expect(bindingOf({ ...key("Dead", { altKey: true }), code: "KeyE" }, "mac")).toBeNull();
    expect(bindingOf({ ...key("Shift", { shiftKey: true }), code: "ShiftLeft" }, "other")).toBeNull();
  });

  it("fires no shortcut while recording", () => {
    recording.on = true;
    try {
      expect(findAction(actions, key("k", { metaKey: true }), "mac", false)).toBeUndefined();
    } finally {
      recording.on = false;
    }
    expect(findAction(actions, key("k", { metaKey: true }), "mac", false)?.id).toBe("palette.open");
  });

  it("refuses Ctrl+L as a global binding on Windows and Linux, with the reason", () => {
    const palette = actions.find((a) => a.id === "palette.open")!;
    expect(refusal("Ctrl+L", palette.shortcut, "other")).toBe("keys.refuseCtrlLetter");
    expect(refusal("Ctrl+Shift+L", palette.shortcut, "other")).toBeNull();
    expect(refusal("Ctrl+L", palette.shortcut, "mac")).toBeNull();
    const tab = actions.find((a) => a.id === "tab.next")!;
    expect(refusal("Ctrl+L", { ...tab.shortcut, global: false }, "other")).toBeNull();
  });

  it("names the action that already holds a key", () => {
    expect(holder(actions, key("b", { metaKey: true }), "mac", "palette.open")?.id).toBe("panel.toggle");
    expect(holder(actions, key("b", { metaKey: true }), "mac", "panel.toggle")).toBeUndefined();
  });

  it("overrides one platform only, and resets one action or all of them", () => {
    let keys: KeyOverrides = setKey({}, "palette.open", "mac", "Meta+Alt+KeyP");
    keys = setKey(keys, "tab.next", "other", "");
    const palette = actions.find((a) => a.id === "palette.open")!;
    const s = overridden(palette.shortcut, keys["palette.open"]);
    expect(s?.mac).toBe("Meta+Alt+KeyP");
    expect(s?.other).toBe(palette.shortcut?.other);
    expect(s?.global).toBe(true);
    expect(overridden(actions.find((a) => a.id === "tab.next")!.shortcut, keys["tab.next"])?.other).toBeUndefined();
    expect(setKey(keys, "palette.open", "mac", null)).toEqual({ "tab.next": { other: "" } });
    expect(setKey(setKey(keys, "palette.open", "mac", null), "tab.next", "other", null)).toEqual({});
  });
});
