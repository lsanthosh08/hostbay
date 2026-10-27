// Zmodem in an SSH terminal (R43): the page's half of internal/app/zmodem.go.
// zmodem.js speaks the protocol here; Go owns every file. A download saves
// only into the folder the user picks in Go's native dialog, under names Go
// checks; an upload reads only the files picked there. The frames are
// documented at ZmodemStreamName in internal/app/zmodem.go.
import Zmodem from "zmodem.js";
import * as api from "../api";
import { t } from "../i18n";

// Frame types, the first byte of every frame after the ptyID.
const REMOTE = 0; // raw bytes, both ways
const SAVE = 1; // page to Go: {name, size, conflict?}
const REPLY = 1; // Go to page: {name?, exists?, error?}
const DATA = 2; // page to Go: the next bytes of the file being saved
const DONE = 3; // page to Go: that file is complete
const END = 4; // page to Go: the session is over

// ZmodemReadFile's most per call.
const READ_SIZE = 1 << 20;
// An upload reads on only while less than this waits to reach Go: zmodem.js
// sends without waiting for the server, so this is the only brake.
const MAX_BUFFERED = 2 << 20;

export type ZmodemDirection = "download" | "upload";
export type ZmodemConflict = "overwrite" | "rename" | "skip";
export type ZmodemProgress = { name: string; size: number; bytes: number };
export type ZmodemResult = {
  direction: ZmodemDirection;
  /** The folder a download saved into; "" for an upload. */
  folder: string;
  /** Saved under these names, or sent. */
  done: string[];
  skipped: string[];
  failed: { name: string; error: string }[];
  outcome: "done" | "cancelled" | "stopped";
  /** Why it stopped. */
  error?: string;
};
export type ZmodemHooks = {
  /** The file moving and its bytes so far; null between files. */
  progress(p: ZmodemProgress | null): void;
  /** What to do about a download whose name is taken in the folder. */
  ask(name: string): Promise<ZmodemConflict>;
  finish(r: ZmodemResult): void;
};
type Reply = { name?: string; exists?: boolean; error?: string };

const message = (e: unknown) => (e instanceof Error ? e.message : typeof e === "string" ? e : api.errorInfo(e).message);

/** Runs the transfer that `sz` or `rz` started in terminal ptyId: asks for
 *  the folder or the files, then moves them over the "zmodem" stream. While
 *  it runs the terminal's keystrokes must not reach the pty; cancel() stops
 *  it. Once the session is over, the server's output goes to term. */
