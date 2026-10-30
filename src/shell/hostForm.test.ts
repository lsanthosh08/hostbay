import { describe, expect, it } from "vitest";
import type { Host } from "../api";
import { asksPasswordAgain, formHost, missingVpn, passwordForKind, proxyAsksPasswordAgain, unencrypted, validateHost, validateProxy } from "./hostForm";

const host = (over: Partial<Host> = {}): Host => ({
  id: "web",
  alias: "web",
  hostname: "web.example",
  port: 22,
  user: "root",
  identityFile: "~/.ssh/id_ed25519",
  tags: [],
  authType: "key",
  kind: "ssh",
  ...over,
});

describe("host form", () => {
  it("accepts a complete SSH host", () => {
    expect(validateHost(host(), [])).toEqual({});
  });

  it("rejects ports outside 1–65535", () => {
    expect(validateHost(host({ port: 0 }), []).port).toBeDefined();
    expect(validateHost(host({ port: 70000 }), []).port).toBeDefined();
    expect(validateHost(host({ port: Number.NaN }), []).port).toBeDefined();
    expect(validateHost(host({ kind: "rdp", authType: "password", hasPassword: true, rdpPort: 70000 }), []).rdpPort).toBeDefined();
    expect(validateHost(host({ port: 65535 }), [])).toEqual({});
  });

  it("requires a host name, alias and user, except a Telnet user", () => {
    const errors = validateHost(host({ hostname: "  ", alias: "", user: "" }), []);
    expect(Object.keys(errors).sort()).toEqual(["alias", "hostname", "user"]);
    expect(validateHost(host({ kind: "telnet", user: "", identityFile: "" }), [])).toEqual({});
  });

  it("refuses a jump host that leads back to the host itself", () => {
    expect(validateHost(host({ jumpHostId: "web" }), []).jumpHostId).toBeDefined();
    const bastion = host({ id: "bastion", jumpHostId: "web" });
    expect(validateHost(host({ jumpHostId: "bastion" }), [bastion]).jumpHostId).toBeDefined();
    expect(validateHost(host({ jumpHostId: "bastion" }), [host({ id: "bastion" })])).toEqual({});
  });

  it("asks for the credential the chosen authentication needs", () => {
    expect(validateHost(host({ identityFile: "" }), []).identityFile).toBeDefined();
    expect(validateHost(host({ authType: "password" }), []).password).toBeDefined();
    expect(validateHost(host({ authType: "password", hasPassword: true }), [])).toEqual({});
  });
});

describe("connection extras", () => {
  it("signs in with the agent or Kerberos without a key file or password", () => {
    expect(validateHost(host({ authType: "agent", identityFile: "" }), [])).toEqual({});
    expect(validateHost(host({ authType: "kerberos", identityFile: "" }), [])).toEqual({});
  });

  it("refuses a proxy together with a jump host, and a proxy for Mosh", () => {
    const bastion = host({ id: "bastion" });
    expect(validateHost(host({ proxyId: "p1", jumpHostId: "bastion" }), [bastion]).proxyId).toBe("hostForm.error.proxyJump");
    expect(validateHost(host({ kind: "mosh", proxyId: "p1" }), []).proxyId).toBe("hostForm.error.proxyMosh");
    expect(validateHost(host({ proxyId: "p1" }), [])).toEqual({});
    expect(validateHost(host({ kind: "ftp", identityFile: "", user: "", authType: null, port: 0, proxyId: "p1" }), [])).toEqual({});
  });

  it("takes a keepalive from 0 to 86400 seconds, or none", () => {
    expect(validateHost(host({ keepalive: 0 }), [])).toEqual({});
    expect(validateHost(host({ keepalive: 86400 }), [])).toEqual({});
    expect(validateHost(host({ keepalive: null }), [])).toEqual({});
    expect(validateHost(host({ keepalive: 86401 }), []).keepalive).toBeDefined();
    expect(validateHost(host({ keepalive: -1 }), []).keepalive).toBeDefined();
    expect(validateHost(host({ keepalive: 1.5 }), []).keepalive).toBeDefined();
  });

  it("needs a raw TCP host's name and port, but no user or credentials", () => {
    const tcp = host({ kind: "tcp", port: 7000, user: "", identityFile: "", authType: null });
    expect(validateHost(tcp, [])).toEqual({});
    expect(validateHost({ ...tcp, port: Number.NaN }, []).port).toBeDefined();
    expect(validateHost({ ...tcp, hostname: "" }, []).hostname).toBeDefined();
  });

  it("names a raw TCP host unencrypted", () => {
    expect(unencrypted(host({ kind: "tcp" }))).toBe(true);
  });
});

