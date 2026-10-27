import { beforeEach, expect, it, vi } from "vitest";
import Zmodem from "zmodem.js";

// A fake of Go's side (internal/app/zmodem.go) with a zmodem.js peer as the
// server's sz or rz behind it, so both ends speak the real protocol.
const go = vi.hoisted(() => ({
  folder: "/dl",
  files: [] as { name: string; size: number }[],
  existing: new Set<string>(),
  saved: new Map<string, Uint8Array>(),
  saves: [] as { name: string; size: number; conflict?: string }[],
  reads: [] as number[],
  upload: new Uint8Array(),
  rest: "$ ", // the shell's prompt, which Go sends after END
  stream: null as null | { toPage(type: number, b: ArrayLike<number>): void; close(): void },
  server: null as null | { consume(b: number[]): void },
  cancel: vi.fn(),
}));

vi.mock("../api", () => ({
  errorInfo: (e: unknown) => ({ message: String(e) }),
  zmodemChooseFolder: async () => go.folder,
  zmodemChooseFiles: async () => go.files,
  zmodemReadFile: async (_id: string, _i: number, offset: number, length: number) => {
    go.reads.push(offset);
    return go.upload.slice(offset, offset + length);
  },
  zmodemCancel: async (id: string) => {
    go.cancel(id);
    go.stream?.close(); // Go aborts at the server, so the pump ends the stream
  },
  zmodemStream: () => {
    let file: { name: string; bytes: number[] } | null = null;
    let first = true;
    let gone = false;
    const ws = {
      binaryType: "",
      bufferedAmount: 0,
      onopen: null as null | (() => void),
      onmessage: null as null | ((ev: { data: ArrayBuffer }) => void),
      onclose: null as null | (() => void),
      closed: false,
      send(d: string | Uint8Array) {
        if (first) {
          first = false;
          return expect(d).toBe("p1");
        }
        const b = d as Uint8Array;
        const body = b.subarray(1);
        const json = () => JSON.parse(new TextDecoder().decode(body));
        const reply = (r: object) => go.stream!.toPage(1, new TextEncoder().encode(JSON.stringify(r)));
        if (b[0] === 0) go.server!.consume(Array.from(body));
        else if (b[0] === 1) {
          const req = json();
          go.saves.push(req);
          let name: string = req.name;
          if (go.existing.has(name) && !req.conflict) return reply({ name, exists: true });
          if (go.existing.has(name) && req.conflict === "rename") name = name.replace(/(\.\w+)$/, " (1)$1");
          file = { name, bytes: [] };
          reply({ name });
        } else if (b[0] === 2) file!.bytes.push(...body);
        else if (b[0] === 3) {
          go.saved.set(file!.name, new Uint8Array(file!.bytes));
          reply({ name: file!.name });
        } else if (b[0] === 4) {
          go.stream!.toPage(0, new TextEncoder().encode(go.rest));
          go.stream!.close();
        }
      },
      close() {
        if (ws.closed) return;
        ws.closed = true;
        setTimeout(() => {
          gone = true; // frames sent before close still arrive, as over the real stream
          ws.onclose?.();
        });
      },
    };
    go.stream = {
      toPage: (type, body) => {
        const b = new Uint8Array(body.length + 1);
        b[0] = type;
        b.set(body, 1);
        setTimeout(() => !gone && ws.onmessage?.({ data: b.buffer }));
      },
      close: ws.close,
    };
    setTimeout(() => {
      ws.onopen?.();
      startServer();
    });
    return ws;
  },
}));

import { startZmodem, type ZmodemConflict, type ZmodemProgress, type ZmodemResult } from "./zmodem";

// eslint-disable-next-line @typescript-eslint/no-explicit-any
const Z = Zmodem as any;
const enc = (s: string) => Array.from(s, (c) => c.charCodeAt(0)); // one byte per char
const toPage = (octets: number[]) => go.stream!.toPage(0, octets);
let startServer = () => {};

/** sz on the server sending these files, then "after" in OO's own read. */
function sz(files: Record<string, string>) {
  startServer = () => {
    const sentry = new Z.Sentry({
      to_terminal: () => {},
      on_retract: () => {},
      // The page's ZRINIT makes this a send session, as lsz's would.
      sender: (o: number[]) => toPage(String.fromCharCode(...o) === "OO" ? [...o, ...enc("after\r\n")] : o),
      on_detect: async (d: { confirm(): { send_offer: Function; close(): Promise<void> } }) => {
        const s = d.confirm();
        for (const [name, text] of Object.entries(files)) {
          const xfer = await s.send_offer({ name, size: text.length });
          if (xfer) await xfer.end(enc(text));
        }
        await s.close();
      },
    });
    go.server = sentry;
    toPage(enc("**\x18B00000000000000\r\x8a\x11")); // ZRQINIT
  };
}

