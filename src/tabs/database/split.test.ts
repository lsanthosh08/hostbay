import { describe, expect, it } from "vitest";
import { clampEditor, DEFAULT_EDITOR, MIN_EDITOR, MIN_RESULTS } from "./split";

describe("clampEditor", () => {
  it("keeps a height that leaves both parts their minimum", () => {
    expect(clampEditor(200, 700)).toBe(200);
  });

  it("never shrinks the editor below its minimum", () => {
    expect(clampEditor(10, 700)).toBe(MIN_EDITOR);
    expect(clampEditor(-50, 700)).toBe(MIN_EDITOR);
  });

  it("leaves the results their minimum height", () => {
    expect(clampEditor(900, 700)).toBe(700 - MIN_RESULTS);
  });

  it("gives the editor its minimum in a space too small for both", () => {
    expect(clampEditor(300, 200)).toBe(MIN_EDITOR);
  });

  it("takes the height as asked before the space is measured", () => {
    expect(clampEditor(DEFAULT_EDITOR, 0)).toBe(DEFAULT_EDITOR);
    expect(clampEditor(10, 0)).toBe(MIN_EDITOR);
  });

  it("reaches the largest height by asking for infinity", () => {
    expect(clampEditor(Infinity, 700)).toBe(700 - MIN_RESULTS);
  });

  it("falls back to the default for a height that is not a number", () => {
    expect(clampEditor(Number.NaN, 700)).toBe(DEFAULT_EDITOR);
  });
});