describe("proxy form", () => {
  const proxy = { id: "", name: "office", type: "socks5" as const, host: "proxy.lan", port: 1080, user: "", password: "" };

  it("accepts a complete proxy, with the user and password optional", () => {
    expect(validateProxy(proxy)).toEqual({});
    expect(validateProxy({ ...proxy, type: "http", user: "me", password: "pw" })).toEqual({});
  });

  it("needs a name, a known type, a host name and a port from 1 to 65535", () => {
    const e = validateProxy({ ...proxy, name: " ", type: "socks4" as never, host: "", port: 0 });
    expect(Object.keys(e).sort()).toEqual(["host", "name", "port", "type"]);
    expect(validateProxy({ ...proxy, port: 65536 }).port).toBeDefined();
    expect(validateProxy({ ...proxy, port: Number.NaN }).port).toBeDefined();
  });

  it("refuses a host name with spaces or control characters, or over 253 characters", () => {
    expect(validateProxy({ ...proxy, host: "proxy lan" }).host).toBeDefined();
    expect(validateProxy({ ...proxy, host: "proxy\u0007" }).host).toBeDefined();
    expect(validateProxy({ ...proxy, host: "a".repeat(254) }).host).toBeDefined();
    expect(validateProxy({ ...proxy, host: "a".repeat(253) })).toEqual({});
  });

  it("limits the user and password to 255 bytes, and an HTTP user to no colon", () => {
    expect(validateProxy({ ...proxy, user: "é".repeat(128) }).user).toBeDefined(); // 256 bytes
    expect(validateProxy({ ...proxy, password: "x".repeat(256) }).password).toBeDefined();
    expect(validateProxy({ ...proxy, user: "a\nb" }).user).toBeDefined();
    expect(validateProxy({ ...proxy, type: "http", user: "a:b" }).user).toBe("proxyForm.error.userColon");
    expect(validateProxy({ ...proxy, user: "a:b" })).toEqual({}); // SOCKS5 sends the user as is
  });

  it("says when an edit means the saved password must be typed again", () => {
    const saved = { ...proxy, id: "p1", hasPassword: true };
    expect(proxyAsksPasswordAgain(saved, { ...saved, host: "other.lan" })).toBe(true);
    expect(proxyAsksPasswordAgain(saved, { ...saved, port: 3128 })).toBe(true);
    expect(proxyAsksPasswordAgain(saved, { ...saved, type: "http" })).toBe(true);
    expect(proxyAsksPasswordAgain(saved, { ...saved, user: "me" })).toBe(true);
    expect(proxyAsksPasswordAgain(saved, { ...saved, name: "renamed" })).toBe(false);
    expect(proxyAsksPasswordAgain(saved, { ...saved, host: "other.lan", password: "new" })).toBe(false);
    expect(proxyAsksPasswordAgain({ ...saved, hasPassword: false }, { ...saved, host: "other.lan" })).toBe(false);
  });
});