/** rz on the server, keeping what it receives in `got`. */
function rz(got: Map<string, number[]>) {
  startServer = () => {
    const s = new Z.Session.Receive();
    s.set_sender(toPage);
    s.on("offer", (o: { get_details(): { name: string }; accept(x: object): void }) => {
      const bytes: number[] = [];
      got.set(o.get_details().name, bytes);
      o.accept({ on_input: (p: number[]) => bytes.push(...p) });
    });
    go.server = s;
    s.start();
  };
}

function run(direction: "download" | "upload", ask: (name: string) => Promise<ZmodemConflict> = async () => "skip") {
  const term: number[] = [];
  const progress: ZmodemProgress[] = [];
  let handle!: { cancel(): void };
  const result = new Promise<ZmodemResult>((finish) => {
    handle = startZmodem("p1", direction, { write: (d) => term.push(...d) }, {
      progress: (p) => p && progress.push(p),
      ask,
      finish,
    });
  });
  return { result, handle, progress, text: () => new TextDecoder().decode(new Uint8Array(term)) };
}

beforeEach(() => {
  go.folder = "/dl";
  go.files = [];
  go.existing = new Set();
  go.saved = new Map();
  go.saves = [];
  go.reads = [];
  go.stream = null;
  go.cancel.mockClear();
});

it("accepts a download offer, forwards its bytes to Go and completes", async () => {
  sz({ "a.txt": "hello zmodem", "b.txt": "second file" });
  const { result, progress } = run("download");
  const r = await result;
  expect(r).toMatchObject({ outcome: "done", folder: "/dl", done: ["a.txt", "b.txt"], skipped: [], failed: [] });
  expect(new TextDecoder().decode(go.saved.get("a.txt"))).toBe("hello zmodem");
  expect(new TextDecoder().decode(go.saved.get("b.txt"))).toBe("second file");
  expect(progress[progress.length - 1]).toEqual({ name: "b.txt", size: 11, bytes: 11 });
});

it("returns text to xterm once the session ends, and none of the protocol", async () => {
  sz({ "a.txt": "x" });
  const { result, text } = run("download");
  await result;
  // "after" came in the same read as sz's OO; "$ " followed END.
  expect(text()).toBe("after\r\n$ ");
});

it.each([
  ["rename", "a (1).txt"],
  ["overwrite", "a.txt"],
] as const)("asks about a name taken in the folder and saves with %s", async (choice, saved) => {
  sz({ "a.txt": "new" });
  go.existing.add("a.txt");
  const ask = vi.fn(async () => choice);
  const r = await run("download", ask).result;
  expect(ask).toHaveBeenCalledWith("a.txt");
  expect(go.saves).toEqual([
    { name: "a.txt", size: 3 },
    { name: "a.txt", size: 3, conflict: choice },
  ]);
  expect(r.done).toEqual([saved]);
  expect(new TextDecoder().decode(go.saved.get(saved))).toBe("new");
});

it("skips a taken name when the user says so", async () => {
  sz({ "a.txt": "new", "b.txt": "b" });
  go.existing.add("a.txt");
  const r = await run("download", async () => "skip").result;
  expect(r).toMatchObject({ outcome: "done", done: ["b.txt"], skipped: ["a.txt"] });
  expect(go.saved.has("a.txt")).toBe(false);
});

it("cancels through ZmodemCancel and reports the file it interrupted", async () => {
  sz({ "a.txt": "new" });
  go.existing.add("a.txt");
  let cancel = () => {};
  const t = run("download", () => {
    cancel();
    return new Promise(() => {}); // the question stays open
  });
  cancel = t.handle.cancel;
  const r = await t.result;
  expect(go.cancel).toHaveBeenCalledWith("p1");
  expect(r.outcome).toBe("cancelled");
  expect(r.failed.map((f) => f.name)).toEqual(["a.txt"]);
});

it("cancels at the server when the folder dialog is cancelled", async () => {
  go.folder = "";
  const r = await run("download").result;
  expect(go.cancel).toHaveBeenCalledWith("p1");
  expect(go.stream).toBeNull();
  expect(r.outcome).toBe("cancelled");
});

it("sends an upload, reading the file from Go in chunks", async () => {
  const size = (1 << 20) + 100;
  go.upload = Uint8Array.from({ length: size }, (_, i) => i % 251);
  go.files = [{ name: "u.bin", size }];
  const got = new Map<string, number[]>();
  rz(got);
  const r = await run("upload").result;
  expect(r).toMatchObject({ outcome: "done", folder: "", done: ["u.bin"], failed: [] });
  expect(go.reads).toEqual([0, 1 << 20, size]);
  expect(new Uint8Array(got.get("u.bin")!)).toEqual(go.upload);
});
