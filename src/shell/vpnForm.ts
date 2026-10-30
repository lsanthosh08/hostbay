import type { Host, Vpn } from "../api";
import { t, type MessageKey } from "../i18n";

/** The supported-types table (R2): each type's UI name and tier. The SSL VPN
 *  type never shows its vendor's name. App-scoped types connect inside
 *  Hostbay; system-wide ones through Hostbay's network helper or the
 *  system's own VPN service. */
export const VPN_TYPES: { id: string; label: MessageKey; tier: "app" | "system" | "external" }[] = [
  { id: "wireguard", label: "vpn.type.wireguard", tier: "app" },
  { id: "tailscale", label: "vpn.type.tailscale", tier: "app" },
  { id: "anyconnect", label: "vpn.type.anyconnect", tier: "app" },
  { id: "sslvpn", label: "vpn.type.sslvpn", tier: "app" },
  { id: "globalprotect", label: "vpn.type.globalprotect", tier: "app" },
  { id: "pulse", label: "vpn.type.pulse", tier: "app" },
  { id: "f5", label: "vpn.type.f5", tier: "app" },
  { id: "array", label: "vpn.type.array", tier: "app" },
  { id: "zerotier", label: "vpn.type.zerotier", tier: "external" },
  { id: "openvpn", label: "vpn.type.openvpn", tier: "system" },
  { id: "ipsec", label: "vpn.type.ipsec", tier: "system" },
  { id: "perimeter81", label: "vpn.type.perimeter81", tier: "external" },
  { id: "other", label: "vpn.type.other", tier: "external" },
];

const typeOf = (id: string) => VPN_TYPES.find((x) => x.id === id);
export const typeLabel = (id: string): string => {
  const ty = typeOf(id);
  return ty ? t(ty.label) : id;
};
export const tierFor = (id: string) => typeOf(id)?.tier ?? "external";

// The OpenConnect protocols (internal/vpn/openconnect/launch.go).
const OPENCONNECT = ["anyconnect", "sslvpn", "globalprotect", "pulse", "nc", "f5", "array"];

export type VpnAuth = "password" | "certificate" | "otp" | "browser";
export const AUTH_LABEL: Record<VpnAuth, MessageKey> = {
  password: "vpnForm.auth.password",
  certificate: "vpnForm.auth.certificate",
  otp: "vpnForm.auth.otp",
  browser: "vpnForm.auth.browser",
};

/** The sign-ins a type offers (R20). Browser sign-in only where its engine
 *  handles it: OpenConnect's AnyConnect (browserTypes in launch.go). Hostbay
 *  cannot send OpenConnect a client certificate yet, so it is not offered. */
export function authsFor(type: string): VpnAuth[] {
  if (OPENCONNECT.includes(type)) return type === "anyconnect" ? ["password", "otp", "browser"] : ["password", "otp"];
  if (type === "openvpn" || type === "ipsec") return ["password", "certificate"];
  return [];
}

/** A gateway Hostbay dials; Tailscale's control server is optional and an
 *  external client needs none. */
export const needsGateway = (type: string) => type === "wireguard" || OPENCONNECT.includes(type) || type === "openvpn" || type === "ipsec";

/** The port a type usually listens on. */
export const defaultPort = (type: string) => (type === "wireguard" ? 51820 : type === "openvpn" ? 1194 : needsGateway(type) ? 443 : 0);

export const FULL_TUNNEL = ["0.0.0.0/0", "::/0"];
export const isFullTunnel = (routes?: string[] | null) => FULL_TUNNEL.every((r) => routes?.includes(r));

export type VpnField = "name" | "gateway" | "port" | "user" | "mtu" | "address" | "peerKey" | "privateKey" | "routes" | "dnsServers" | "dnsDomains";

// eslint-disable-next-line no-control-regex
const control = /[\x00-\x1f\x7f]/;
const spaceOrControl = /[\s\x00-\x1f\x7f]/;

function ipv4(s: string) {
  const m = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(s);
  return !!m && m.slice(1).every((o) => Number(o) <= 255);
}
function ipv6(s: string) {
  if (!s.includes(":") || /[[\]/]/.test(s)) return false;
  try {
    new URL(`http://[${s}]/`);
    return true;
  } catch {
    return false;
  }
}
export const validIp = (s: string) => ipv4(s) || ipv6(s);
/** A network such as 10.0.0.0/24, as netip.ParsePrefix reads it. */
export function validCidr(s: string) {
  const [ip, bits, extra] = s.split("/");
  if (extra !== undefined || bits === undefined || !/^\d{1,3}$/.test(bits)) return false;
  return ipv4(ip) ? Number(bits) <= 32 : ipv6(ip) && Number(bits) <= 128;
}