describe("VNC host form", () => {
  const vnc = (over: Partial<Host> = {}): Host => host({ kind: "vnc", identityFile: "", user: "", authType: null, port: 0, hasPassword: true, ...over });

  it("needs a host name and password, with the port and user optional", () => {
    expect(validateHost(vnc(), [])).toEqual({});
    expect(validateHost(vnc({ port: Number.NaN }), [])).toEqual({});
    expect(validateHost(vnc({ port: 5901, user: "admin", tls: "none" }), [])).toEqual({});
    expect(validateHost(vnc({ hostname: " " }), []).hostname).toBeDefined();
    expect(validateHost(vnc({ port: 70000 }), []).port).toBeDefined();
    expect(validateHost(vnc({ hasPassword: false }), []).password).toBeDefined();
    expect(validateHost(vnc({ hasPassword: false, password: "pw" }), [])).toEqual({});
    expect(validateHost(vnc({ tls: "implicit" }), []).tls).toBeDefined();
  });

  it("refuses a proxy together with a jump host", () => {
    const bastion = host({ id: "bastion" });
    expect(validateHost(vnc({ proxyId: "p1", jumpHostId: "bastion" }), [bastion]).proxyId).toBe("hostForm.error.proxyJump");
    expect(validateHost(vnc({ proxyId: "p1" }), [])).toEqual({});
  });

  it("names plain VNC unencrypted unless a jump host carries it", () => {
    expect(unencrypted(vnc({ tls: "none" }))).toBe(true);
    expect(unencrypted(vnc({ tls: "none", proxyId: "p1" }))).toBe(true);
    expect(unencrypted(vnc({ tls: "none", jumpHostId: "bastion" }))).toBe(false);
    expect(unencrypted(vnc())).toBe(false); // VeNCrypt is the default
    expect(unencrypted(vnc({ tls: "vencrypt" }))).toBe(false);
  });

  it("asks for the password again after the security mode changes", () => {
    expect(asksPasswordAgain(vnc(), vnc({ tls: "none" }))).toBe(true);
    expect(asksPasswordAgain(vnc(), vnc({ tls: "vencrypt" }))).toBe(false); // VeNCrypt is the default
    expect(asksPasswordAgain(vnc(), vnc({ tls: "none", password: "pw" }))).toBe(false);
    expect(asksPasswordAgain(vnc(), vnc({ alias: "renamed" }))).toBe(false);
  });
});

describe("SPICE host form", () => {
  const spice = (over: Partial<Host> = {}): Host => host({ kind: "spice", identityFile: "", user: "", authType: null, port: 0, ...over });

  it("needs a host name, with the port and password optional", () => {
    expect(validateHost(spice(), [])).toEqual({});
    expect(validateHost(spice({ port: Number.NaN }), [])).toEqual({});
    expect(validateHost(spice({ port: 5930, tls: "none", hasPassword: true }), [])).toEqual({});
    expect(validateHost(spice({ tls: "tls" }), [])).toEqual({});
    expect(validateHost(spice({ hostname: " " }), []).hostname).toBeDefined();
    expect(validateHost(spice({ port: 70000 }), []).port).toBeDefined();
    expect(validateHost(spice({ tls: "vencrypt" }), []).tls).toBe("hostForm.error.spiceSecurity");
  });

  it("refuses a proxy together with a jump host, and a jump host loop", () => {
    const bastion = host({ id: "bastion", jumpHostId: "web" });
    expect(validateHost(spice({ proxyId: "p1", jumpHostId: "bastion" }), [bastion]).proxyId).toBe("hostForm.error.proxyJump");
    expect(validateHost(spice({ jumpHostId: "bastion" }), [bastion]).jumpHostId).toBeDefined();
    expect(validateHost(spice({ proxyId: "p1" }), [])).toEqual({});
  });

  it("saves no user and no auth type", () => {
    expect(formHost(spice({ user: "left over", authType: "key", port: Number.NaN }))).toMatchObject({ user: "", authType: null, port: 0 });
  });

  it("names a plain port unencrypted unless a jump host carries it", () => {
    expect(unencrypted(spice({ tls: "none" }))).toBe(true);
    expect(unencrypted(spice({ tls: "none", proxyId: "p1" }))).toBe(true);
    expect(unencrypted(spice({ tls: "none", jumpHostId: "bastion" }))).toBe(false);
    expect(unencrypted(spice())).toBe(false); // TLS is the default
    expect(unencrypted(spice({ tls: "tls" }))).toBe(false);
  });

  it("asks for the password again after the security mode changes", () => {
    expect(asksPasswordAgain(spice({ hasPassword: true }), spice({ tls: "none" }))).toBe(true);
    expect(asksPasswordAgain(spice({ hasPassword: true }), spice({ tls: "tls" }))).toBe(false); // TLS is the default
    expect(asksPasswordAgain(spice({ hasPassword: true }), spice({ tls: "none", password: "pw" }))).toBe(false);
    expect(asksPasswordAgain(spice(), spice({ tls: "none" }))).toBe(false); // nothing saved to lose
  });
});

