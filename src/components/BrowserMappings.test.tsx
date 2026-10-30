import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type { BrowserMapping, InstalledBrowser, Vpn } from "../api";
import { en } from "../i18n/en";
import { BrowserMappingsView } from "./BrowserMappings";

const noop = () => {};
const chrome: InstalledBrowser = { id: "chrome", name: "Google Chrome", profiles: [{ dir: "Profile 3", name: "Work" }] };
const vpns: Vpn[] = [
  { id: "app1", name: "Atlas", type: "wireguard", tier: "app", tags: [] },
  { id: "ext1", name: "Office", type: "other", tier: "external", tags: [] },
];
const render = (browsers: InstalledBrowser[], mappings: BrowserMapping[]) =>
  renderToStaticMarkup(createElement(BrowserMappingsView, { browsers, mappings, vpns, onSave: noop, onRemove: noop, onLaunch: noop }));

describe("browser mappings", () => {
  it("offers only app-scoped VPNs and the browser's profiles by name", () => {
    const html = render([chrome], []);
    expect(html).toContain(">Atlas</option>");
    expect(html).not.toContain(">Office</option>");
    expect(html).toContain('value="Profile 3"');
    expect(html).toContain(">Work</option>");
  });

  it("says when a mapping's VPN has no SOCKS endpoint (AE6)", () => {
    const html = render([chrome], [{ browser: "chrome", profile: "Profile 3", vpn: "ext1" }]);
    expect(html).toContain("Google Chrome · Work → Office");
    expect(html).toContain(en["vpn.browserNoSocks"].replace("{vpn}", "Office"));
  });

  it("with no browser installed, says so", () => {
    expect(render([], [])).toContain(en["vpn.browserNone"]);
  });
});
