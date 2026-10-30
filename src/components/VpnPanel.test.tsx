import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type { Host, Vpn, VpnState } from "../api";
import { en } from "../i18n/en";
import { VpnPanelView } from "./VpnPanel";

const noop = () => {};
const vpn = (over: Partial<Vpn> = {}): Vpn => ({ id: "v1", name: "Atlas VPN", type: "wireguard", tier: "app", tags: ["atlas"], ...over });
const host = (tags: string[]): Host => ({ id: "h", alias: "h", hostname: "h", port: 22, user: "u", identityFile: "", tags });

function render(vpns: Vpn[], states: Record<string, VpnState> = {}, over: { tag?: string; text?: string; hosts?: Host[] } = {}) {
  return renderToStaticMarkup(
    createElement(VpnPanelView, {
      vpns,
      states,
      hosts: over.hosts ?? [],
      using: {},
      now: 0,
      tag: over.tag ?? "",
      text: over.text ?? "",
      announce: "",
      onTag: noop,
      onText: noop,
      onNew: noop,
      onEdit: noop,
      onConnect: noop,
      onDisconnect: noop,
    })
  );
}

const buttons = (html: string) => [...html.matchAll(/<button[^>]*>(.*?)<\/button>/g)].map((m) => m[1].replace(/<[^>]+>/g, ""));

describe("VPN panel", () => {
  it("with no profiles, offers a new one", () => {
    const html = render([]);
    expect(html).toContain(en["vpn.empty.title"]);
    expect(buttons(html)).toContain(en["vpn.new"]);
  });

  it("shows a failed profile's reason and Retry", () => {
    const html = render([vpn()], { v1: { id: "v1", status: "failed", reason: "handshake timed out" } });
    expect(html).toContain("handshake timed out");
    expect(html).toContain(en["vpn.status.failed"]);
    expect(buttons(html)).toContain(en["vpn.retry"]);
  });

  it("shows a connected profile's address and routes", () => {
    const html = render([vpn()], { v1: { id: "v1", status: "connected", address: ["10.8.0.2/32"], routes: ["10.0.0.0/8"], since: 0 } });
    expect(html).toContain("10.8.0.2/32");
    expect(html).toContain("10.0.0.0/8");
    expect(html).toContain(en["vpn.status.connected"]);
  });

  it("filters by tag, case-insensitively, with chips counting hosts and profiles", () => {
    const list = [vpn({ tags: ["Atlas"] }), vpn({ id: "v2", name: "Beta VPN", tags: ["beta"] })];
    const html = render(list, {}, { tag: "atlas", hosts: [host(["atlas"]), host(["ATLAS"])] });
    expect(html).toContain("Atlas VPN");
    expect(html).not.toContain("Beta VPN");
    expect(html).toMatch(/aria-pressed="true"[^>]*>Atlas \(3\)/);
    expect(html).toMatch(/aria-pressed="false"[^>]*>beta \(1\)/);
  });

  it("names the SSL VPN type without a vendor name", () => {
    const html = render([vpn({ type: "sslvpn" })]);
    expect(html).toContain("SSL VPN (PPP over TLS)");
    expect(html).not.toMatch(/forti/i);
  });

  it("announces state changes in a live region", () => {
    expect(render([vpn()])).toContain('aria-live="polite"');
  });
});