describe("file host form", () => {
  const file = (over: Partial<Host>): Host => host({ identityFile: "", user: "", authType: null, port: 0, ...over });

  it("needs an FTP or SMB host name, with the port and user optional", () => {
    expect(validateHost(file({ kind: "ftp" }), [])).toEqual({});
    expect(validateHost(file({ kind: "ftp", port: Number.NaN }), [])).toEqual({});
    expect(validateHost(file({ kind: "smb", domain: "CORP" }), [])).toEqual({});
    expect(validateHost(file({ kind: "ftp", hostname: " " }), []).hostname).toBeDefined();
    expect(validateHost(file({ kind: "smb", port: 70000 }), []).port).toBeDefined();
    expect(validateHost(file({ kind: "ftp", tls: "starttls" as never }), []).tls).toBeDefined();
  });

  it("needs a WebDAV or S3 URL with http(s) and a host, not a host name", () => {
    const dav = file({ kind: "webdav", hostname: "", url: "https://dav.example/files/" });
    expect(validateHost(dav, [])).toEqual({});
    expect(validateHost({ ...dav, url: "http://10.0.0.5:8080" }, [])).toEqual({});
    expect(validateHost({ ...dav, url: "" }, []).url).toBeDefined();
    expect(validateHost({ ...dav, url: "ftp://dav.example" }, []).url).toBeDefined();
    expect(validateHost({ ...dav, url: "https://" }, []).url).toBeDefined();
  });

  it("needs an S3 access key ID and bucket", () => {
    const s3 = file({ kind: "s3", hostname: "", url: "https://s3.example", user: "AKIA", bucket: "backups" });
    expect(validateHost(s3, [])).toEqual({});
    expect(Object.keys(validateHost({ ...s3, user: "", bucket: "" }, [])).sort()).toEqual(["bucket", "user"]);
  });

  it("names unencrypted file hosts", () => {
    expect(unencrypted(file({ kind: "ftp", tls: "none" }))).toBe(true);
    expect(unencrypted(file({ kind: "ftp" }))).toBe(false);
    expect(unencrypted(file({ kind: "webdav", url: "http://dav.example" }))).toBe(true);
    expect(unencrypted(file({ kind: "s3", url: "HTTPS://s3.example" }))).toBe(false);
    expect(unencrypted(file({ kind: "smb" }))).toBe(false);
  });

  it("says when an edit makes the saved password ask again", () => {
    const ftp = file({ kind: "ftp", hasPassword: true });
    expect(asksPasswordAgain(ftp, { ...ftp, tls: "implicit" })).toBe(true);
    expect(asksPasswordAgain(ftp, { ...ftp, tls: "explicit" })).toBe(false); // explicit is the default
    expect(asksPasswordAgain(ftp, { ...ftp, alias: "renamed" })).toBe(false);
    const dav = file({ kind: "webdav", url: "https://a.example", hasPassword: true });
    expect(asksPasswordAgain(dav, { ...dav, url: "https://b.example" })).toBe(true);
    const smb = file({ kind: "smb", hasPassword: true });
    expect(asksPasswordAgain(smb, { ...smb, domain: "CORP" })).toBe(true);
    // A new password typed in replaces the saved one, and nothing saved asks nothing.
    expect(asksPasswordAgain(smb, { ...smb, domain: "CORP", password: "pw" })).toBe(false);
    expect(asksPasswordAgain({ ...smb, hasPassword: false }, { ...smb, domain: "CORP" })).toBe(false);
  });
});

