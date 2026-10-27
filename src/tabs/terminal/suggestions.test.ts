import { describe, expect, it } from "vitest";
import { InputLine, type BufferLike } from "./inputLine";
import { accepts, completionRest, historySuggestion, mayAskAI } from "./suggestions";

function screen(rows: string[], cursorX: number) {
  const s = { rows, cursorX };
  const buf: BufferLike = {
    get cursorX() {
      return s.cursorX;
    },
    get cursorY() {
      return s.rows.length - 1;
    },
    baseY: 0,
    getLine: (y) =>
      y < s.rows.length ? { isWrapped: false, translateToString: (_t?: boolean, start = 0) => s.rows[y].slice(start).replace(/\s+$/, "") } : undefined,
  };
  return { s, buf };
}

describe("history suggestions", () => {
  const history = ["ls -la", "systemctl status nginx", "systemctl restart nginx", "ls"];

  it("complete from the newest entry that extends the line", () => {
    expect(historySuggestion("systemctl ", history)).toBe("restart nginx");
    expect(historySuggestion("systemctl s", history)).toBe("tatus nginx");
    expect(historySuggestion("ls -", history)).toBe("la");
  });

  it("show nothing for an empty line, an exact match or no match", () => {
    expect(historySuggestion("", history)).toBe("");
    expect(historySuggestion("ls", ["ls"])).toBe("");
    expect(historySuggestion("ls", history)).toBe(" -la"); // past the exact entry to an older one
    expect(historySuggestion("docker", history)).toBe("");
  });

  it("never offer a control or invisible character, which accepting would type", () => {
    expect(historySuggestion("echo ", ["echo hi\rrm -rf ~"])).toBe("");
    expect(historySuggestion("echo ", ["echo ‮hi"])).toBe("");
  });
});

describe("AI completions", () => {
  it("show only the rest of a completion that still extends the line", () => {
    expect(completionRest("tail -f /var/log/ng", "tail -f /var/log/nginx/error.log")).toBe("inx/error.log");
    expect(completionRest("tail -f /var/log/ngi", "tail -f /var/log/nginx/error.log")).toBe("nx/error.log");
    expect(completionRest("tail -f /x", "tail -f /var/log/nginx/error.log")).toBe("");
    expect(completionRest("ls", "ls\rrm -rf ~")).toBe("");
    expect(completionRest("ls", "")).toBe("");
  });

  it("are asked for only when on, on a terminal without local echo, from a line that echoed", () => {
    const on = { enabled: true, localEcho: false, echoed: true, normal: true, line: "systemctl st" };
    expect(mayAskAI(on)).toBe(true);
    expect(mayAskAI({ ...on, enabled: false })).toBe(false);
    expect(mayAskAI({ ...on, localEcho: true })).toBe(false);
    expect(mayAskAI({ ...on, echoed: false })).toBe(false);
    expect(mayAskAI({ ...on, line: "  " })).toBe(false);
    expect(mayAskAI({ ...on, normal: false })).toBe(false); // vim, less or top: the screen is not a command line
  });
});

describe("accepting a suggestion", () => {
  it("takes Right Arrow at the end of the line only while one shows", () => {
    expect(accepts({ key: "ArrowRight" }, "tatus", true)).toBe(true);
    expect(accepts({ key: "ArrowRight" }, "", true)).toBe(false);
    expect(accepts({ key: "ArrowRight" }, "tatus", false)).toBe(false);
    expect(accepts({ key: "ArrowRight", shiftKey: true }, "tatus", true)).toBe(false);
  });

  it("leaves Tab to the shell, always", () => {
    expect(accepts({ key: "Tab" }, "tatus", true)).toBe(false);
  });
});

describe("the line being typed", () => {
  it("is read from where input began, with whether typing showed and the cursor is at its end", () => {
    const { s, buf } = screen(["$ "], 2);
    const line = new InputLine();
    expect(line.current(buf)).toBeNull();
    line.onData("s", buf);
    s.rows[0] = "$ systemctl st";
    s.cursorX = 14;
    expect(line.current(buf)).toEqual({ text: "systemctl st", echoed: true, atEnd: true });
    s.cursorX = 5;
    expect(line.current(buf)?.atEnd).toBe(false);
  });

  it("reports a line typed with echo off as not echoed", () => {
    const { buf } = screen(["Password: "], 10);
    const line = new InputLine();
    for (const k of "hunter2") line.onData(k, buf);
    expect(line.current(buf)).toEqual({ text: "", echoed: false, atEnd: true });
  });
});