export function startZmodem(
  ptyId: string,
  direction: ZmodemDirection,
  term: { write(data: Uint8Array): void },
  hooks: ZmodemHooks
): { cancel(): void } {
  const r: ZmodemResult = { direction, folder: "", done: [], skipped: [], failed: [], outcome: "done" };
  let ws: ReturnType<typeof api.zmodemStream> | null = null;
  let current: ZmodemProgress | null = null; // the file being offered, saved or sent
  let ended = false; // END sent: the server's bytes are terminal text again
  let cancelled = false;
  let finished = false;
  const replies: ((r: Reply) => void)[] = []; // Go answers SAVE and DONE in order

  const frame = (type: number, body: ArrayLike<number> = []) => {
    const b = new Uint8Array(body.length + 1);
    b[0] = type;
    b.set(body, 1);
    ws?.send(b);
  };
  const request = (type: number, body: object) =>
    new Promise<Reply>((resolve) => {
      replies.push(resolve);
      frame(type, new TextEncoder().encode(JSON.stringify(body)));
    });
  const setCurrent = (p: ZmodemProgress | null) => {
    current = p;
    hooks.progress(p && { ...p });
  };

  const finish = () => {
    if (finished) return;
    finished = true;
    if (current) r.failed.push({ name: current.name, error: r.error ?? t("zmodem.interrupted") });
    r.outcome = r.error ? "stopped" : cancelled ? "cancelled" : "done";
    hooks.progress(null);
    hooks.finish(r);
  };
  const cancel = () => {
    if (finished || cancelled) return;
    cancelled = true;
    // Go aborts at the server and closes the stream, which finishes here.
    api.zmodemCancel(ptyId).catch(() => ws?.close()); // a stream closed without END aborts too
    if (!ws) finish();
  };
  const fail = (e: unknown) => {
    r.error ??= message(e);
    cancel();
  };
  const end = () => {
    ended = true;
    frame(END);
  };

  async function receive(offer: Zmodem.Offer) {
    const { name, size = 0 } = offer.get_details();
    current = { name, size, bytes: 0 };
    let reply = await request(SAVE, { name, size });
    if (reply.exists) {
      const choice = await hooks.ask(name);
      if (choice === "skip") {
        offer.skip();
        r.skipped.push(name);
        return setCurrent(null);
      }
      reply = await request(SAVE, { name, size, conflict: choice });
    }
    if (reply.error || !reply.name) {
      offer.skip();
      r.failed.push({ name, error: reply.error ?? "" });
      return setCurrent(null);
    }
    const p = { name: reply.name, size, bytes: 0 };
    setCurrent(p);
    await offer.accept({
      on_input: (payload) => {
        frame(DATA, payload);
        p.bytes += payload.length;
        setCurrent(p);
      },
    });
    const done = await request(DONE, {});
    if (done.error) r.failed.push({ name: p.name, error: done.error });
    else r.done.push(done.name ?? p.name);
    setCurrent(null);
  }

  async function send(session: Zmodem.Session, files: api.ZmodemFile[]) {
    const drained = () =>
      new Promise<void>(function check(resolve) {
        if (finished || !ws || ws.bufferedAmount < MAX_BUFFERED) resolve();
        else setTimeout(() => check(resolve), 20);
      });
    for (const [i, f] of files.entries()) {
      const p = (current = { name: f.name, size: f.size, bytes: 0 });
      const xfer = await session.send_offer({ name: f.name, size: f.size });
      if (!xfer) {
        r.skipped.push(f.name);
        setCurrent(null);
        continue;
      }
      setCurrent(p);
      for (;;) {
        await drained();
        if (finished) return;
        const b = await api.zmodemReadFile(ptyId, i, p.bytes, READ_SIZE);
        if (!b.length) break;
        xfer.send(b);
        p.bytes += b.length;
        setCurrent(p);
      }
      await xfer.end();
      r.done.push(f.name);
      setCurrent(null);
    }
    await session.close();
  }

  (async () => {
    let files: api.ZmodemFile[] = [];
    try {
      if (direction === "download") r.folder = await api.zmodemChooseFolder(ptyId);
      else files = await api.zmodemChooseFiles(ptyId);
    } catch (e) {
      r.error = message(e);
    }
    if (direction === "download" ? !r.folder : !files.length) return cancel();
    if (cancelled) return;

    let session: Zmodem.Session | null = null;
    const sentry = new Zmodem.Sentry({
      // Go showed the output before the transfer; what comes in during it is
      // protocol noise. After it, what zmodem.js hands back is the shell's.
      to_terminal: (octets) => {
        if (ended && octets.length) term.write(new Uint8Array(octets));
      },
      sender: (octets) => frame(REMOTE, octets),
      on_detect: (d) => {
        if (session) return;
        if (d.get_session_role() !== (direction === "download" ? "receive" : "send")) return cancel();
        const s = (session = d.confirm());
        s.on("session_end", () => {
          if (s.aborted()) r.error ??= t("zmodem.serverCancelled");
          end();
        });
        if (direction === "download") {
          let chain = Promise.resolve();
          s.on("offer", (offer) => {
            chain = chain.then(() => receive(offer)).catch(fail);
          });
          s.start().catch(fail);
        } else send(s, files).catch(fail);
      },
      on_retract: () => {},
    });

    ws = api.zmodemStream();
    ws.binaryType = "arraybuffer";
    ws.onopen = () => ws?.send(ptyId);
    ws.onmessage = (ev: MessageEvent) => {
      const b = new Uint8Array(ev.data as ArrayBuffer);
      if (b[0] === REPLY) {
        replies.shift()?.(JSON.parse(new TextDecoder().decode(b.subarray(1))) as Reply);
      } else if (b[0] === REMOTE && ended) {
        term.write(b.subarray(1));
      } else if (b[0] === REMOTE) {
        try {
          sentry.consume(b.subarray(1));
        } catch (e) {
          if (!ended) fail(e); // a peer abort throws after it has ended the session
        }
      }
    };
    // Go closes the stream after END, after ZmodemCancel, after two idle
    // minutes, or when the terminal closes.
    ws.onclose = () => {
      if (!ended && !cancelled) r.error ??= t("zmodem.lost");
      finish();
    };
  })();

  return { cancel };
}