describe("X11 host form (R47)", () => {
  it("keeps X11 forwarding and the desktop command for an SSH host, off and empty by default", () => {
    expect(formHost(host())).toMatchObject({ x11: null, x11Desktop: null });
    expect(formHost(host({ x11: true, x11Desktop: " startxfce4 " }))).toMatchObject({ x11: true, x11Desktop: "startxfce4" });
    expect(formHost(host({ x11: false, x11Desktop: "  " }))).toMatchObject({ x11: null, x11Desktop: null });
  });

  it("drops X11 from a host switched away from SSH, with agent forwarding and legacy algorithms", () => {
    const was = host({ x11: true, x11Desktop: "mate-session", forwardAgent: true, algorithms: { ciphers: ["3des-cbc"] } });
    for (const kind of ["rdp", "mosh", "telnet", "vnc", "ftp"] as const)
      expect(formHost({ ...was, kind })).toMatchObject({ x11: null, x11Desktop: null, forwardAgent: null, algorithms: null });
  });
  it("drops a jump host left on a file host, which never goes through one", () => {
    const was = host({ jumpHostId: "bastion", proxyId: "p1" });
    for (const kind of ["ftp", "smb", "webdav", "s3"] as const) {
      expect(formHost({ ...was, kind }).jumpHostId).toBeNull();
      expect(validateHost({ ...was, kind }, []).proxyId).toBeUndefined();
    }
    expect(formHost(was).jumpHostId).toBe("bastion");
  });
});

