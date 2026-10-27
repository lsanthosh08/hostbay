import { afterEach, expect, it } from "vitest";
import { channelSocket } from "./spice";

// spice-html5 reads window.setTimeout and `'MediaSource' in window`.
(globalThis as { window?: unknown }).window ??= globalThis;

/** A WebSocket-shaped stand-in for a WailsSocket ("spice" GoStream). */
class FakeStream extends EventTarget {
  binaryType = "arraybuffer";
  readyState = 0;
  url = "http://wails.localhost/wails/stream/poll#spice";
  sent: unknown[] = [];
  send(data: unknown) {
    if (this.readyState !== 1) throw new DOMException("Still in CONNECTING state.", "InvalidStateError");
    this.sent.push(data);
  }
  close() {
    this.readyState = 3;
  }
  open() {
    this.readyState = 1;
    this.dispatchEvent(new Event("open"));
  }
}

// The vendored client is plain JS served as its own files (not typed).
const vendor = (file: string) => import(/* @vite-ignore */ `../vendor/spice-html5/${file}`);

const stops: (() => void)[] = [];
afterEach(() => stops.splice(0).forEach((f) => f()));

// The main channel's link message as Go expects it byte for byte
// (internal/remotedesktop/spice.go SpiceMainLink).
const u32 = (...v: number[]) => v.flatMap((n) => [n & 255, (n >> 8) & 255, (n >> 16) & 255, n >>> 24]);
const MAIN_LINK = [..."REDQ"].map((c) => c.charCodeAt(0)).concat(u32(2, 2, 26, 0), [1, 0], u32(1, 1, 18, 1 | (1 << 3), 1 << 2));

it("sends the spiceId first on every channel, before the link message", async () => {
  const { SpiceMainConn } = await vendor("main.js");
  const { SpiceConn } = await vendor("spiceconn.js");
  const streams: FakeStream[] = [];
  const socket = channelSocket("spice-1", () => {
    const s = new FakeStream();
    streams.push(s);
    return s as unknown as WebSocket;
  });

  const main = new SpiceMainConn({ uri: "spice://hostbay", password: "", screen_id: "screen", socket });
  stops.push(() => main.stop());
  // A channel from the main channel's channels list, built the way main.js
  // builds it: {uri: this.ws.url, parent: this, ...}.
  const display = new SpiceConn({ uri: main.ws.url, parent: main, connection_id: 7, type: 2, chan_id: 0 });
  stops.push(() => display.cleanup());

  expect(streams).toHaveLength(2);
  for (const s of streams) s.open();
  for (const s of streams) {
    expect(s.sent[0]).toBe("spice-1");
    expect(new TextDecoder().decode(new Uint8Array(s.sent[1] as ArrayBuffer).slice(0, 4))).toBe("REDQ");
  }
  expect([...new Uint8Array(streams[0].sent[1] as ArrayBuffer)]).toEqual(MAIN_LINK);
  // The display channel links as itself: type 2, ID 0, the main channel's session.
  const link = new Uint8Array(streams[1].sent[1] as ArrayBuffer);
  expect([link[16], link[20], link[21]]).toEqual([7, 2, 0]);
});
