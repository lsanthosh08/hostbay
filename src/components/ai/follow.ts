import type { ActiveTerm } from "../../tabs/terminal/active";

/** The terminal a follow-active AI conversation acts on (R73): the shell
 *  focused last, only while its pane is the focused one (activeId, KTD40).
 *  "" is none, so Go refuses a proposal instead of guessing. */
export function followTarget(activeId: string | null, t: ActiveTerm | null): string {
  return (t && t.sessionId === activeId && t.ptyId()) || "";
}
