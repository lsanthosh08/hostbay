---
title: Route allowlisted domains through a VPN in every browser
date: 2026-11-03
type: feat
artifact_contract: ce-unified-plan/v1
product_contract_source: ce-brainstorm
execution: code
---

# Route allowlisted domains through a VPN in every browser

## Goal Capsule

- **Objective:** While a VPN profile is connected, the sites the user allowlisted for it and the VPN's own networks reach that VPN from the user's everyday browsers (Safari, Chrome, Edge, Brave, Firefox), and everything else goes direct, with no separate window, profile or browser extension.
- **Means:** a proxy auto-config (PAC) that Hostbay serves on loopback and sets as the macOS system auto-proxy, plus host routes for VPNs that have no proxy port.
- **Product authority:** the user, in this session. Windows and Linux are not active scope.
- **Open blockers:** none.

---

## Product Contract

### Summary

Each VPN profile gets a list of domains to route through it. Hostbay turns the connected profiles' domains and networks into one system PAC on macOS, so every browser that follows the system proxy sends those sites through the right VPN and the rest direct. The "launch a browser profile through a VPN" feature, its mappings and its separate profiles are removed.

### Problem Frame

Today a site reaches an app-scoped VPN from a browser only through a Chrome that Hostbay launches with the VPN's proxy. That Chrome is a second window or a second profile, it refuses to open beside a running Chrome unless Hostbay keeps a signed-out copy of the profile, and Safari and Firefox are not covered at all. A browser extension would not close the gap: Safari web extensions have no proxy API, Chrome installs an extension from outside its store only through developer mode or an administrator policy, and release Firefox installs only signed add-ons. Harmony SASE and system-wide VPNs have no proxy port, so a domain list cannot reach them through a proxy either.

### Key Decisions

- **System PAC on macOS over browser extensions.** One mechanism covers all five browsers, needs nothing installed in them, and does not depend on store listings or signing. Governs R5, R6, R8. (session-settled: user-directed — chosen over Chrome/Firefox extensions and over extensions plus PAC: extensions cannot cover Safari and need developer mode or AMO signing to install)
- **Remove launch-through-VPN.** The PAC replaces it in the user's own browser, so the launcher, its mappings and its separate profiles go. Governs R12, R13.
- **A disconnected profile's domains go direct.** They leave the PAC when the profile disconnects, so allowlisted public sites still load; no request is sent to a proxy that is not there. Governs R7.
- **Domains for VPNs without a proxy port become host routes.** Harmony SASE and system-wide profiles carry an allowlisted domain as routes to its current addresses, re-resolved while connected. Governs R9, R10.
- **A domain belongs to one profile.** Two profiles claiming the same domain would make the route depend on order, so saving the second is refused. Governs R3.

### Requirements

**Domain lists**

- R1. Every VPN profile, of any tier, has a list of domains whose traffic goes through it, edited in the profile form and synced with the profile.
- R2. A domain entry covers the domain and all its subdomains (`corp.example` covers `wiki.corp.example`); an entry is a plain DNS name, and anything else is refused when saved.
- R3. Saving a profile whose list holds a domain another profile already lists, or a subdomain of one, is refused naming that profile.
- R4. A profile's own networks (its routes, plus the routes its tunnel pushed when connected) go through it as well as its domains, without the user listing them.

**Browsers through app-scoped VPNs (system PAC)**

- R5. While at least one connected app-scoped profile has domains or networks, Hostbay serves a PAC on loopback and sets it as the macOS automatic proxy configuration for every network service; when none does, it removes that setting.
- R6. The PAC sends each listed domain, and each address in a listed network, to the SOCKS listener of the profile that lists it, with no direct fallback for those, and sends everything else direct.
- R7. The PAC follows connects and disconnects within seconds: a profile's entries appear when it connects and leave when it disconnects, drops or is deleted.
- R8. Quitting Hostbay removes the system setting; if Hostbay ends without removing it, browsers that cannot fetch the PAC go direct, and Hostbay removes the stale setting on its next start.

**Domains through VPNs without a proxy port**

- R9. For a connected system-wide or external profile whose routes apply, each listed domain is resolved and routed to its tunnel as host routes, through the same routing plan, preview and auto-apply as its networks.
- R10. Hostbay re-resolves those domains while the profile stays connected, adding routes for new addresses and removing routes for addresses no longer returned.

**Visibility**

- R11. The VPN panel shows, per connected profile, how many domains and networks are routed through it, and Settings shows whether the system proxy is set by Hostbay.

