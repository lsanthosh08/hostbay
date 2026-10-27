// The vendored spice-html5 0.3.0 (src/vendor/spice-html5, LGPL-3.0) and the
// one hook Hostbay adds to it: a socket option (PATCHES.md there).

/** The part of SpiceMainConn that SpiceTab uses. */
export type SpiceMain = { stop(): void };
export type SpiceOptions = {
  uri: string;
  password: string;
  screen_id: string;
  socket: () => WebSocket;
  onsuccess: () => void;
  onerror: (e: Error) => void;
};
type SpiceModule = {
  SpiceMainConn: new (o: SpiceOptions) => SpiceMain;
  sendCtrlAltDel: (sc: SpiceMain) => void;
};

// Served as its own unminified files, never bundled into the app's chunks,
// so it stays a separate, replaceable library (vite.config.ts copies it).
const SPICE_MAIN = `${import.meta.env.DEV ? "/src/" : import.meta.env.BASE_URL}vendor/spice-html5/main.js`;
export const loadSpice = (): Promise<SpiceModule> => import(/* @vite-ignore */ SPICE_MAIN);

/**
 * The socket for each SPICE channel: its own "spice" GoStream, whose first
 * frame must be the spiceId. This listener is added before SpiceConn adds
 * its own, so the spiceId goes out before spice-html5's link message
 * (send_hdr). Channels the main channel opens inherit it through `parent`.
 */
export const channelSocket = (spiceId: string, open: () => WebSocket) => () => {
  const ws = open();
  ws.addEventListener("open", () => ws.send(spiceId), { once: true });
  return ws;
};