describe("database host form", () => {
  const pg = (over: Partial<Host> = {}): Host =>
    host({ kind: "postgres", identityFile: "", authType: null, port: 0, user: "app", hasPassword: true, ...over });
  const lite = (over: Partial<Host> = {}): Host =>
    host({ kind: "sqlite", hostname: "", identityFile: "", user: "", authType: null, port: 0, file: "/data/app.db", ...over });

  it("needs a PostgreSQL host's name, user and password, with the port optional", () => {
    expect(validateHost(pg(), [])).toEqual({});
    expect(validateHost(pg({ port: 6432, tls: "none" }), [])).toEqual({});
    const errors = validateHost(pg({ hostname: " ", user: "", hasPassword: false, port: 70000, tls: "tls" }), []);
    expect(Object.keys(errors).sort()).toEqual(["hostname", "password", "port", "tls", "user"]);
  });

  it("needs a SQLite file chosen in the dialog, and nothing else", () => {
    expect(validateHost(lite(), [])).toEqual({});
    expect(validateHost(lite({ file: null, filePick: "token" }), [])).toEqual({});
    expect(validateHost(lite({ file: null }), []).file).toBe("hostForm.error.file");
    expect(formHost(lite({ hostname: "left", user: "over", jumpHostId: "j", proxyId: "p", tls: "none" }))).toMatchObject({
      hostname: "",
      user: "",
      jumpHostId: null,
      proxyId: null,
      tls: null,
      authType: null,
    });
  });

  it("names a PostgreSQL host without TLS unencrypted unless a jump host carries it to its own loopback", () => {
    expect(unencrypted(pg({ tls: "none" }))).toBe(true);
    expect(unencrypted(pg({ tls: "none", jumpHostId: "bastion" }))).toBe(true); // the jump host's network is still crossed
    expect(unencrypted(pg({ tls: "none", jumpHostId: "bastion", hostname: "127.0.0.1" }))).toBe(false);
    expect(unencrypted(pg({ tls: "none", hostname: "localhost" }))).toBe(true); // no jump host: nothing carries it
    expect(unencrypted(pg())).toBe(false);
    expect(unencrypted(lite())).toBe(false);
  });

  it("asks for the password again after the TLS mode changes", () => {
    expect(asksPasswordAgain(pg(), pg({ tls: "none" }))).toBe(true);
    expect(asksPasswordAgain(pg(), pg({ tls: "verify" }))).toBe(false); // verify is the default
    expect(asksPasswordAgain(pg(), pg({ tls: "none", password: "pw" }))).toBe(false);
  });

  it("treats MySQL, SQL Server and Oracle hosts as PostgreSQL ones", () => {
    for (const kind of ["mysql", "mssql", "oracle"] as const) {
      const db = (over: Partial<Host> = {}) => pg({ kind, database: "FREEPDB1", ...over });
      expect(validateHost(db(), [])).toEqual({});
      expect(validateHost(db({ user: "", hostname: "", tls: "prefer" as never }), [])).toMatchObject({
        user: "hostForm.error.user",
        hostname: "hostForm.error.hostname",
        tls: "hostForm.error.dbTls",
      });
      expect(unencrypted(db({ tls: "none" }))).toBe(true);
      expect(unencrypted(db({ tls: "none", jumpHostId: "bastion", hostname: "127.0.0.1" }))).toBe(false);
      expect(asksPasswordAgain(db(), db({ tls: "none" }))).toBe(true);
      // The password is sealed to the kind too, and a URL left from a
      // WebDAV or rqlite host is not this one's.
      expect(asksPasswordAgain(pg({ hasPassword: true }), db())).toBe(true);
      expect(formHost(db({ url: "https://left.over" })).url).toBeNull();
    }
    expect(validateHost(pg({ kind: "oracle", database: "" }), []).database).toBe("hostForm.error.serviceName");
  });

  it("needs an rqlite node's http(s) URL, with the user and password optional", () => {
    const rq = (over: Partial<Host> = {}) => host({ kind: "rqlite", hostname: "", port: 0, user: "", identityFile: "", authType: null, url: "https://rq.lan:4001", ...over });
    expect(validateHost(rq(), [])).toEqual({});
    expect(validateHost(rq({ url: "rq.lan:4001" }), []).url).toBe("hostForm.error.url");
    expect(validateHost(rq({ user: "app" }), []).password).toBe("hostForm.error.password");
    expect(unencrypted(rq({ url: "http://rq.lan:4001" }))).toBe(true);
    expect(unencrypted(rq({ url: "http://localhost:4001", jumpHostId: "bastion" }))).toBe(false);
    expect(unencrypted(rq())).toBe(false);
    expect(asksPasswordAgain(rq({ hasPassword: true }), rq({ url: "http://rq.lan:4001" }))).toBe(true);
    // Its URL is its address: a host name, port, database or TLS mode left
    // from another kind is dropped.
    expect(formHost(rq({ hostname: "left", port: 5432, database: "over", tls: "none" }))).toMatchObject({
      hostname: "",
      port: 0,
      database: null,
      tls: null,
      url: "https://rq.lan:4001",
    });
  });
});

describe("Kubernetes host form", () => {
  const k8s = (over: Partial<Host> = {}): Host =>
    host({ kind: "kubernetes", hostname: "k8s.lan", identityFile: "", authType: null, port: 6443, user: "", url: "https://k8s.lan:6443", namespace: "apps", ...over });

  it("needs an https:// API server and a valid namespace, and nothing SSH asks for", () => {
    expect(validateHost(k8s(), [])).toEqual({});
    expect(validateHost(k8s({ namespace: null, hasPassword: true }), [])).toEqual({});
    expect(validateHost(k8s({ url: "http://k8s.lan:8080" }), []).url).toBe("hostForm.error.clusterUrl");
    expect(validateHost(k8s({ namespace: "Not_Valid" }), []).namespace).toBe("hostForm.error.namespace");
    expect(formHost(k8s({ user: "left", authType: "key", tls: "none" }))).toMatchObject({ user: "", authType: null, tls: null });
  });

  it("asks for the token again after the server URL changes", () => {
    expect(asksPasswordAgain(k8s({ hasPassword: true }), k8s({ url: "https://other.lan" }))).toBe(true);
    expect(asksPasswordAgain(k8s({ hasPassword: true }), k8s({ namespace: "web" }))).toBe(false);
    expect(unencrypted(k8s())).toBe(false);
  });
});

