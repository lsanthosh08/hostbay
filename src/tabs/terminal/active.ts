import * as api from "../../api";
import type { MessageKey } from "../../i18n";
import { quickBytes } from "./inputLine";

/** A terminal pane the quick-commands and history panels act on: the one
 *  last focused, while it is mounted. */
export type ActiveTerm = {
  /** The session (pane) it belongs to. */
  sessionId: string;
  /** null once the terminal has closed. */
  ptyId(): string | null;
  /** Whether the shell asked for bracketed paste. */
  bracketed(): boolean;
  focus(): void;
};

let current: ActiveTerm | null = null;
const listeners = new Set<() => void>();
const changed = () => listeners.forEach((f) => f());

export const activeTerminal = () => current;

/** Tells listeners the active terminal's state changed, such as its shell
 *  opening after it took focus. */
export const notifyActiveTerminal = changed;

export function setActiveTerminal(t: ActiveTerm) {
  if (current === t) return;
  current = t;
  changed();
}

export function clearActiveTerminal(t: ActiveTerm) {
  if (current !== t) return;
  current = null;
  changed();
}

/** Calls f whenever the active terminal changes; returns the unsubscribe. */
export function onActiveTerminal(f: () => void) {
  listeners.add(f);
  return () => void listeners.delete(f);
}

/** Sends text to the active terminal as Run or Paste does. With no active
 *  terminal, or a closed one, it sends nothing, queues nothing, and returns
 *  why. */
export async function sendToActive(text: string, mode: "run" | "paste"): Promise<MessageKey | null> {
  const t = current;
  if (!t) return "quick.noTerminal";
  const id = t.ptyId();
  if (!id) return "quick.disconnected";
  await api.ptyWrite(id, quickBytes(text, mode, t.bracketed()));
  t.focus();
  return null;
}
