import { afterEach, describe, expect, it, vi } from "vitest";
import { focusReturn, focusTarget } from "./Dialog";

// Just enough of an element for focusTarget: a parent chain and isConnected.
const node = (parentElement: unknown = null, isConnected = true) => ({ parentElement, isConnected }) as unknown as HTMLElement;
const focused = (el: HTMLElement) => vi.stubGlobal("document", { activeElement: el });

afterEach(() => vi.unstubAllGlobals());

describe("focusTarget", () => {
  it("gives focus back to the element focused at open", () => {
    const field = node();
    focused(field);
    expect(focusTarget()()).toBe(field);
  });

  // Cmd+K, Save workspace: the prompt opens as the palette closes, and the
  // palette's input it opened from is gone by the time the prompt closes.
  it("goes where the closing palette would have, once the element it opened from is gone", () => {
    const terminal = node();
    const palette = node();
    focusReturn.set(palette as unknown as Element, () => terminal);
    const input = node(palette, false);
    focused(input);
    expect(focusTarget()()).toBe(terminal);
  });

  it("stays inside a dialog that is still open", () => {
    const editor = node();
    focusReturn.set(editor as unknown as Element, () => node());
    const field = node(editor, true);
    focused(field);
    expect(focusTarget()()).toBe(field);
  });
});