describe("Redis, MongoDB and Elasticsearch host forms", () => {
  const db = (kind: "redis" | "mongodb", over: Partial<Host> = {}): Host =>
    host({ kind, identityFile: "", authType: null, port: 0, user: "", ...over });
  const es = (over: Partial<Host> = {}): Host =>
    host({ kind: "elasticsearch", hostname: "", identityFile: "", authType: null, port: 0, user: "", url: "https://es.lan:9200", ...over });

  // R56: Redis takes a password alone, and with neither user nor password
  // each signs in to nothing; a Redis database is a number.
  it("needs only a host name, with sign-in optional", () => {
    expect(validateHost(db("redis"), [])).toEqual({});
    expect(validateHost(db("mongodb", { user: "app" }), [])).toEqual({});
    expect(validateHost(db("redis", { database: "3", tls: "none" }), [])).toEqual({});
    const errors = validateHost(db("redis", { hostname: "", database: "x", tls: "tls", port: 70000 }), []);
    expect(Object.keys(errors).sort()).toEqual(["database", "hostname", "port", "tls"]);
  });

  it("addresses an Elasticsearch node by URL alone", () => {
    expect(validateHost(es(), [])).toEqual({});
    expect(validateHost(es({ url: "ftp://es.lan" }), []).url).toBe("hostForm.error.url");
    expect(formHost(es({ hostname: "left", port: 22, tls: "none" }))).toMatchObject({ hostname: "", port: 0, tls: null });
  });

  // KTD25: without TLS, or over http://, each is unencrypted unless the
  // database runs on the jump host itself; the password is bound to the
  // TLS mode, or to the URL.
  it("names plain connections unencrypted and asks for the password again when they change", () => {
    expect(unencrypted(db("redis", { tls: "none" }))).toBe(true);
    expect(unencrypted(db("mongodb", { tls: "none", jumpHostId: "bastion", hostname: "localhost" }))).toBe(false);
    expect(unencrypted(db("mongodb"))).toBe(false);
    expect(unencrypted(es({ url: "http://es.lan:9200" }))).toBe(true);
    expect(unencrypted(es({ url: "http://127.0.0.1:9200", jumpHostId: "bastion" }))).toBe(false);
    expect(unencrypted(es())).toBe(false);
    expect(asksPasswordAgain(db("redis", { hasPassword: true }), db("redis", { tls: "none" }))).toBe(true);
    expect(asksPasswordAgain(es({ hasPassword: true }), es({ url: "http://es.lan:9200" }))).toBe(true);
    expect(asksPasswordAgain(es({ hasPassword: true }), es())).toBe(false);
  });
});

describe("changing a host's kind", () => {
  // A PostgreSQL password left in the form must not become a cluster's
  // bearer token, or reach any other kind's server.
  it("drops a typed password when the kind changes", () => {
    const pg = host({ kind: "postgres", port: 5432, password: "pg-secret" });
    for (const k of ["kubernetes", "elasticsearch", "rqlite", "redis", "ssh"]) expect(passwordForKind(pg, k)).toBe("");
    expect(passwordForKind(pg, "postgres")).toBe("pg-secret");
  });
});

