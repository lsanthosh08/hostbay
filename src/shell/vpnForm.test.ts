import { describe, expect, it } from "vitest";
import type { Host, Vpn } from "../api";
import { en } from "../i18n/en";
import { addTag } from "../components/TagEditor";
import { FULL_TUNNEL, VPN_TYPES, filterVpns, isFullTunnel, tagCounts, tierFor, typeLabel, validateVpn, vpnAsksSecretAgain, authsFor, pickAuth } from "./vpnForm";

const vpn = (over: Partial<Vpn> = {}): Vpn => ({
  id: "v1",
  name: "Atlas VPN",
  type: "wireguard",
  tier: "app",
  gateway: "vpn.atlas.example",
  port: 51820,
  address: ["10.8.0.2/32"],
  peerKey: "pk",
  routes: ["10.0.0.0/8"],
  tags: ["atlas"],
  hasSecret: true,
  ...over,
});
const host = (tags: string[]): Host => ({ id: tags.join(), alias: "h", hostname: "h", port: 22, user: "u", identityFile: "", tags });

describe("VPN types", () => {
  it("labels the SSL VPN type without a vendor name", () => {
    expect(typeLabel("sslvpn")).toBe("SSL VPN (PPP over TLS)");
    for (const ty of VPN_TYPES) expect(en[ty.label]).not.toMatch(/forti/i);
    const all = Object.values(en).join("\n");
    expect(all).not.toMatch(/forti/i);
  });

  it("takes the tier from the supported-types table", () => {
    expect(tierFor("wireguard")).toBe("app");
    expect(tierFor("sslvpn")).toBe("app");
    expect(tierFor("openvpn")).toBe("system");
    expect(tierFor("zerotier")).toBe("external");
  });

  it("offers browser sign-in only where the engine supports it", () => {
    expect(authsFor("anyconnect")).toContain("browser");
    expect(authsFor("sslvpn")).not.toContain("browser");
    expect(authsFor("sslvpn")).toEqual(["password", "otp"]);
  });

  it("offers no certificate sign-in for OpenConnect types, which Hostbay cannot send yet", () => {
    for (const ty of ["anyconnect", "sslvpn", "globalprotect", "pulse", "nc", "f5", "array"]) expect(authsFor(ty)).not.toContain("certificate");
    expect(authsFor("openvpn")).toContain("certificate");
  });

  it("keeps a sign-in the type offers, else falls back to its first or none", () => {
    expect(pickAuth("anyconnect", "browser")).toBe("browser");
    expect(pickAuth("sslvpn", "certificate")).toBe("password");
    expect(pickAuth("openvpn", undefined)).toBe("password");
    expect(pickAuth("wireguard", "password")).toBe("");
  });
});

describe("VPN form", () => {
  it("accepts a complete WireGuard profile", () => {
    expect(validateVpn(vpn())).toEqual({});
  });

  it("needs a name, a gateway without spaces and networks it can parse", () => {
    const e = validateVpn(vpn({ name: " ", gateway: "a b", routes: ["10.0.0.0"], address: ["nope"], dnsServers: ["x"], mtu: 100 }));
    expect(Object.keys(e).sort()).toEqual(["address", "dnsServers", "gateway", "mtu", "name", "routes"]);
    expect(validateVpn(vpn({ routes: FULL_TUNNEL, dnsServers: ["10.0.0.53", "fd00::53"], dnsDomains: ["corp.example"] }))).toEqual({});
  });

  it("takes plain domain names and refuses wildcards, URLs and spaces", () => {
    expect(validateVpn(vpn({ domains: ["corp.example", "Wiki.Corp.Example."] }))).toEqual({});
    for (const d of ["*.corp.example", "http://x", "a b", '"x"', "a_b.example"]) expect(validateVpn(vpn({ domains: [d] })).domains, d).toBe("vpnForm.error.domain");
  });

  it("asks an external profile for neither gateway nor keys, which its own app holds", () => {
    for (const type of ["sslvpn", "anyconnect", "openvpn", "wireguard"])
      expect(validateVpn({ id: "", name: "Edge", type, tier: "external", tags: [] }), type).toEqual({});
  });

  it("needs a WireGuard private key unless one is saved", () => {
    expect(validateVpn(vpn({ hasSecret: false })).privateKey).toBeDefined();
    expect(validateVpn(vpn({ hasSecret: false, privateKey: "k" }))).toEqual({});
  });

  it("warns that a gateway change asks for the saved password again", () => {
    const saved = vpn({ type: "sslvpn", user: "me", auth: "password" });
    expect(vpnAsksSecretAgain(saved, { ...saved, gateway: "other.example" })).toBe(true);
    expect(vpnAsksSecretAgain(saved, { ...saved, user: "you" })).toBe(true);
    expect(vpnAsksSecretAgain(saved, { ...saved, peerKey: "other" })).toBe(true);
    expect(vpnAsksSecretAgain(saved, { ...saved, name: "renamed", port: 4443 })).toBe(false);
    expect(vpnAsksSecretAgain(saved, { ...saved, gateway: "other.example", password: "new" })).toBe(false);
    expect(vpnAsksSecretAgain({ ...saved, hasSecret: false }, { ...saved, gateway: "other.example" })).toBe(false);
  });

  it("knows a full tunnel", () => {
    expect(isFullTunnel(["::/0", "0.0.0.0/0"])).toBe(true);
    expect(isFullTunnel(["10.0.0.0/8"])).toBe(false);
  });
});

describe("tags", () => {
  it("keeps one tag when a case variant is added", () => {
    expect(addTag(["atlas"], " Atlas ")).toEqual(["atlas"]);
    expect(addTag(["atlas"], "beta")).toEqual(["atlas", "beta"]);
    expect(addTag(["atlas"], "  ")).toEqual(["atlas"]);
  });

  it("counts a tag across hosts and profiles, case-insensitively", () => {
    const counts = tagCounts([host(["atlas"]), host(["Atlas", "web"]), host([])], [vpn(), vpn({ id: "v2", tags: ["beta"] })]);
    expect(counts.get("atlas")).toEqual({ label: "atlas", hosts: 2, vpns: 1 });
    expect(counts.get("beta")).toEqual({ label: "beta", hosts: 0, vpns: 1 });
  });

  it("filters profiles by tag and by text", () => {
    const list = [vpn({ tags: ["Atlas"] }), vpn({ id: "v2", name: "Beta", tags: ["beta"] }), vpn({ id: "v3", name: "Other", tags: [] })];
    expect(filterVpns(list, "atlas", "").map((v) => v.id)).toEqual(["v1"]);
    expect(filterVpns(list, "", "bet").map((v) => v.id)).toEqual(["v2"]);
    expect(filterVpns(list, "", "").length).toBe(3);
  });
});

describe("client program path", () => {
  const ext = (clientPath?: string) => ({ id: "x", name: "Harmony", type: "perimeter81", tier: "external" as const, clientPath, tags: [] });
  it("accepts an absolute path and none, refuses a relative or odd one", () => {
    expect(validateVpn(ext()).clientPath).toBeUndefined();
    expect(validateVpn(ext("/Applications/Check Point SASE.app")).clientPath).toBeUndefined();
    expect(validateVpn(ext("C:\\Program Files\\App\\app.exe")).clientPath).toBeUndefined();
    expect(validateVpn(ext("Check Point SASE")).clientPath).toBe("vpnForm.error.clientPath");
    expect(validateVpn(ext("/Applications/x\n.app")).clientPath).toBe("vpnForm.error.clientPath");
  });
});