/** The profile form's inline errors; the backend checks the same
 *  (internal/app/vpns.go checkVPN), and WireGuard's own needs besides. */
export function validateVpn(v: Vpn): Partial<Record<VpnField, MessageKey>> {
  const e: Partial<Record<VpnField, MessageKey>> = {};
  const gw = v.gateway ?? "";
  if (!v.name.trim() || control.test(v.name)) e.name = "vpnForm.error.name";
  // An external profile's own app holds its gateway and keys.
  const external = v.tier === "external";
  if (gw.length > 253 || spaceOrControl.test(gw) || (needsGateway(v.type) && !external && !gw)) e.gateway = "vpnForm.error.gateway";
  if (v.port != null && v.port !== 0 && !(Number.isInteger(v.port) && v.port >= 1 && v.port <= 65535)) e.port = "hostForm.error.port";
  if (control.test(v.user ?? "")) e.user = "vpnForm.error.user";
  if (v.mtu && !(Number.isInteger(v.mtu) && v.mtu >= 576 && v.mtu <= 65535)) e.mtu = "vpnForm.error.mtu";
  if (Number.isNaN(v.mtu)) e.mtu = "vpnForm.error.mtu";
  if (!(v.address ?? []).every(validCidr)) e.address = "vpnForm.error.cidr";
  if (!(v.routes ?? []).every(validCidr)) e.routes = "vpnForm.error.cidr";
  if (!(v.dnsServers ?? []).every(validIp)) e.dnsServers = "vpnForm.error.dnsServer";
  if (!(v.dnsDomains ?? []).every((d) => d && !spaceOrControl.test(d) && !d.includes(","))) e.dnsDomains = "vpnForm.error.dnsDomain";
  if (v.type === "wireguard" && !external) {
    // Without these the tunnel cannot come up or routes nothing.
    if (!v.address?.length) e.address ??= "vpnForm.error.address";
    if (!v.peerKey || spaceOrControl.test(v.peerKey)) e.peerKey = "vpnForm.error.peerKey";
    if (!v.privateKey && !v.hasSecret) e.privateKey = "vpnForm.error.privateKey";
    if (!v.routes?.length) e.routes ??= "vpnForm.error.routes";
  }
  return e;
}

/** Saved secrets are sealed to the profile's type, gateway, user, tier and
 *  peer key (store.VPN.SecretAAD), so changing one leaves them unopenable
 *  unless a new secret is typed with the change. */
export function vpnAsksSecretAgain(before: Vpn, after: Vpn): boolean {
  if (!before.hasSecret || after.password || after.privateKey || after.presharedKey || after.authKey) return false;
  const aad = (v: Vpn) => [v.type, v.gateway ?? "", v.user ?? "", v.tier, v.peerKey ?? ""].join("\0");
  return aad(before) !== aad(after);
}

/** Every tag on hosts and profiles by its lower-case form (R6, R7): the
 *  casing first seen, and how many hosts and profiles carry it. */
export function tagCounts(hosts: Host[], vpns: Vpn[]) {
  const m = new Map<string, { label: string; hosts: number; vpns: number }>();
  const count = (tags: string[] | null | undefined, k: "hosts" | "vpns") => {
    // A tag listed twice on one item counts once.
    for (const tag of new Set((tags ?? []).map((x) => x.trim()).filter(Boolean))) {
      const c = m.get(tag.toLowerCase()) ?? { label: tag, hosts: 0, vpns: 0 };
      c[k]++;
      m.set(tag.toLowerCase(), c);
    }
  };
  // Profiles first, so the VPN list's chips keep the profiles' casing.
  vpns.forEach((v) => count(v.tags, "vpns"));
  hosts.forEach((h) => count(h.tags, "hosts"));
  return m;
}

/** Profiles carrying `tag` (any casing) whose name, gateway or tags contain `text`. */
export function filterVpns(vpns: Vpn[], tag: string, text: string) {
  const q = text.trim().toLowerCase();
  return vpns.filter(
    (v) =>
      (!tag || (v.tags ?? []).some((x) => x.toLowerCase() === tag.toLowerCase())) &&
      (!q || [v.name, v.gateway ?? "", ...(v.tags ?? [])].some((s) => s.toLowerCase().includes(q)))
  );
}