**Removal**

- R12. The browser launch feature is removed: the launch action, the browser-profile mappings and their editor, and the separate browser profiles Hostbay created.
- R13. Importing a TunnelHub file no longer creates browser mappings; its Chrome entries are listed as skipped with the reason.

### Key Flows

- F1. Reach an internal site from Safari
  - **Trigger:** the user lists `corp.example` on the OD Internal profile and connects it.
  - **Steps:** Hostbay starts the profile's SOCKS listener, serves the PAC and sets the system auto-proxy; Safari asks the PAC for `wiki.corp.example` and gets the listener; `ipinfo.io` gets direct.
  - **Outcome:** the wiki loads through OD Internal; ipinfo.io shows the user's own public address.
  - **Covered by:** R1, R2, R5, R6
- F2. Disconnect
  - **Trigger:** the user disconnects OD Internal while another app-scoped profile stays connected.
  - **Steps:** the PAC drops OD Internal's entries; the system setting stays for the other profile.
  - **Outcome:** `wiki.corp.example` now goes direct; the other profile's sites still go through it.
  - **Covered by:** R5, R7
- F3. A domain through Harmony SASE
  - **Trigger:** the user lists `portal.example` on Harmony SASE, which is connected with auto-apply on.
  - **Steps:** Hostbay resolves `portal.example` and adds host routes to Harmony's tunnel through the routing plan; it re-resolves while Harmony stays up.
  - **Outcome:** `portal.example` goes through Harmony from every app, browsers included.
  - **Covered by:** R1, R9, R10

### Acceptance Examples

- AE1. Covers R3. Given Harmony SASE lists `corp.example`, when the user saves OD Internal with `wiki.corp.example`, the save is refused naming Harmony SASE.
- AE2. Covers R5, R8. Given no app-scoped profile is connected, the system automatic proxy setting is not Hostbay's; given Hostbay crashed with it set, the next start removes it.
- AE3. Covers R6. Given OD Internal is connected with network `10.212.0.0/16` and no domains, a request to `10.212.4.5` goes to its listener and a request to `ipinfo.io` goes direct.
- AE4. Covers R10. Given `portal.example` resolved to A and now resolves to B while Harmony is connected, the route to A is removed and a route to B is added.

### Scope Boundaries

- Browser extensions for Chrome, Firefox or Safari (deferred; the PAC covers these browsers without them).
- Windows and Linux system proxy settings (deferred).
- Apps that ignore the system proxy (command-line tools such as curl, some Electron apps) are not covered by R5 and R6.
- Per-browser on/off control.
- Wildcards or patterns other than "domain and its subdomains", and per-path rules.
- IPv6 networks and AAAA records for host routes in R9 (deferred).

### Dependencies / Assumptions

- Setting the macOS automatic proxy needs an administrator, so the privileged network helper gains that operation; existing installs see the helper's administrator prompt once to upgrade it.
- Safari, Chrome, Edge and Brave follow the macOS system proxy; Firefox does by default ("Use system proxy settings"). A Firefox set to its own proxy settings is not covered.
- Other apps that honour the system proxy, such as Mail, also send allowlisted domains through the VPN. This is accepted.
- An app-scoped profile's SOCKS listener is loopback-only and needs no sign-in, as it is today for browser launches.

### Outstanding Questions

- Deferred to Planning: whether the per-profile domain list reuses the existing DNS-domain field or is a new field; the two mean different things today (names resolved through the VPN's DNS versus traffic routed through it).
- Deferred to Planning: the re-resolve interval for R10 and how a resolution failure is shown.
- Deferred to Planning: whether names under a listed domain are resolved through the VPN's own DNS when the profile has DNS servers.

### Sources / Research

- Current launcher and PAC: `internal/app/vpnbrowser.go`, `internal/vpn/browser/launch.go`, `src/components/BrowserMappings.tsx`.
- Helper operations today (routes, MTU, OpenVPN and OpenConnect start, tunnel stop; no proxy operation): `internal/netd/api.go`.
- Routing plan, preview and auto-apply that R9 extends: `internal/vpn/routing/planner.go`, `internal/app/routing.go`.
- Helper's per-profile browser SOCKS listener: `internal/vpn/browserport.go`.
- TunnelHub Chrome mappings: `internal/importer/tunnelhub/import.go`, `internal/app/vpnbrowser.go` `saveImportedBrowsers`.
- Prior VPN plan whose R33 and KTD11 this replaces: `docs/plans/2026-10-30-1022-feat-vpn-profiles-plan.md`.
