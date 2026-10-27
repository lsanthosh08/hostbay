import { describe, expect, it } from "vitest";
import type { ActiveTerm } from "../../tabs/terminal/active";
import { followTarget } from "./follow";

const term = (sessionId: string, ptyId: string | null): ActiveTerm => ({ sessionId, ptyId: () => ptyId, bracketed: () => false, focus: () => {} });

// R73: a follow-active conversation acts on the focused pane (U25's
// activeId), through the shell in it that was focused last.
describe("the follow-active target", () => {
  it("is the focused pane's terminal", () => {
    expect(followTarget("s2", term("s2", "p2"))).toBe("p2");
  });

  it("is nothing when the focused pane is not that terminal's", () => {
    // Pane s1 (a files view, say) is focused; the last terminal is in s2.
    expect(followTarget("s1", term("s2", "p2"))).toBe("");
    expect(followTarget(null, term("s2", "p2"))).toBe("");
  });

  it("is nothing before the shell opens or after it closed", () => {
    expect(followTarget("s2", term("s2", null))).toBe("");
    expect(followTarget("s2", null)).toBe("");
  });
});
