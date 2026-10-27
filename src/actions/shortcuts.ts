// Keyboard shortcuts as data, matched by a pure function. A binding is
// "Mod+…+Key" with Meta (Cmd), Ctrl, Alt and Shift as modifiers, in that
// order. Key is a Latin letter, matched by the character it types so it
// follows the keyboard layout, or else a KeyboardEvent code (Digit1, Comma,
// Tab, KeyK for Option+K on macOS), matched by the key's position (KTD40).
import type { MessageKey } from "../i18n";

export type Platform = "mac" | "other";

export type Shortcut = {
  mac?: string;
  other?: string;
  /** Fires even while a terminal or an input has focus: true on every
   *  platform, "mac" on macOS only. Anything else leaves keys typed there to
   *  the terminal, so on Windows and Linux a global binding is never Ctrl plus
   *  one letter, a terminal control key. */
  global?: boolean | "mac";
};

/** The user's bindings by action ID (the keyboard settings group): a
 *  platform's binding replaces the default there, and "" unbinds it. */
export type KeyOverrides = Record<string, { mac?: string | null; other?: string | null } | undefined>;

export type KeyInput = Pick<KeyboardEvent, "key" | "code" | "metaKey" | "ctrlKey" | "shiftKey" | "altKey">;

export const platform: Platform =
  typeof navigator !== "undefined" && /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent) ? "mac" : "other";

/** True while the keybinding editor records a key: no shortcut fires. */
export const recording = { on: false };

/** The binding a key press writes, or null for a modifier alone or a dead
 *  key. Option on macOS changes the letter typed, so a letter pressed with
 *  it goes by code. */
export function bindingOf(e: KeyInput, on: Platform): string | null {
  if (!e.code || e.key === "Dead" || /^(Meta|Control|Alt|AltGraph|Shift|CapsLock|OS|Process|Unidentified)$/.test(e.key)) return null;
  const letter = /^[a-z]$/i.test(e.key) && !(on === "mac" && e.altKey);
  const mods = [e.metaKey && "Meta", e.ctrlKey && "Ctrl", e.altKey && "Alt", e.shiftKey && "Shift"].filter(Boolean);
  return [...mods, letter ? e.key.toUpperCase() : e.code].join("+");
}

// A letter binding also answers its key's position when the key typed no
// Latin letter (Option on macOS, or a non-Latin layout).
const fires = (binding: string | undefined, pressed: string | null) =>
  !!binding && !!pressed && (pressed === binding || pressed.replace(/Key([A-Z])$/, "$1") === binding);

export const matches = (binding: string, e: KeyInput, on: Platform) => fires(binding, bindingOf(e, on));

/** The action a key press triggers. `typing` is true while focus is in a
 *  terminal or an input, where only global shortcuts fire. */
export function findAction<A extends { shortcut?: Shortcut }>(
  actions: A[],
  e: KeyInput,
  on: Platform,
  typing: boolean
): A | undefined {
  if (recording.on) return undefined;
  const pressed = bindingOf(e, on);
  return actions.find((a) => {
    const s = a.shortcut;
    if (!fires(s?.[on], pressed)) return false;
    return !typing || s!.global === true || s!.global === on;
  });
}

/** The action other than `except` that the key press already triggers. */
export function holder<A extends { id: string; shortcut?: Shortcut }>(actions: A[], e: KeyInput, on: Platform, except: string) {
  const pressed = bindingOf(e, on);
  return actions.find((a) => a.id !== except && fires(a.shortcut?.[on], pressed));
}

/** Why `binding` cannot be shortcut `s` on this platform, or null. */
export function refusal(binding: string, s: Shortcut | undefined, on: Platform): MessageKey | null {
  return on === "other" && s?.global === true && /^Ctrl\+[A-Z]$/.test(binding) ? "keys.refuseCtrlLetter" : null;
}

/** Shortcut s with the user's bindings over it. */
export function overridden(s: Shortcut | undefined, o: KeyOverrides[string]): Shortcut | undefined {
  if (!o) return s;
  const out: Shortcut = { ...s };
  for (const p of ["mac", "other"] as const) if (o[p] != null) out[p] = o[p] || undefined;
  return out;
}

/** keys with action id bound to `binding` on one platform: "" unbinds it
 *  there, and null puts its default back. The other platform keeps its own. */
export function setKey(keys: KeyOverrides, id: string, on: Platform, binding: string | null): KeyOverrides {
  const o = { ...keys[id], [on]: binding };
  if (binding === null) delete o[on];
  const next = { ...keys, [id]: o };
  if (o.mac == null && o.other == null) delete next[id];
  return next;
}

const MAC_GLYPH: Record<string, string> = { Meta: "⌘", Ctrl: "⌃", Alt: "⌥", Shift: "⇧" };
const KEY_GLYPH: Record<string, string> = {
  Comma: ",", Period: ".", Slash: "/", Backslash: "\\", Semicolon: ";", Quote: "'", Backquote: "`",
  BracketLeft: "[", BracketRight: "]", Minus: "-", Equal: "=",
  ArrowLeft: "←", ArrowRight: "→", ArrowUp: "↑", ArrowDown: "↓",
};

/** A binding as the platform writes it: ⌘K on macOS, Ctrl+Shift+P elsewhere. */
export function formatShortcut(binding: string, on: Platform = platform): string {
  const parts = binding.split("+").map((p, i, all) =>
    i < all.length - 1 ? (on === "mac" ? MAC_GLYPH[p] ?? p : p) : KEY_GLYPH[p] ?? p.replace(/^(Key|Digit)(.)$/, "$2")
  );
  return parts.join(on === "mac" ? "" : "+");
}
