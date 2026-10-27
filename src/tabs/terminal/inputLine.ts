// Command history is read from xterm's buffer when Enter is pressed (KTD39):
// from the column where input began to the end of the line, so a
// Tab-completed or Up-recalled line is recorded as the shell showed it.
// TermPane leaves it off on terminals where Go adds local echo, where the
// screen shows typed text whatever the device does.

/** The part of xterm's IBuffer this reads. */
export type BufferLike = {
  cursorX: number;
  cursorY: number;
  baseY: number;
  getLine(y: number): { isWrapped: boolean; translateToString(trimRight?: boolean, start?: number): string } | undefined;
};

/** Text from (row, col) to the end of its line, wrapped rows included. */
function textFrom(buf: BufferLike, row: number, col: number): string {
  let text = buf.getLine(row)?.translateToString(true, col) ?? "";
  for (let r = row + 1; buf.getLine(r)?.isWrapped; r++) text += buf.getLine(r)!.translateToString(true);
  return text;
}

/** Follows one terminal's typing and reads each entered line. */
export class InputLine {
  private start: { row: number; col: number; before: string } | null = null;

  /** Call with each onData before it is sent. At Enter it returns the line
   *  to record, or null: nothing was typed, or typing changed nothing on
   *  screen, which is how a line typed while echo was off looks. */
  onData(data: string, buf: BufferLike): string | null {
    const enter = data.includes("\r");
    if (!this.start && !enter) {
      const row = buf.baseY + buf.cursorY;
      this.start = { row, col: buf.cursorX, before: textFrom(buf, row, buf.cursorX) };
    }
    if (data.includes("\x03") || data.includes("\x04")) {
      this.start = null;
      return null;
    }
    if (!enter) return null;
    const s = this.start;
    this.start = null;
    if (!s) return null;
    const now = textFrom(buf, s.row, s.col);
    return now === s.before ? null : now.trim() || null;
  }

  /** The line being typed, or null before the first key: its text, whether
   *  typing changed the screen (false while echo is off) and whether the
   *  cursor is at its end. */
  current(buf: BufferLike): { text: string; echoed: boolean; atEnd: boolean } | null {
    const s = this.start;
    if (!s) return null;
    const text = textFrom(buf, s.row, s.col);
    return { text, echoed: text !== s.before, atEnd: textFrom(buf, buf.baseY + buf.cursorY, buf.cursorX) === "" };
  }
}

/** What a quick command sends: Run is the text and Enter, Paste the text
 *  alone. Several lines go as one bracketed paste when the shell asked for
 *  bracketed paste, so none of them runs on its own. */
export function quickBytes(text: string, mode: "run" | "paste", bracketed: boolean): string {
  const body = text.replace(/\r?\n$/, "").replace(/\r?\n/g, "\r");
  const sent = bracketed && body.includes("\r") ? `\x1b[200~${body}\x1b[201~` : body;
  return mode === "run" ? sent + "\r" : sent;
}
