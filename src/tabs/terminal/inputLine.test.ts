import { describe, expect, it } from "vitest";
import { InputLine, quickBytes, type BufferLike } from "./inputLine";

/** A screen of plain rows with a cursor, standing in for xterm's buffer. */
function screen(rows: string[], cursorX: number, cursorY = rows.length - 1) {
  const s = { rows, wrapped: new Set<number>(), cursorX, cursorY };
  const buf: BufferLike = {
    get cursorX() {
      return s.cursorX;
    },
    get cursorY() {
      return s.cursorY;
    },
    baseY: 0,
    getLine: (y) =>
      y < s.rows.length
        ? {
            isWrapped: s.wrapped.has(y),
            translateToString: (_trim?: boolean, start = 0) => s.rows[y].slice(start).replace(/\s+$/, ""),
          }
        : undefined,
  };
  return { s, buf };
}

describe("InputLine", () => {
  it("records what the shell showed, from where input began", () => {
    const { s, buf } = screen(["$ "], 2);
    const line = new InputLine();
    expect(line.onData("l", buf)).toBeNull();
    s.rows[0] = "$ ls -la";
    s.cursorX = 8;
    expect(line.onData("\r", buf)).toBe("ls -la");
  });

  it("skips a line typed while echo was off", () => {
    const { buf } = screen(["[sudo] password for root: "], 26);
    const line = new InputLine();
    for (const k of "hunter2") line.onData(k, buf);
    expect(line.onData("\r", buf)).toBeNull();
  });

  it("records a Tab-completed line as the shell completed it", () => {
    const { s, buf } = screen(["$ "], 2);
    const line = new InputLine();
    line.onData("s", buf);
    line.onData("y", buf);
    line.onData("\t", buf);
    s.rows[0] = "$ systemctl ";
    expect(line.onData("\r", buf)).toBe("systemctl");
  });

  it("records an Up-recalled line, wrapped over two rows", () => {
    const { s, buf } = screen(["$ "], 2);
    const line = new InputLine();
    line.onData("\x1b[A", buf);
    s.rows = ["$ docker compose -f prod.yml", "up -d"];
    s.wrapped.add(1);
    s.cursorY = 1;
    expect(line.onData("\r", buf)).toBe("docker compose -f prod.ymlup -d");
  });

  it("starts again after Ctrl+C", () => {
    const { s, buf } = screen(["$ "], 2);
    const line = new InputLine();
    line.onData("x", buf);
    s.rows = ["$ x^C", "$ "];
    s.cursorY = 1;
    expect(line.onData("\x03", buf)).toBeNull();
    line.onData("p", buf);
    s.rows[1] = "$ pwd";
    expect(line.onData("\r", buf)).toBe("pwd");
  });
});

describe("quickBytes", () => {
  it("sends one line with Enter", () => {
    expect(quickBytes("uptime\n", "run", true)).toBe("uptime\r");
  });
  it("sends several lines as one bracketed paste, then Enter", () => {
    expect(quickBytes("cd /srv\nls\n", "run", true)).toBe("\x1b[200~cd /srv\rls\x1b[201~\r");
  });
  it("pastes without Enter, and without brackets the shell did not ask for", () => {
    expect(quickBytes("cd /srv\nls", "paste", false)).toBe("cd /srv\rls");
  });
});
