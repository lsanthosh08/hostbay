import { describe, expect, it } from "vitest";
import { clampWidth, copyText, isJSON, rowText, windowRange } from "./grid";

describe("windowRange", () => {
  it("draws the visible rows and a margin at the top", () => {
    expect(windowRange(0, 240, 24, 100_000)).toEqual({ first: 0, end: 20 });
  });
  it("draws a window in the middle, not every row", () => {
    expect(windowRange(24_000, 240, 24, 100_000)).toEqual({ first: 990, end: 1020 });
  });
  it("stops at the last row", () => {
    expect(windowRange(2_400, 240, 24, 105)).toEqual({ first: 90, end: 105 });
    expect(windowRange(0, 240, 24, 0)).toEqual({ first: 0, end: 0 });
  });
});

describe("copy", () => {
  it("copies NULL as nothing and a value as itself", () => {
    expect(copyText({ t: "", n: true })).toBe("");
    expect(copyText({ t: "a\tb" })).toBe("a\tb");
  });
  it("copies a row tab-separated, keeping each value on one line", () => {
    expect(rowText([{ t: "1" }, { t: "a\tb\nc" }, { t: "", n: true }])).toBe("1\ta b c\t");
  });
});

it("recognises a JSON object or array, not a bare string or number", () => {
  expect(isJSON('{"a": 1}')).toBe(true);
  expect(isJSON("[1, 2]")).toBe(true);
  expect(isJSON("{not json}")).toBe(false);
  expect(isJSON("42")).toBe(false);
});

it("keeps a resized column usable", () => {
  expect(clampWidth(3)).toBe(48);
  expect(clampWidth(5000)).toBe(1200);
});
