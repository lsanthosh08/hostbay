import { hostKind, isDatabaseHost, isFileHost, type Host, type HostKind, type Proxy } from "../api";
import type { MessageKey } from "../i18n";

export type HostField =
  | "alias"
  | "hostname"
  | "user"
  | "port"
  | "rdpPort"
  | "identityFile"
  | "password"
  | "jumpHostId"
  | "tls"
  | "url"
  | "bucket"
  | "proxyId"
  | "keepalive"
  | "file"
  | "namespace"
  | "database";

const validPort = (p: number | null | undefined) => Number.isInteger(p) && p! >= 1 && p! <= 65535;

/** The database kinds at a host name and port with a TLS mode (KTD25). */
export const SQL_SERVERS: readonly HostKind[] = ["postgres", "mysql", "mssql", "oracle"];

/** A WebDAV folder or S3 endpoint: http:// or https:// with a host. */
function validUrl(raw: string | null | undefined): boolean {
  if (!raw || !/^https?:\/\//i.test(raw)) return false;
  try {
    return new URL(raw).hostname !== "";
  } catch {
    return false;
  }
}

/** The host editor's inline errors, by field; empty when the host can be
 *  saved. `hosts` are the saved ones, to follow a jump host's own chain. */
export function validateHost(h: Host, hosts: Host[]): Partial<Record<HostField, MessageKey>> {
  const e: Partial<Record<HostField, MessageKey>> = {};
  const kind = hostKind(h);
  if (!h.alias.trim()) e.alias = "hostForm.error.alias";
  // A host behind a jump host reaches it through the jump host's proxy, and
  // Mosh's UDP would go around a proxy (the backend refuses both too).
  if (h.proxyId && h.jumpHostId && !isFileHost(h)) e.proxyId = "hostForm.error.proxyJump";
  else if (h.proxyId && kind === "mosh") e.proxyId = "hostForm.error.proxyMosh";
  if (h.keepalive != null && !(Number.isInteger(h.keepalive) && h.keepalive >= 0 && h.keepalive <= 86400))
    e.keepalive = "hostForm.error.keepalive";
  if (isFileHost(h)) {
    // A file host signs in with its password, or anonymously with no user;
    // only S3 needs its access key ID. The port is optional (0 or empty is
    // the protocol's default). WebDAV and S3 connect to their URL alone.
    if (kind === "ftp" || kind === "smb") {
      if (!h.hostname.trim()) e.hostname = "hostForm.error.hostname";
      if (h.port && !validPort(h.port)) e.port = "hostForm.error.port";
    }
    if (kind === "ftp" && h.tls != null && !["explicit", "implicit", "none"].includes(h.tls)) e.tls = "hostForm.error.tls";
    if ((kind === "webdav" || kind === "s3") && !validUrl(h.url)) e.url = "hostForm.error.url";
    if (kind === "s3") {
      if (!h.user.trim()) e.user = "hostForm.error.accessKey";
      if (!h.bucket?.trim()) e.bucket = "hostForm.error.bucket";
    }
    return e;
  }
  if (kind === "kubernetes") {
    // An https:// API server; the token is optional (a client certificate
    // from the kubeconfig signs in instead), and the CA stays in Go.
    if (!/^https:\/\//i.test(h.url ?? "") || !validUrl(h.url)) e.url = "hostForm.error.clusterUrl";
    if (h.namespace && !/^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$/.test(h.namespace)) e.namespace = "hostForm.error.namespace";
    return jumpLoop(h, hosts, e);
  }
  if (kind === "sqlite") {
    // A file on this computer, chosen in Go's dialog; nothing else.
    if (!h.file && !h.filePick) e.file = "hostForm.error.file";
    return e;
  }
  if (kind === "elasticsearch") {
    // A node by its URL; with no user it signs in to nothing.
    if (!validUrl(h.url)) e.url = "hostForm.error.url";
    return jumpLoop(h, hosts, e);
  }
  if (kind === "rqlite") {
    // Connects to its URL; a user signs in with a password, none to nothing.
    if (!validUrl(h.url)) e.url = "hostForm.error.url";
    if (h.user.trim() && !h.password && !h.hasPassword) e.password = "hostForm.error.password";
    return jumpLoop(h, hosts, e);
  }
  if (!h.hostname.trim()) e.hostname = "hostForm.error.hostname";
  if (kind === "redis" || kind === "mongodb") {
    // User and password are optional: Redis takes a password alone, and
    // with neither each signs in to nothing. A port of 0 or empty is the
    // default.
    if (h.port && !validPort(h.port)) e.port = "hostForm.error.port";
    if (h.tls != null && !["verify", "none"].includes(h.tls)) e.tls = "hostForm.error.dbTls";
    if (kind === "redis" && h.database && !/^\d+$/.test(h.database)) e.database = "hostForm.error.redisDb";
    return jumpLoop(h, hosts, e);
  }
  if (SQL_SERVERS.includes(kind)) {
    // Signs in with a user and password; a port of 0 or empty is the
    // engine's default. Oracle opens a service by name.
    if (h.port && !validPort(h.port)) e.port = "hostForm.error.port";
    if (h.tls != null && !["verify", "none"].includes(h.tls)) e.tls = "hostForm.error.dbTls";
    if (!h.user.trim()) e.user = "hostForm.error.user";
    if (!h.password && !h.hasPassword) e.password = "hostForm.error.password";
    if (kind === "oracle" && !h.database?.trim()) e.database = "hostForm.error.serviceName";
    return jumpLoop(h, hosts, e);
  }
  if (kind === "vnc") {
    // A VNC host always has a password (Go signs in with it, KTD19); the
    // user is optional (Apple Remote Desktop, VeNCrypt X509Plain), and a
    // port of 0 or empty is 5900.
    if (h.port && !validPort(h.port)) e.port = "hostForm.error.port";
    if (h.tls != null && !["vencrypt", "none"].includes(h.tls)) e.tls = "hostForm.error.vncSecurity";
    if (!h.password && !h.hasPassword) e.password = "hostForm.error.password";
    return e;
  }
  if (kind === "spice") {
    // A SPICE host has no user and its password is optional (Go sends an
    // empty ticket for none); a port of 0 or empty is 5900.
    if (h.port && !validPort(h.port)) e.port = "hostForm.error.port";
    if (h.tls != null && !["tls", "none"].includes(h.tls)) e.tls = "hostForm.error.spiceSecurity";
    return jumpLoop(h, hosts, e);
  }
  // Telnet logs in inside the terminal and raw TCP has no log-in, so
  // neither needs a user or credentials. The agent and Kerberos need neither
  // a key file nor a password.
  const signsIn = kind !== "telnet" && kind !== "tcp";
  if (signsIn && !h.user.trim()) e.user = "hostForm.error.user";
  if (!validPort(h.port)) e.port = "hostForm.error.port";
  if (kind === "rdp" && h.rdpPort != null && !validPort(h.rdpPort)) e.rdpPort = "hostForm.error.port";
  if (signsIn) {
    const auth = kind === "rdp" ? "password" : h.authType ?? "key";
    if (auth === "key" && !h.identityFile.trim()) e.identityFile = "hostForm.error.key";
    if (auth === "password" && !h.password && !h.hasPassword) e.password = "hostForm.error.password";
  }
  return jumpLoop(h, hosts, e);
}

/** A chain that comes back to this host would loop (the backend refuses it too). */
function jumpLoop(h: Host, hosts: Host[], e: Partial<Record<HostField, MessageKey>>) {
  const seen = new Set<string>();
  for (let id = h.jumpHostId; id && !seen.has(id); id = hosts.find((x) => x.id === id)?.jumpHostId) {
    if (id === h.id) {
      e.jumpHostId = "hostForm.error.jumpLoop";
      break;
    }
    seen.add(id);
  }
  return e;
}

/** The host as saved or tested. A file host signs in with a password only
 *  when it has a user, which the backend reads from the user field, not
 *  from authType; raw TCP signs in to nothing. Agent forwarding, legacy
 *  algorithms and X11 are SSH's only, so a host switched away drops them,
 *  and X11 stays off unless turned on (R47). */
export function formHost(h: Host): Host {
  const kind = hostKind(h);
  const file = isFileHost(h);
  const vnc = kind === "vnc";
  const spice = kind === "spice";
  const db = isDatabaseHost(h);
  const k8s = kind === "kubernetes";
  const authType = file || vnc || spice || db || k8s || kind === "tcp" ? null : kind === "rdp" ? "password" : h.authType ?? "key";
  const port = (file || vnc || spice || db) && Number.isNaN(h.port) ? 0 : h.port;
  const ssh = kind === "ssh";
  // A SQLite host is a file here: no address, sign-in, TLS or route.
  const local = kind === "sqlite" ? { hostname: "", user: "", tls: null, jumpHostId: null, proxyId: null, vpnId: null, port: 0 } : {};
  // An Elasticsearch node is its URL: no host name, port or TLS mode of its
  // own; nor an rqlite node, which has no database name either. A database
  // server's address is its host name and port, not a URL.
  const node =
    kind === "elasticsearch"
      ? { hostname: "", port: 0, tls: null }
      : kind === "rqlite"
        ? { hostname: "", port: 0, database: null, tls: null }
        : SQL_SERVERS.includes(kind)
          ? { url: null }
          : {};
  return {
    ...h,
    // SPICE has no user; one left from another kind would bind the password.
    // A cluster signs in with its token or client certificate alone.
    user: spice || k8s ? "" : h.user,
    tls: k8s ? null : h.tls,
    authType,
    kind,
    port,
    forwardAgent: ssh ? h.forwardAgent : null,
    algorithms: ssh ? h.algorithms : null,
    x11: (ssh && h.x11) || null,
    x11Desktop: (ssh && h.x11Desktop?.trim()) || null,
    // A file host is dialled directly or through a proxy, never a jump host;
    // one left from another kind is hidden in the editor.
    jumpHostId: file ? null : h.jumpHostId,
    ...local,
    ...node,
  };
}

const loopback = (host: string) => /^(localhost|127(\.\d{1,3}){3}|::1|\[::1\])$/i.test(host.trim());

/** A URL's host name, or "" when it does not parse. */
function urlHost(raw: string): string {
  try {
    return new URL(raw).hostname;
  } catch {
    return "";
  }
}

/** A host that connects without encryption: raw TCP always, a file host
 *  set to plain FTP or an http:// WebDAV folder or S3 endpoint, or plain VNC
 *  or SPICE that no jump host's SSH connection carries (KTD18), or a
 *  PostgreSQL, MySQL, SQL Server or Oracle host without TLS or http://
 *  rqlite node, unless it is the jump host's own loopback
 *  (KTD25, as store.Host.Plaintext). */
export function unencrypted(h: Host): boolean {
  if (h.kind === "tcp") return true;
  if (SQL_SERVERS.includes(hostKind(h)) || h.kind === "redis" || h.kind === "mongodb")
    return h.tls === "none" && !(h.jumpHostId && loopback(h.hostname));
  if (h.kind === "elasticsearch" || h.kind === "rqlite")
    return /^http:\/\//i.test(h.url ?? "") && !(h.jumpHostId && loopback(urlHost(h.url ?? "")));
  if (h.kind === "vnc" || h.kind === "spice") return h.tls === "none" && !h.jumpHostId;
  if (h.kind === "ftp") return h.tls === "none";
  if (h.kind === "webdav" || h.kind === "s3") return /^http:\/\//i.test(h.url ?? "");
  return false;
}

/** The typed password to keep when the editor's kind changes to `kind`:
 *  none, so one typed for one server (a PostgreSQL password) never reaches
 *  another kind's (as a cluster's bearer token). */
export const passwordForKind = (h: Host, kind: string) => (kind === hostKind(h) ? h.password ?? "" : "");

/** What a saved password is sealed to, as store.Host.SecretAAD builds it
 *  from the host as the form saves it: its ID, host name, port and user,
 *  then for most kinds the kind and its FTP TLS mode, URL or SMB domain, a
 *  VNC or SPICE host's security mode (KTD18), a database host's TLS mode or
 *  node URL (KTD25), or a cluster's server URL (KTD28). Go sets a cluster's
 *  host and port from that URL, so the URL stands for them. */
function sealedTo(raw: Host): string {
  const h = formHost(raw);
  const k = hostKind(h);
  const tls = (def: string) => h.tls || def;
  const extra =
    k === "ftp"
      ? tls("explicit")
      : k === "webdav" || k === "s3" || k === "kubernetes" || k === "elasticsearch" || k === "rqlite"
        ? h.url ?? ""
        : k === "smb"
          ? h.domain ?? ""
          : k === "vnc"
            ? tls("vencrypt")
            : k === "spice"
              ? tls("tls")
              : isDatabaseHost(h)
                ? tls("verify")
                : null;
  const addr = k === "kubernetes" ? [h.id, h.user] : [h.id, h.hostname, String(h.port), h.user];
  return [...addr, ...(extra === null ? [] : [k, extra])].join("\0");
}

/** SaveHost keeps a saved password sealed as it was, so an edit to what it
 *  is sealed to leaves it unopenable and the next connect asks again,
 *  unless a new password is typed in with the change. */
export function asksPasswordAgain(before: Host, after: Host): boolean {
  if (!before.hasPassword || after.password) return false;
  return sealedTo(before) !== sealedTo(after);
}

export type ProxyField = "name" | "type" | "host" | "port" | "user" | "password";

const bytes = (s: string) => new TextEncoder().encode(s).length;
// Spaces and control characters cannot be part of a host name, and a line
// break in a user or password would break the SOCKS5 or CONNECT request.
// eslint-disable-next-line no-control-regex
const badHost = /[\s\x00-\x1f\x7f]/;
const lineBreak = /[\r\n\0]/;

/** The proxy form's inline errors, by field; the backend checks the same
 *  (internal/app/proxies.go checkProxy). */
export function validateProxy(p: Proxy): Partial<Record<ProxyField, MessageKey>> {
  const e: Partial<Record<ProxyField, MessageKey>> = {};
  const user = p.user ?? "";
  const pw = p.password ?? "";
  if (!p.name.trim()) e.name = "proxyForm.error.name";
  if (p.type !== "socks5" && p.type !== "http") e.type = "proxyForm.error.type";
  if (!p.host || p.host.length > 253 || badHost.test(p.host)) e.host = "proxyForm.error.host";
  if (!validPort(p.port)) e.port = "hostForm.error.port";
  if (bytes(user) > 255 || lineBreak.test(user)) e.user = "proxyForm.error.user";
  // HTTP Basic auth joins user and password with a colon.
  else if (p.type === "http" && user.includes(":")) e.user = "proxyForm.error.userColon";
  if (bytes(pw) > 255 || lineBreak.test(pw)) e.password = "proxyForm.error.password";
  return e;
}

/** A proxy's saved password is sealed to its type, address and user (KTD6),
 *  so changing one of them leaves it unopenable unless a new one is typed. */
export function proxyAsksPasswordAgain(before: Proxy, after: Proxy): boolean {
  if (!before.hasPassword || after.password) return false;
  return before.type !== after.type || before.host !== after.host || before.port !== after.port || (before.user ?? "") !== (after.user ?? "");
}
