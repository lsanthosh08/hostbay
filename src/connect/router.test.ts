import { describe, expect, it } from "vitest";
import type { AppError, Host } from "../api";
import { route, trustService } from "./router";

const host = (over: Partial<Host> = {}): Host => ({
  id: "web",
  alias: "web",
  hostname: "web.example",
  port: 22,
  user: "root",
  identityFile: "",
  tags: [],
  ...over,
});
const err = (code: string, details?: Record<string, string>): AppError => ({ code, message: `${code} message`, details });

describe("connect router", () => {
  // AE16: a failure stays in the tab with a way forward.
  it("offers Wake-on-LAN for an unreachable host with a known MAC, else a plain retry", () => {
    expect(route(err("Unreachable"), { host: host({ mac: "aa:bb:cc:dd:ee:ff" }) }).kind).toBe("wake");
    expect(route(err("Unreachable"), { host: host() })).toEqual({ kind: "failure", message: "Unreachable message" });
  });

  it("asks to trust an unknown key with its request ID, hop and fingerprint", () => {
    const details = { requestId: "r1", hop: "1", host: "jump", port: "22", keyType: "ssh-ed25519", fingerprint: "SHA256:x" };
    const step = route(err("HostKeyUnknown", details), { host: host() });
    expect(step).toMatchObject({ kind: "trust", prompt: { requestId: "r1", hop: "1", fingerprint: "SHA256:x" } });
  });

  // AE2: a changed key is a refusal with both fingerprints and no accept.
  it("refuses a changed key with both fingerprints and no way to accept it", () => {
    const details = {
      host: "web.example",
      port: "22",
      hop: "0",
      keyType: "ssh-ed25519",
      fingerprint: "SHA256:new",
      knownFingerprints: "SHA256:old",
      removeId: "rm1",
    };
    const step = route(err("HostKeyChanged", details), { host: host() });
    expect(step).toMatchObject({ kind: "keyChanged", change: { fingerprint: "SHA256:new", knownFingerprints: "SHA256:old", removeId: "rm1" } });
    expect(JSON.stringify(step)).not.toMatch(/requestId|accept/);
  });

  // R29: never an empty password; a locked vault unlocks first.
  it("unlocks and retries a locked vault, and asks for a missing password", () => {
    expect(route(err("SecretNeeded", { reason: "locked" }), { host: host() }).kind).toBe("unlock");
    const jump = host({ id: "jump", alias: "jump", authType: "password" });
    expect(route(err("SecretNeeded", { hostId: "jump" }), { host: host(), hosts: [host(), jump] })).toMatchObject({
      kind: "secret",
      host: { id: "jump" },
    });
  });

  it("names verification failures and a missing RDP helper", () => {
    expect(route(err("VerifyFailed"), {}).kind).toBe("verify");
    expect(route(err("HelperMissing"), {}).kind).toBe("helperMissing");
  });

  it("turns an unknown code into a plain failure with its message", () => {
    expect(route(err("Bogus"), { host: host() })).toEqual({ kind: "failure", message: "Bogus message" });
    expect(route({ message: "boom" }, {})).toEqual({ kind: "failure", message: "boom" });
  });

  it("shows a drop after the session went live as the reconnect banner", () => {
    expect(route(err("Unreachable"), { host: host({ mac: "aa:bb:cc:dd:ee:ff" }), wasLive: true }).kind).toBe("reconnect");
  });

  // KTD18: a file host's certificate is trusted like an RDP one, and a
  // changed one is refused like a changed SSH key (AE2).
  it("asks to trust a file host's unknown certificate through the transfer service", () => {
    for (const kind of ["ftpCert", "webdavCert", "s3Cert"]) {
      const details = { kind, requestId: "c1", host: "dav.lan", port: "443", keyType: "WebDAV certificate", fingerprint: "SHA256:c" };
      const step = route(err("HostKeyUnknown", details), { host: host({ kind: "webdav" }) });
      expect(step).toMatchObject({ kind: "trust", prompt: { kind, requestId: "c1", fingerprint: "SHA256:c" } });
      if (step.kind === "trust") expect(trustService(step.prompt)).toBe("transfer");
    }
    expect(trustService({ kind: "rdpCert" } as never)).toBe("rdp");
    expect(trustService({ hop: "1" } as never)).toBe("ssh");
  });

  it("refuses a file host's changed certificate with both fingerprints and nothing to remove", () => {
    const details = { kind: "webdavCert", host: "dav.lan", port: "443", expected: "SHA256:old", fingerprint: "SHA256:new" };
    const step = route(err("HostKeyChanged", details), { host: host({ kind: "webdav" }) });
    expect(step).toMatchObject({ kind: "keyChanged", change: { knownFingerprints: "SHA256:old", fingerprint: "SHA256:new", removeId: "" } });
    expect(JSON.stringify(step)).not.toMatch(/requestId|accept/);
  });

  it("asks for a file host's password when it has a user, as R29 does", () => {
    const dav = host({ kind: "webdav", user: "me", authType: null });
    expect(route(err("SecretNeeded", { hostId: "web" }), { host: dav, hosts: [dav] })).toMatchObject({ kind: "secret", host: { id: "web" } });
  });

  // KTD18, KTD19: a VNC server's VeNCrypt certificate goes to the VNC
  // service, a changed one is refused, and a refused password asks again.
  it("asks to trust a VNC certificate through the VNC service", () => {
    const details = { kind: "vncCert", requestId: "v1", host: "mac.lan", port: "5900", keyType: "VNC certificate", fingerprint: "SHA256:v" };
    const step = route(err("HostKeyUnknown", details), { host: host({ kind: "vnc" }) });
    expect(step).toMatchObject({ kind: "trust", prompt: { kind: "vncCert", requestId: "v1", fingerprint: "SHA256:v" } });
    if (step.kind === "trust") expect(trustService(step.prompt)).toBe("vnc");
  });

  it("refuses a changed VNC certificate with both fingerprints and no accept", () => {
    const details = { kind: "vncCert", host: "mac.lan", port: "5900", expected: "SHA256:old", fingerprint: "SHA256:new" };
    const step = route(err("HostKeyChanged", details), { host: host({ kind: "vnc" }) });
    expect(step).toMatchObject({ kind: "keyChanged", change: { knownFingerprints: "SHA256:old", fingerprint: "SHA256:new", removeId: "" } });
    expect(JSON.stringify(step)).not.toMatch(/requestId|accept/);
  });

  it("asks for a VNC host's password, with or without a user", () => {
    const vnc = host({ kind: "vnc", user: "", authType: null });
    for (const reason of ["wrong", "missing"])
      expect(route(err("SecretNeeded", { reason, hostId: "web" }), { host: vnc, hosts: [vnc] })).toMatchObject({ kind: "secret", host: { id: "web" } });
  });

  // KTD22: a proxy problem never offers a direct connection.
  it("sends a held proxy to Sync settings and a missing one to Edit host", () => {
    expect(route(err("ProxyFailed", { step: "proxy", reason: "held" }), { host: host() })).toEqual({
      kind: "proxyHeld",
      message: "ProxyFailed message",
    });
    expect(route(err("ProxyFailed", { step: "proxy", reason: "missing" }), { host: host() })).toEqual({
      kind: "proxyMissing",
      message: "ProxyFailed message",
    });
  });

  it("shows any other proxy failure as a plain failure at the proxy step", () => {
    const step = route(err("ProxyFailed", { step: "proxy", host: "proxy.lan", port: "1080" }), { host: host({ mac: "aa:bb:cc:dd:ee:ff" }) });
    expect(step).toEqual({ kind: "failure", message: "ProxyFailed message", proxy: true });
  });

  // R16, R17, AE2: a VPN problem never offers a direct connection or Wake-on-LAN.
  it("sends a held VPN to Sync settings and a missing or ambiguous one to Edit host", () => {
    const mac = host({ mac: "aa:bb:cc:dd:ee:ff" });
    expect(route(err("VPNFailed", { vpnId: "v1", reason: "held" }), { host: mac })).toEqual({ kind: "vpnHeld", message: "VPNFailed message" });
    expect(route(err("VPNFailed", { vpnId: "v1", reason: "missing" }), { host: mac })).toEqual({ kind: "vpnChoose", message: "VPNFailed message" });
    expect(route(err("VPNAmbiguous", { vpnIds: "a,b" }), { host: mac })).toEqual({ kind: "vpnChoose", message: "VPNAmbiguous message" });
    for (const code of ["VPNFailed", "VPNDown"])
      expect(route(err(code, { vpnId: "v1", reason: "failed" }), { host: mac })).toEqual({ kind: "failure", message: `${code} message` });
  });

  // R21, AE8: an unknown gateway certificate can be trusted; a changed one is refused.
  it("asks to trust an unknown VPN gateway certificate and refuses a changed one", () => {
    const base = { vpnId: "v1", serverCert: "sha256:new" };
    expect(route(err("VPNFailed", { ...base, reason: "cert-unknown" }), { host: host() })).toEqual({
      kind: "vpnCert",
      message: "VPNFailed message",
      vpnId: "v1",
      cert: { pin: "sha256:new" },
    });
    expect(route(err("VPNFailed", { ...base, reason: "cert-changed", pinned: "sha256:old" }), { host: host() })).toEqual({
      kind: "vpnCertChanged",
      message: "VPNFailed message",
      vpnId: "v1",
      cert: { pin: "sha256:new", pinned: "sha256:old" },
    });
    // No certificate or profile to act on: a plain failure.
    expect(route(err("VPNFailed", { vpnId: "v1", reason: "cert-unknown" }), { host: host() }).kind).toBe("failure");
  });

  it("asks to run kinit for a missing Kerberos ticket, not for a password", () => {
    const krb = host({ authType: "kerberos" });
    expect(route(err("SecretNeeded", { kind: "kerberos" }), { host: krb, hosts: [krb] })).toEqual({
      kind: "kerberos",
      message: "SecretNeeded message",
    });
  });

  it("asks for a proxy's password when the saved one is missing or no longer opens", () => {
    const pw = host({ authType: "password" });
    expect(route(err("SecretNeeded", { proxyId: "p1", hostId: "web" }), { host: pw, hosts: [pw] })).toEqual({
      kind: "proxySecret",
      message: "SecretNeeded message",
      proxyId: "p1",
    });
    // A locked vault still unlocks first.
    expect(route(err("SecretNeeded", { proxyId: "p1", reason: "locked" }), { host: pw }).kind).toBe("unlock");
  });

  // R46, KTD18: a SPICE TLS port's certificate goes to the SPICE service, a
  // changed one is refused, and a refused password asks for one.
  it("asks to trust a SPICE certificate through the SPICE service", () => {
    const details = { kind: "spiceCert", requestId: "s1", host: "kvm.lan", port: "5900", keyType: "SPICE certificate", fingerprint: "SHA256:s" };
    const step = route(err("HostKeyUnknown", details), { host: host({ kind: "spice" }) });
    expect(step).toMatchObject({ kind: "trust", prompt: { kind: "spiceCert", requestId: "s1", fingerprint: "SHA256:s" } });
    if (step.kind === "trust") expect(trustService(step.prompt)).toBe("spice");
  });

  it("refuses a changed SPICE certificate with both fingerprints and no accept", () => {
    const details = { kind: "spiceCert", host: "kvm.lan", port: "5900", expected: "SHA256:old", fingerprint: "SHA256:new" };
    const step = route(err("HostKeyChanged", details), { host: host({ kind: "spice" }) });
    expect(step).toMatchObject({ kind: "keyChanged", change: { knownFingerprints: "SHA256:old", fingerprint: "SHA256:new", removeId: "" } });
    expect(JSON.stringify(step)).not.toMatch(/requestId|accept/);
  });

  it("asks for a SPICE host's password when the server refused it", () => {
    const spice = host({ kind: "spice", user: "", authType: null });
    expect(route(err("SecretNeeded", { reason: "wrong", hostId: "web" }), { host: spice, hosts: [spice] })).toMatchObject({
      kind: "secret",
      host: { id: "web" },
    });
  });

  // R56, KTD25: a database server's certificate goes to the database
  // service, a changed one keeps the removal ID it came with, and a
  // database host always signs in with its password.
  it("routes a database host's certificate and password steps", () => {
    const details = { kind: "postgresCert", requestId: "p1", host: "db.lan", port: "5432", keyType: "PostgreSQL certificate", fingerprint: "SHA256:p" };
    const step = route(err("HostKeyUnknown", details), { host: host({ kind: "postgres" }) });
    if (step.kind !== "trust") throw new Error(step.kind);
    expect(trustService(step.prompt)).toBe("database");
    const changed = route(err("HostKeyChanged", { kind: "postgresCert", host: "db.lan", expected: "SHA256:old", fingerprint: "SHA256:new", removeId: "rm1" }), {});
    expect(changed).toMatchObject({ kind: "keyChanged", change: { removeId: "rm1", knownFingerprints: "SHA256:old" } });
    const pg = host({ kind: "postgres", authType: null });
    expect(route(err("SecretNeeded", { reason: "missing", hostId: "web" }), { host: pg, hosts: [pg] })).toMatchObject({ kind: "secret" });
    for (const kind of ["mysql", "mssql", "oracle", "rqlite"] as const) {
      expect(trustService({ kind: `${kind}Cert` } as never)).toBe("database");
      const db = host({ kind, authType: null });
      expect(route(err("SecretNeeded", { reason: "wrong", hostId: "web" }), { host: db, hosts: [db] })).toMatchObject({ kind: "secret" });
    }
  });

  // R56: Redis, MongoDB and Elasticsearch certificates go to the database
  // service, and a server that refused to sign in without a password asks
  // for one, user name or not.
  it("routes Redis, MongoDB and Elasticsearch certificate and password steps", () => {
    for (const kind of ["redis", "mongodb", "elasticsearch"] as const) {
      const step = route(err("HostKeyUnknown", { kind: `${kind}Cert`, requestId: "r1", host: "db.lan", port: "1", fingerprint: "SHA256:r" }), {});
      if (step.kind !== "trust") throw new Error(step.kind);
      expect(trustService(step.prompt)).toBe("database");
      const h = host({ kind, authType: null, user: "" });
      expect(route(err("SecretNeeded", { reason: "wrong", hostId: "web" }), { host: h, hosts: [h] })).toMatchObject({ kind: "secret" });
    }
  });

  // R47: an X11 desktop that cannot start says why, in Go's words.
  it("shows an X11 desktop's start failure plainly, with a retry", () => {
    for (const message of [
      "this host has no Xvnc; install TigerVNC's Xvnc (package tigervnc-standalone-server or tigervnc-server)",
      "this host's Xvnc cannot serve VNC on a Unix socket (no -rfbunixpath); install TigerVNC 1.10 or later",
      "found no desktop to run on this host (startxfce4, mate-session or gnome-session); install one or set the host's X11 desktop command",
      "Xvnc did not start on this host",
    ])
      expect(route({ message }, { host: host() })).toEqual({ kind: "failure", message });
  });
});

describe("Kubernetes connect steps", () => {
  // R57, KTD28: a cluster's certificate goes to the Kubernetes service, and
  // a refused or unopenable token asks for one.
  it("routes a cluster's certificate and token steps", () => {
    const details = { kind: "kubernetesCert", requestId: "k1", host: "k8s.lan", port: "6443", keyType: "Kubernetes certificate", fingerprint: "SHA256:k" };
    const step = route(err("HostKeyUnknown", details), { host: host({ kind: "kubernetes" }) });
    if (step.kind !== "trust") throw new Error(step.kind);
    expect(trustService(step.prompt)).toBe("kubernetes");
    const k = host({ kind: "kubernetes", authType: null });
    expect(route(err("SecretNeeded", { reason: "wrong", hostId: "web" }), { host: k, hosts: [k] })).toMatchObject({ kind: "secret" });
  });
});
