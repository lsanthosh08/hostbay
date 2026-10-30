import { isFileHost, type AppError, type Host, type HostKeyChange, type HostKeyPrompt } from "../api";
import { vpnCertOf, type VpnCert } from "./vpnCert";

/** What a failed connect asks of the user next. Every step carries the
 *  backend's message, so none ends in a silent close. */
export type Step =
  | { kind: "trust"; message: string; prompt: HostKeyPrompt }
  // A changed key is refused: it shows both fingerprints and, at most,
  // removal of the old key behind OS verification (R13, AE2). Never accept.
  | { kind: "keyChanged"; message: string; change: HostKeyChange }
  // The saved password of `host` (this host or a jump host) is missing or no
  // longer opens: ask for it, never send an empty one (R29).
  | { kind: "secret"; message: string; host: Host }
  // A saved proxy's password is missing or no longer opens (it is sealed to
  // the proxy's address): ask for it and save it to the proxy.
  | { kind: "proxySecret"; message: string; proxyId: string }
  // No Kerberos ticket: the user runs kinit outside Hostbay, then retries.
  | { kind: "kerberos"; message: string }
  // The host's proxy waits for confirmation in Sync settings, or no longer
  // exists. Neither ever falls back to a direct connection (KTD22).
  | { kind: "proxyHeld"; message: string }
  | { kind: "proxyMissing"; message: string }
  // Likewise the host's VPN profile waits for confirmation in Sync settings;
  // or it no longer exists, or two connected profiles reach an unbound host,
  // and the user names one on the host (R16, R17, AE2).
  | { kind: "vpnHeld"; message: string }
  | { kind: "vpnChoose"; message: string }
  // The VPN gateway's certificate is not trusted yet: trust it, then retry.
  | { kind: "vpnCert"; message: string; vpnId: string; cert: VpnCert }
  // A changed gateway certificate is refused (R21, AE8): it can only be
  // forgotten, never accepted here.
  | { kind: "vpnCertChanged"; message: string; vpnId: string; cert: VpnCert }
  | { kind: "unlock"; message: string }
  | { kind: "wake"; message: string }
  | { kind: "verify"; message: string }
  | { kind: "helperMissing"; message: string }
  | { kind: "reconnect"; message: string }
  // proxy: the host's proxy failed, so the target was never dialled.
  | { kind: "failure"; message: string; proxy?: true };

/** The next step for a connect that failed with `err` (App.doConnect's
 *  branches, one for one). `wasLive`: the session had gone live, so the drop
 *  shows as a banner over its views instead of the full-body status view. */
export function route(err: AppError, ctx: { host?: Host; hosts?: Host[]; wasLive?: boolean }): Step {
  const { message, code, details } = err;
  if (ctx.wasLive) return { kind: "reconnect", message };
  switch (code) {
    case "SecretNeeded": {
      if (details?.reason === "locked") return { kind: "unlock", message };
      if (details?.proxyId) return { kind: "proxySecret", message, proxyId: details.proxyId };
      if (details?.kind === "kerberos") return { kind: "kerberos", message };
      const needs = ctx.hosts?.find((h) => h.id === details?.hostId);
      // A file host with a user signs in with its password (none is
      // anonymous); RDP and VNC always do, and SPICE once its server refused
      // the empty ticket sent for no password. A network database always
      // signs in with its password too, and Redis, MongoDB, Elasticsearch
      // and rqlite once the server refused to go without one.
      const always = needs?.kind === "rdp" || needs?.kind === "vnc" || needs?.kind === "spice" || needs?.kind === "postgres" || needs?.kind === "kubernetes" ||
        needs?.kind === "redis" || needs?.kind === "mongodb" || needs?.kind === "elasticsearch" ||
        needs?.kind === "mysql" || needs?.kind === "mssql" || needs?.kind === "oracle" || needs?.kind === "rqlite";
      if (needs && (needs.authType === "password" || always || (isFileHost(needs) && needs.user)))
        return { kind: "secret", message, host: needs };
      break;
    }
    case "HostKeyUnknown":
      if (details?.requestId) return { kind: "trust", message, prompt: details as HostKeyPrompt };
      break;
    case "HostKeyChanged":
      // A certificate refusal names the pinned one "expected" and has no
      // stored key to remove.
      return {
        kind: "keyChanged",
        message,
        change: { removeId: "", knownFingerprints: details?.expected ?? "", hop: "0", keyType: "", ...details } as HostKeyChange,
      };
    case "Unreachable":
      if (ctx.host?.mac) return { kind: "wake", message };
      break;
    case "ProxyFailed":
      if (details?.reason === "held") return { kind: "proxyHeld", message };
      if (details?.reason === "missing") return { kind: "proxyMissing", message };
      return { kind: "failure", message, proxy: true };
    case "VPNFailed": {
      const cert = vpnCertOf(err);
      if (cert && details?.vpnId) return { kind: cert.pinned === undefined ? "vpnCert" : "vpnCertChanged", message, vpnId: details.vpnId, cert };
      if (details?.reason === "held") return { kind: "vpnHeld", message };
      if (details?.reason === "missing") return { kind: "vpnChoose", message };
      break;
    }
    case "VPNAmbiguous":
      return { kind: "vpnChoose", message };
    case "VerifyFailed":
      return { kind: "verify", message };
    case "HelperMissing":
      return { kind: "helperMissing", message };
  }
  return { kind: "failure", message };
}

/** Which service answers a trust step: an RDP certificate the RDP service, a
 *  VNC server's VeNCrypt certificate the VNC service, a SPICE TLS port's
 *  certificate the SPICE service, a database server's certificate the
 *  database service, a file host's
 *  certificate (ftpCert, webdavCert, s3Cert) the transfer service, and an
 *  SSH host key (any hop) the connection service. */
export function trustService(prompt: HostKeyPrompt): "rdp" | "vnc" | "spice" | "database" | "kubernetes" | "transfer" | "ssh" {
  if (prompt.kind === "kubernetesCert") return "kubernetes";
  if (prompt.kind === "rdpCert") return "rdp";
  if (prompt.kind === "vncCert") return "vnc";
  if (prompt.kind === "spiceCert") return "spice";
  if (prompt.kind === "postgresCert") return "database";
  if (prompt.kind === "redisCert" || prompt.kind === "mongodbCert" || prompt.kind === "elasticsearchCert") return "database";
  if (prompt.kind === "mysqlCert" || prompt.kind === "mssqlCert" || prompt.kind === "oracleCert" || prompt.kind === "rqliteCert") return "database";
  return prompt.kind?.endsWith("Cert") ? "transfer" : "ssh";
}
