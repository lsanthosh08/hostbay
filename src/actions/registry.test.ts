import { expect, it, vi } from "vitest";
import { en } from "../i18n/en";
import { actions, bound, editable, keysFor } from "./registry";
import { platform } from "./shortcuts";

// The user's bindings, as the keyboard settings group loads them.
const keys: Record<string, { mac?: string; other?: string }> = {};
vi.mock("../state/settings", () => ({ settings: () => ({ keys }) }));

it("gives every action a unique ID", () => {
  const ids = actions.map((a) => a.id);
  expect(new Set(ids).size).toBe(ids.length);
});

it("gives every action a label key defined in en.ts", () => {
  expect(actions.filter((a) => !(a.label in en)).map((a) => a.id)).toEqual([]);
});

it("registers the palette and the tab shortcuts", () => {
  const ids = actions.map((a) => a.id);
  expect(ids).toContain("palette.open");
  for (let n = 1; n <= 9; n++) expect(ids).toContain(`tab.${n}`);
});

it("offers every non-hidden action to the keybinding editor, bound or not", () => {
  const ids = editable().map((a) => a.id);
  expect(ids).toEqual(actions.filter((a) => !a.hidden).map((a) => a.id));
  expect(ids).toEqual(expect.arrayContaining(["pane.splitRight", "pane.next", "pane.unsplit", "workspace.save", "host.new"]));
});

it("merges the user's bindings over the defaults, one platform at a time", () => {
  const other = platform === "mac" ? "other" : "mac";
  keys["panel.toggle"] = { [platform]: "Alt+KeyB" };
  keys["tab.next"] = { [platform]: "" };
  try {
    const panel = bound().find((a) => a.id === "panel.toggle")!;
    expect(panel.shortcut?.[platform]).toBe("Alt+KeyB");
    expect(panel.shortcut?.[other]).toBe(actions.find((a) => a.id === "panel.toggle")!.shortcut?.[other]);
    expect(keysFor("panel.toggle")).toBe(platform === "mac" ? "⌥B" : "Alt+B");
    expect(keysFor("tab.next")).toBeUndefined();
  } finally {
    delete keys["panel.toggle"];
    delete keys["tab.next"];
  }
  expect(keysFor("tab.next")).toBe(platform === "mac" ? "⌃Tab" : "Ctrl+Tab");
});
