import type * as api from "../api";
import { t } from "../i18n";

// The create form takes ports and mounts one per line, in the engine's own
// -p and -v syntax; these turn the lines into CreateOptions. The backend
// validates every value again.

const lines = (text: string) =>
  text
    .split("\n")
    .map((l) => l.trim())
    .filter(Boolean);

/** "8080:80", "0.0.0.0:8080:80", "[::1]:53:53/udp". No address means
 *  127.0.0.1, so a port opens to this computer only unless asked. */
export function parsePort(line: string): api.PortOption {
  let s = line.trim();
  let protocol = "tcp";
  const slash = s.lastIndexOf("/");
  if (slash > s.lastIndexOf(":")) {
    protocol = s.slice(slash + 1);
    s = s.slice(0, slash);
  }
  let hostAddress = "127.0.0.1";
  if (s.startsWith("[")) {
    const end = s.indexOf("]:");
    if (end < 0) throw new Error(t("containers.form.badPort", { line }));
    hostAddress = s.slice(1, end);
    s = s.slice(end + 2);
  }
  const parts = s.split(":");
  if (parts.length === 3 && !line.trim().startsWith("[")) hostAddress = parts.shift()!;
  const [hostPort, containerPort] = parts.map(Number);
  if (parts.length !== 2 || !hostPort || !containerPort) throw new Error(t("containers.form.badPort", { line }));
  return { hostAddress, hostPort, containerPort, protocol };
}

/** "source:/target" or "source:/target:ro"; a Windows source keeps its
 *  drive colon (C:\data:/data). */
export function parseMount(line: string): api.MountOption {
  let s = line.trim();
  const readOnly = s.endsWith(":ro");
  if (readOnly) s = s.slice(0, -3);
  const at = s.lastIndexOf(":");
  if (at <= 0) throw new Error(t("containers.form.badMount", { line }));
  return { source: s.slice(0, at), target: s.slice(at + 1), readOnly };
}

export type CreateForm = { image: string; name: string; ports: string; env: string; volumes: string; binds: string; restart: string };

export const toCreateOptions = (f: CreateForm): api.CreateOptions => ({
  image: f.image.trim(),
  name: f.name.trim(),
  ports: lines(f.ports).map(parsePort),
  env: lines(f.env),
  volumes: lines(f.volumes).map(parseMount),
  binds: lines(f.binds).map(parseMount),
  restart: f.restart,
});

/** Whether a port listens on this computer only; any other address needs
 *  the beyond-loopback warning (R33). */
export const loopback = (p: api.PortOption) =>
  p.hostAddress === "::1" || p.hostAddress === "localhost" || /^127\./.test(p.hostAddress);

/** A port's address and host port as the form takes them back: an IPv6
 *  address in brackets, so [::] does not read as ":::8080". */
export const listenAddress = (p: api.PortOption) => `${p.hostAddress.includes(":") ? `[${p.hostAddress}]` : p.hostAddress}:${p.hostPort}`;

/** Whether a remote host has none of the engines the tab drives, as the
 *  dashboard found them. */
export const noEngine = (d: api.DashboardInfo) => !d.dockerInstalled && !d.podmanInstalled && !d.nerdctlInstalled;
