import { describe, expect, it } from "vitest";
import type { Proxy } from "../api";
import { holdReasons, ignoreCancel, plainOnly } from "./SyncSettings";

const hold = { id: "h1", name: "web", kind: "host" };
const office: Proxy = { id: "p1", name: "office", type: "socks5", host: "proxy.lan", port: 1080 };

describe("sync hold reasons", () => {
  it("reads a hold that names nothing as plaintext, as older holds were", () => {
    expect(holdReasons(hold, [])).toEqual([expect.stringMatching(/unencrypted/)]);
  });

  it("names each weakened setting once", () => {
    const r = holdReasons({ ...hold, forwardAgent: true, algorithms: { kex: ["diffie-hellman-group1-sha1"], ciphers: ["3des-cbc"] } }, []);
    expect(r).toHaveLength(2);
    expect(r[0]).toMatch(/agent/);
    expect(r[1]).toMatch(/diffie-hellman-group1-sha1, 3des-cbc/);
  });

  it("names the new proxy, or says the proxy is removed", () => {
    expect(holdReasons({ ...hold, proxyId: "p1" }, [office])[0]).toMatch(/office \(socks5 proxy\.lan:1080\)/);
    expect(holdReasons({ ...hold, proxyId: "gone" }, [office])[0]).toMatch(/not on this computer/);
    expect(holdReasons({ ...hold, proxyId: "" }, [office])[0]).toMatch(/connect directly/);
  });

  // R47: X11 forwarding lets the host see and control this screen.
  it("names a synced X11 forwarding change, not as plaintext", () => {
    expect(holdReasons({ ...hold, x11: true }, [])).toEqual([expect.stringMatching(/^Turn on X11 forwarding/)]);
    expect(holdReasons({ ...hold, x11: true, forwardAgent: true }, [])).toHaveLength(2);
  });

  // KTD28: a synced server or CA change for a cluster waits, and says so.
  it("names a held cluster's new server, not as plaintext", () => {
    const cluster = { url: "https://evil.example:6443", hostname: "evil.example", port: 6443 };
    expect(holdReasons({ ...hold, cluster }, [])).toEqual([expect.stringMatching(/evil\.example:6443/)]);
  });

  // A known cluster whose server changed and proxy did not carries only
  // its cluster: that is not a plaintext file-host hold.
  it("does not word a cluster-only hold as plaintext alone", () => {
    const cluster = { url: "https://k8s.example:6443", hostname: "k8s.example", port: 6443 };
    expect(plainOnly({ ...hold, cluster })).toBe(false);
    expect(plainOnly({ ...hold, plaintext: true })).toBe(true);
    expect(plainOnly(hold)).toBe(true);
  });

  // A cluster new here carries proxyId "" (it never had a proxy), as does
  // a known one whose proxy was removed with its server; the line must be
  // true of both, so it never says a proxy is being stopped.
  it("words a held cluster's empty proxy as a direct connection, not a removal", () => {
    const cluster = { url: "https://k8s.example:6443", hostname: "k8s.example", port: 6443 };
    const r = holdReasons({ ...hold, cluster, proxyId: "" }, []);
    expect(r).toHaveLength(2);
    expect(r[0]).toMatch(/k8s\.example:6443/);
    expect(r[1]).not.toMatch(/Stop using a proxy/);
    expect(r[1]).toMatch(/without a proxy/);
  });
});

// Allowing a widening hold asks in Go's dialog and OS verification; saying
// no there is not an error, and the hold stays listed.
describe("allowing a hold", () => {
  it("is quiet when the dialog or verification is cancelled", () => {
    expect(() => ignoreCancel({ cause: { code: "VerifyFailed", message: "Cancelled; nothing changed." } })).not.toThrow();
  });
  it("still reports any other failure", () => {
    expect(() => ignoreCancel({ cause: { code: "Internal", message: "disk full" } })).toThrow();
  });
});