// store.Host.SecretAAD seals a saved password to the host's ID, host name,
// port and user, then to its kind and TLS mode, URL or domain. SaveHost
// does not reseal it, so any change to those leaves it unopenable.
describe("asking for the saved password again", () => {
  const saved = (over: Partial<Host> = {}): Host => host({ hasPassword: true, ...over });

  it("asks after a database host's host name, port or user changes", () => {
    const pg = saved({ kind: "postgres", port: 5432, user: "app", identityFile: "", authType: null, tls: "verify", database: "app" });
    expect(asksPasswordAgain(pg, { ...pg, hostname: "db2.example" })).toBe(true);
    expect(asksPasswordAgain(pg, { ...pg, port: 5433 })).toBe(true);
    expect(asksPasswordAgain(pg, { ...pg, user: "admin" })).toBe(true);
    expect(asksPasswordAgain(pg, { ...pg, alias: "renamed", database: "other" })).toBe(false);
  });

  it("asks after an SSH or file host's address changes too", () => {
    expect(asksPasswordAgain(saved(), saved({ hostname: "web2.example" }))).toBe(true);
    const ftp = saved({ kind: "ftp", port: 21, identityFile: "", authType: null });
    expect(asksPasswordAgain(ftp, { ...ftp, user: "other" })).toBe(true);
  });

  // f42a97c drops a typed password on a kind change; the kind is sealed in.
  it("asks after the kind changes between kinds that seal it", () => {
    const redis = saved({ kind: "redis", port: 0, user: "", identityFile: "", authType: null });
    expect(asksPasswordAgain(redis, { ...redis, kind: "mongodb" })).toBe(true);
    const es = saved({ kind: "elasticsearch", hostname: "", port: 0, user: "", identityFile: "", authType: null, url: "https://es.lan:9200" });
    expect(asksPasswordAgain(es, { ...es, kind: "rqlite" })).toBe(true);
    const k8s = saved({ kind: "kubernetes", hostname: "", port: 0, user: "", identityFile: "", authType: null, url: "https://k8s.lan:6443" });
    expect(asksPasswordAgain(k8s, { ...k8s, kind: "elasticsearch" })).toBe(true);
    expect(asksPasswordAgain(saved(), saved({ kind: "ftp" }))).toBe(true);
    // Neither SSH nor RDP seals its kind.
    expect(asksPasswordAgain(saved(), saved({ kind: "rdp" }))).toBe(false);
  });

  // The editor compares what it will save: a host name, port or user that
  // the kind drops on save (an Elasticsearch node is its URL) is no change.
  it("ignores fields the kind does not save", () => {
    const es = saved({ kind: "elasticsearch", hostname: "", port: 0, user: "", identityFile: "", authType: null, url: "https://es.lan:9200" });
    expect(asksPasswordAgain(es, { ...es, hostname: "left.over", port: 5432 })).toBe(false);
    // A cluster dials the host and port of its URL, which is what is sealed.
    const k8s = saved({ kind: "kubernetes", hostname: "k8s.lan", port: 6443, user: "", identityFile: "", authType: null, url: "https://k8s.lan:6443" });
    expect(asksPasswordAgain(k8s, { ...k8s, user: "left" })).toBe(false);
  });
});

// R15, KTD5: a host names one VPN profile, never with a proxy.
describe("host VPN", () => {
  const vpns = [{ id: "v1", name: "Atlas VPN", type: "wireguard", tier: "app" }];

  it("refuses a VPN together with a proxy, on any kind", () => {
    expect(validateHost(host({ vpnId: "v1", proxyId: "p1" }), []).vpnId).toBe("hostForm.error.vpnProxy");
    expect(validateHost(host({ kind: "ftp", identityFile: "", user: "", authType: null, port: 0, vpnId: "v1", proxyId: "p1" }), []).vpnId).toBe("hostForm.error.vpnProxy");
    expect(validateHost(host({ vpnId: "v1" }), [])).toEqual({});
    expect(validateHost(host({ kind: "mosh", vpnId: "v1" }), [])).toEqual({});
  });

  it("shows missing for a host naming a deleted or held profile", () => {
    expect(missingVpn(host({ vpnId: "gone" }), vpns)).toBe(true);
    expect(missingVpn(host({ vpnId: "held" }), vpns)).toBe(true);
    expect(missingVpn(host({ vpnId: "v1" }), vpns)).toBe(false);
    expect(missingVpn(host(), vpns)).toBe(false);
  });
});
