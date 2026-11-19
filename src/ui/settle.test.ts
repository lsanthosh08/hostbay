import { describe, expect, it } from "vitest";
import { SETTLE_MS, settled, track, waitLeft, waitProps } from "./settle";

describe("the settle guard", () => {
  it("ignores a click until the button has stayed in place a moment", () => {
    expect(settled(1000, 1000)).toBe(false);
    expect(settled(1000, 1000 + SETTLE_MS - 1)).toBe(false);
    expect(settled(1000, 1000 + SETTLE_MS)).toBe(true);
  });

  it("arms when first shown", () => {
    const m = { top: null, key: undefined, since: 0 };
    track(m, 500, undefined, 1000);
    expect(m.since).toBe(1000);
  });

  it("keeps waiting from the first render while nothing moves", () => {
    const m = { top: null, key: undefined, since: 0 };
    track(m, 500, "a", 1000);
    track(m, 500, "a", 1400);
    expect(settled(m.since, 1700)).toBe(true);
  });

  // The approval tray is pinned to the window's bottom: when the newest
  // card goes, the older ones slide down without changing their index.
  it("re-arms when the button moves on screen", () => {
    const m = { top: null, key: undefined, since: 0 };
    track(m, 500, "a", 1000);
    track(m, 620, "a", 1700);
    expect(m.since).toBe(1700);
    expect(settled(m.since, 1800)).toBe(false);
  });

  // A queued trust prompt opens in the same place as the one just answered.
  it("re-arms when what it confirms changes in place", () => {
    const m = { top: null, key: undefined, since: 0 };
    const first = {};
    track(m, 0, first, 1000);
    track(m, 0, {}, 1700);
    expect(m.since).toBe(1700);
  });

  it("says how long the button still waits, then 0", () => {
    expect(waitLeft(1000, 1000)).toBe(SETTLE_MS);
    expect(waitLeft(1000, 1200)).toBe(SETTLE_MS - 200);
    expect(waitLeft(1000, 1000 + SETTLE_MS)).toBe(0);
    expect(waitLeft(1000, 5000)).toBe(0);
  });

  // Not `disabled`: that would drop the button from the tab order.
  it("draws a waiting button as unavailable but focusable, and a ready one as usual", () => {
    expect(waitProps(true)).toEqual({ "aria-disabled": true, className: "opacity-50 cursor-not-allowed" });
    expect(waitProps(false)).toEqual({});
  });
});
