// Completion while typing (R62): ghost text after the cursor, from the
// host's history (U43) or, when the user turned AI suggestions on, from the
// AI. Accepting one only types it; Enter stays the user's.

// A control or invisible character, which accepting would type: Enter
// included, and bidi controls and zero-width characters, which would not
// show as they run.
const unsafe = /[\u0000-\u001f\u007f-\u009f­؜​-‏‪-‮⁠⁦-⁩﻿]/;

/** The rest of the newest history entry that extends line, or "". */
export function historySuggestion(line: string, history: string[]): string {
  if (!line) return "";
  for (let i = history.length - 1; i >= 0; i--) {
    const h = history[i];
    if (h.length > line.length && h.startsWith(line)) return unsafe.test(h) ? "" : h.slice(line.length);
  }
  return "";
}

/** The rest of an AI-completed line, while it still extends line. */
export function completionRest(line: string, completed: string): string {
  return completed.length > line.length && completed.startsWith(line) && !unsafe.test(completed) ? completed.slice(line.length) : "";
}

/** Whether the AI may be asked about this line (KTD39): only when the user
 *  turned suggestions on, never where Go adds local echo, never on the
 *  alternate screen (normal false: an editor or pager holds the terminal),
 *  and never from a line whose typing changed nothing on screen, as one
 *  typed with echo off looks. */
export function mayAskAI(o: { enabled: boolean; localEcho: boolean; echoed: boolean; normal: boolean; line: string }): boolean {
  return o.enabled && !o.localEcho && o.echoed && o.normal && o.line.trim() !== "";
}

/** Right Arrow at the end of the line accepts a suggestion, only while one
 *  shows; every other key, Tab included, goes to the shell. */
export function accepts(e: { key: string; shiftKey?: boolean; ctrlKey?: boolean; altKey?: boolean; metaKey?: boolean }, ghost: string, atEnd: boolean): boolean {
  return e.key === "ArrowRight" && !e.shiftKey && !e.ctrlKey && !e.altKey && !e.metaKey && ghost !== "" && atEnd;
}

/** How long typing pauses before the AI is asked. */
export const AI_PAUSE_MS = 300;
