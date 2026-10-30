---
title: VPN Profiles - Plan
type: feat
date: 2026-10-30
topic: vpn-profiles
artifact_contract: ce-unified-plan/v1
product_contract_source: ce-brainstorm
execution: code
---

# VPN Profiles - Plan

## Goal Capsule

- **Objective:** A Hostbay user reaches hosts that sit behind one or more VPNs without leaving Hostbay: they import or create a VPN profile once, tag it with project names, and Hostbay (or an MCP agent, after approval) brings up the right VPN when a host or tag needs it, with several VPNs up at the same time and each destination routed through the right one.
- **Product authority:** the user pre-approved this plan's decisions and asked that safe defaults be chosen instead of questions (memory: autonomous Hostbay execution). Defaults chosen here are listed under Dependencies / Assumptions so a reviewer can overturn them. The user's TunnelHub app is the source of ideas, not code to port.
- **Open blockers:** none for planning. The privileged helper (R30) gates only the system-wide tier.

---

## Product Contract

### Summary

Hostbay gains VPN profiles: saved, synced, tagged definitions of a VPN that Hostbay connects itself or drives through the vendor's client.
Most protocols run inside Hostbay as app-scoped tunnels that carry only Hostbay's own traffic, so several VPNs, even with overlapping networks, stay up together without admin rights.
For VPNs that must own the system routing table, Hostbay brings TunnelHub's routing into the app: it detects every connected VPN, labels its tunnel interface, and routes each destination through its chosen VPN or the local network.

### Problem Frame

The hosts Hostbay manages for a project often sit behind that project's VPN, and different projects use different VPN products.
Today the user starts each VPN in its own vendor app, remembers which VPN a host needs, and fights the vendor clients for the routing table, because most of them install a full-tunnel route that swallows the other VPN's networks.
The user built TunnelHub to fix the routing half of this on macOS: it runs on top of already-connected VPN clients and rewrites routes per destination.
TunnelHub still leaves connecting to each vendor app, knows nothing about hosts, needs a hand-edited config, and runs only on macOS.
An MCP agent asked to run a command on a project's host has no way to bring the VPN up, so the request fails with a timeout that does not say why.

### What TunnelHub does, and what Hostbay takes from it

TunnelHub (`~/Documents/Santhosh/tunnel-hub`, Tauri 2, macOS only) never connects a VPN. It waits for vendor clients to connect, then edits the routing table.

| TunnelHub behavior | How it works there | In Hostbay |
|---|---|---|
| Detect VPNs | `scutil --nc list` for system VPNs, a running-process scan for daemon VPNs such as AnyConnect's `vpnagentd`, and leftover point-to-point `utun` interfaces for agent VPNs such as Harmony SASE. | Kept: external VPN detection (R22, R23). |
| Label tunnels | Maps each `utun` to a VPN by `scutil` interface name, then peer CIDR, then tunnel shape (point-to-point vs peer-less). Two VPNs of the same shape stay unlabeled rather than guessed. | Kept, including "never guess" (R24). |
| Routing modes | Full (install `0.0.0.0/1` and `128.0.0.0/1` on one VPN), Split (only listed networks through the VPN), Dual (each destination to its own VPN or `local`), Reset. | Kept for system-wide and external VPNs (R25, R26). |
| Coexistence fixes | Strips a VPN's half-internet routes in Dual mode and narrows a greedy route such as `10.0.0.0/8` to the subnets it needs, so a second VPN can connect. Sets tunnel MTU to 1280 to stop SSH stalls. | Kept (R27, R28). |
| Auto-apply | Re-runs the chosen mode when a VPN tunnel appears, and tells the user. | Kept, and re-applies when an agent re-pushes routes (R29). |
| Privileged helper | A root LaunchDaemon on a Unix socket runs only `route` and `ifconfig`, so the user approves once at install. | Kept as an idea, made safe (R30, R31). |
| Chrome profile routing | Launches a Chrome profile with `--proxy-server=socks5://…` bound to a VPN's SOCKS endpoint. | Kept, and Hostbay supplies the SOCKS endpoint itself for app-scoped VPNs (R32, R33). |
| Menu-bar control | Tray menu switches modes; window closes to the tray. | Kept as tray actions for profiles (R10). |

Two TunnelHub weaknesses Hostbay must not copy: its helper socket is world-writable (`0666`), so any local program can rewrite the routing table as root, and its route changes are best-effort with failures ignored, so the user cannot tell a partial apply from a full one.

### Where VPNs plug into Hostbay

Hostbay already sends most connections through one dial step, which makes an app-scoped VPN one more way to dial, beside direct, proxy and jump host.

- SSH first hops go through `sshx.DialTCP` (`internal/sshx/dial.go:43`). Telnet, raw TCP, VNC, SPICE, Kubernetes and database hosts go through `ConnService.dialHost` (`internal/app/conn.go:541`). A missing proxy is an error, never a direct dial (KTD22).
- Three paths do not use that step and need their own VPN wiring: RDP hands the helper the host's address when there is no jump host or proxy (`internal/app/rdp.go:72`); file hosts (FTP, S3, WebDAV, SMB) dial directly when no proxy is set (`internal/app/transfer.go:108`); mosh opens its own UDP socket (`internal/terminal/mosh.go:157`).
- Hostbay's SOCKS5 server for dynamic SSH tunnels (`internal/tunnel/socks.go`) is the piece R32 reuses to expose an app-scoped VPN to a browser profile.

### Supported VPN types

Labels are the UI names. The Fortinet-compatible type never shows a vendor name in the UI.

| UI name | Protocol | Tier (R2) | Notes |
|---|---|---|---|
| WireGuard | WireGuard | App-scoped | Runs inside Hostbay in userspace; no admin. System-wide on request. |
| Tailscale / Headscale | WireGuard mesh | App-scoped | Joins the tailnet inside Hostbay as its own node. |
| Cisco AnyConnect / Secure Client | AnyConnect | App-scoped | Through a bundled OpenConnect in its no-admin mode. |
| SSL VPN (PPP over TLS) | Fortinet-compatible | App-scoped | Through OpenConnect for password, certificate and one-time code. Browser sign-in (SAML) is system-wide only. |
| Palo Alto GlobalProtect | GlobalProtect | App-scoped | Through OpenConnect. |
| Pulse / Ivanti Connect Secure | Pulse, Network Connect | App-scoped | Through OpenConnect. |
| F5 BIG-IP SSL VPN | F5 | App-scoped | Through OpenConnect. |
| Array Networks SSL VPN | Array | App-scoped | Through OpenConnect. |
| ZeroTier | ZeroTier | App-scoped | Joins the network inside Hostbay. |
| OpenVPN | OpenVPN 2.x | System-wide | Needs a tunnel device, so admin rights through the helper. Linux can use OpenVPN 3 without admin. |
| IKEv2 / L2TP over IPsec | IPsec | System-wide | Through the operating system's own VPN service. |
| Perimeter 81 / Harmony SASE | Vendor | External | Detected and routed; connected in the vendor app. An admin-issued `.ovpn` can be imported as an OpenVPN profile instead. |
| Any other connected VPN | Vendor | External | Cisco Secure Client app, the SSL VPN vendor app, OpenVPN Connect, Tunnelblick, Cloudflare WARP, NetBird, Zscaler: detected and routed; launched or driven through their CLI where one exists. |

### Key Decisions

- **App-scoped first.** A VPN inside Hostbay carries only Hostbay's traffic, needs no admin rights, cannot break other networking, and lets VPNs with overlapping networks coexist. This is what makes "several VPNs at once" dependable. Governs R2, R12, R14.
- **TunnelHub's routing covers what app-scoped cannot.** VPNs that must own the system routing table, and vendor clients Hostbay cannot replace, are routed per destination the way TunnelHub does it, inside Hostbay and on every platform. Governs R22 to R29.
- **A VPN is a dial path, like a proxy.** A host names the VPN it is reached through, and nothing falls back to a direct dial when that VPN is down. Governs R15, R16.
- **The helper is narrow and authenticated.** One approval installs it; it accepts requests only from Hostbay and only route, interface and VPN-start operations Hostbay names. Governs R30, R31.
- **Tags are shared with hosts.** A project tag means the same thing on a host and on a VPN profile, so one tag finds a project's hosts and the VPN that reaches them. Governs R6, R7.
- **An agent never connects a VPN silently.** Bringing a VPN up changes what the computer can reach, so an MCP connect asks in the approval tray unless the user allowed that profile for that token. Governs R34.

### Requirements

**Profiles and tags**

- R1. A VPN profile stores a name, a type from the supported table, the gateway, the sign-in method, any certificates or keys, its route list, its DNS settings, and its tags.
- R2. Each profile runs in one tier: app-scoped (inside Hostbay, Hostbay traffic only), system-wide (the whole computer, through the helper), or external (a vendor client Hostbay detects and routes). A type that supports app-scoped defaults to it.
- R3. An external profile names the installed client; Hostbay launches it, connects and disconnects through its CLI where one exists, and otherwise shows the client's own state.
- R4. Profile secrets (passwords, private keys, pre-shared keys) are sealed like proxy passwords and bound to the profile's type, gateway and user, so editing the gateway makes the old secret unopenable.
- R5. Profiles sync across the user's computers like proxies do, including the hold that waits for this computer to confirm a synced change.
- R6. A profile carries any number of tags; tags are free text, matched without regard to case, and shared with host tags. The host editor gains the same tag editor.
- R7. The VPN list filters by tag and by text, and each tag shows how many hosts and VPN profiles carry it.

**Connecting, several at once**

- R8. The user connects and disconnects a profile from the VPN list, from a host's menu, from the command palette, and from the tray.
- R9. Each profile shows live state: connecting, signing in, connected (with assigned address, routes, interface and uptime), failed with a reason, disconnected, or dropped with a reason.
- R10. The tray lists profiles with their state and connects or disconnects one without opening the window.
- R11. Hostbay lists connected profiles in one place with which hosts and browser profiles are using each.
- R12. Any number of app-scoped profiles may be connected at the same time, including profiles whose route lists overlap.
- R13. A system-wide profile may connect while other system-wide or external VPNs are up only when the routing plan of R25 can place it; otherwise Hostbay names the conflicting profile and does not connect.
- R14. App-scoped profiles never change the system routing table, DNS or interfaces.
- R15. A host may name one VPN profile it is reached through. Connecting to that host brings the profile up first when it is down, then dials through it, for every host kind including RDP, file hosts and mosh.
- R16. A host whose VPN profile cannot connect or was deleted fails with that reason; it never dials directly instead.
- R17. A host with no named profile, whose address falls inside exactly one connected app-scoped profile's routes, dials through that profile. When two connected profiles both match, the host does not connect and Hostbay asks the user to name one on the host.
- R18. A dropped VPN is shown as dropped and is reconnected only if the profile's auto-reconnect setting is on; sessions that used it end with the reason.
- R19. Quitting Hostbay with VPNs connected asks first, as it does for running tunnels, and disconnects the profiles Hostbay connected.

**Sign-in**

- R20. Profiles support each sign-in their protocol offers: username and password, client certificate, one-time code, and browser sign-in (SAML) in the user's default browser.
- R21. A gateway certificate the system does not trust is shown with its fingerprint and pinned only after the user accepts it; a changed certificate refuses the connection, as Hostbay does for RDP and TLS hosts.

**Routing system-wide and external VPNs (from TunnelHub)**

- R22. Hostbay detects VPNs already connected on this computer, whether registered with the operating system, run by a vendor daemon, or present only as a tunnel interface, and offers each as an external profile.
- R23. A detection refresh adds newly installed VPNs and drops removed ones, keeping any profile that hosts or routes still use.
- R24. Hostbay labels each tunnel interface with its VPN by the operating system's own name for it, then by the profile's address range, then by tunnel shape; when two VPNs cannot be told apart it leaves both unlabeled and asks the user to pin one, and never routes traffic on a guess.
- R25. A profile's routes apply in one of four modes: full tunnel, split tunnel, per-destination (each network to its own VPN or to the local network), and reset (remove every route Hostbay added).
- R26. Before applying, Hostbay shows the planned route changes and reports afterwards which changes succeeded and which failed.
- R27. In per-destination mode Hostbay removes half-internet routes that a VPN's full-tunnel setting installed, and offers to narrow a greedy route such as `10.0.0.0/8` to the subnets that VPN needs so another VPN can connect.
- R28. Each profile sets its tunnel MTU, defaulting to 1280.
- R29. When a VPN connects, or a vendor agent re-pushes its routes, Hostbay re-applies the chosen mode automatically if the profile's auto-apply setting is on, and notifies the user.
- R30. System-wide routing and system-wide profiles use a privileged helper installed once after the user agrees in a native dialog that says what it can do; without it, they show why they cannot run.
- R31. The helper accepts requests only from the signed Hostbay app, only for route, interface MTU and VPN start and stop operations, and only for addresses and interfaces belonging to a Hostbay profile.

**Browser and app routing (from TunnelHub)**

- R32. An app-scoped profile can expose a local SOCKS5 endpoint on `127.0.0.1` that carries traffic through that VPN.
- R33. The user maps a Chrome or Chromium-family browser profile to a VPN profile and launches it from Hostbay pointed at that VPN's SOCKS endpoint; a profile with no SOCKS endpoint says so instead of launching unrouted.

**MCP agent**

- R34. MCP gains tools to list VPN profiles (filtered by tag), report their state, connect one by profile or by tag, and disconnect one the agent connected. Connect and disconnect go through the approval tray unless the user allowed that profile for that token.
- R35. Connecting by tag picks the single profile carrying that tag; when several carry it, the tool returns the candidates instead of choosing.
- R36. `list_connections` reports each host's tags and the VPN profile it is reached through.
- R37. Running a command on a host whose VPN is down returns an error that names the VPN profile, instead of a timeout.

**Import and routing**

- R38. Import reads VPN configuration files: OpenVPN `.ovpn` (with inline certificates), WireGuard `.conf`, Cisco AnyConnect profile `.xml`, Apple `.mobileconfig`, NetworkManager `.nmconnection`, Windows phonebook `.pbk`, Tunnelblick `.tblk` folders, and Viscosity `.visc` bundles.
- R39. Import also scans known locations of installed clients on this computer (Tunnelblick, Viscosity, OpenVPN, AnyConnect profiles, NetworkManager, Windows phonebooks) and lists what it finds for the user to pick.
- R40. Import keeps the file's routes and DNS settings: OpenVPN `route` lines and `redirect-gateway`, WireGuard `AllowedIPs` and `DNS`, AnyConnect split-include and split-exclude lists, NetworkManager route and DNS entries.
- R41. Import also reads a TunnelHub `vpn-config.json`, turning its VPNs into external profiles, its `routes` into per-destination routes and its `chrome` entries into browser mappings.
- R42. Every imported profile shows its route list in an editor before saving, where the user can switch between full and split tunnel and add or remove networks and DNS domains.
- R43. Import never runs or keeps scripts from a config file (`up`, `down`, `script-security` and similar); it lists each ignored directive in the import report.
- R44. Import never stores a secret that was not in the file or that the user did not type; a profile imported without its password asks for it on first connect.

### Key Flows

- F1. Connect a host behind a VPN
  - **Trigger:** the user opens a host that names VPN profile P.
  - **Steps:** Hostbay sees P is down, connects P (asking for a one-time code or opening browser sign-in if needed), then dials the host through P. The tab shows "via P".
  - **Outcome:** the session runs; P stays up until the user disconnects it or quits.
  - **Covered by:** R8, R9, R15, R16, R20
- F2. Several projects at once, app-scoped
  - **Trigger:** the user works on projects A and B, whose VPNs both route `10.0.0.0/8`.
  - **Steps:** the user connects both app-scoped profiles. Host A1 names profile A and host B1 names profile B; both connect.
  - **Outcome:** each host's traffic goes through its own VPN; nothing else on the computer changes.
  - **Covered by:** R12, R14, R15, R17
- F3. Two vendor VPNs, per-destination
  - **Trigger:** Harmony SASE and the SSL VPN vendor client are both connected, and Harmony pushed `10.0.0.0/8`.
  - **Steps:** Hostbay detects and labels both tunnels, shows the plan (strip Harmony's halves, narrow `10.0.0.0/8` to its subnets, route `10.212.0.0/16` to the SSL VPN), the user applies, and Hostbay reports each change.
  - **Outcome:** both VPNs carry their own networks; the plan re-applies if Harmony re-pushes its routes.
  - **Covered by:** R22, R24, R25, R26, R27, R29
- F4. Agent reaches a project host
  - **Trigger:** an MCP agent is asked to check disk space on hosts tagged `atlas`.
  - **Steps:** the agent calls `list_connections`, picks hosts tagged `atlas`, sees they are reached through "Atlas VPN", connects by tag `atlas`, the user approves in the tray, then the agent runs its command.
  - **Outcome:** the command runs; the approval card named the profile and the token.
  - **Covered by:** R34, R35, R36
- F5. Import with routing
  - **Trigger:** the user drops an `.ovpn` file onto the VPN list.
  - **Steps:** Hostbay parses it, shows the gateway, sign-in method, routes and DNS, lists ignored script directives, and asks for tags.
  - **Outcome:** a saved profile with its split-tunnel routes and no secret the file lacked.
  - **Covered by:** R38, R40, R42, R43, R44

### Acceptance Examples

- AE1. **Covers R16.** **Given** host H names profile P and P's gateway is unreachable, **when** the user opens H, **then** the tab fails with "VPN P could not connect: <reason>" and nothing is sent to H's address outside P.
- AE2. **Covers R17.** **Given** app-scoped profiles A and B are connected and both route `10.1.0.0/16`, **when** the user opens host `10.1.2.3` that names no profile, **then** the connect fails and asks the user to choose A or B on the host.
- AE3. **Covers R13, R14.** **Given** an app-scoped WireGuard profile is connected, **when** the user connects a system-wide OpenVPN profile with `redirect-gateway`, **then** it connects, because the app-scoped profile uses no system routes. **Given** instead a second system-wide full-tunnel profile is connected, **then** the new one does not connect and names the other.
- AE4. **Covers R24.** **Given** two point-to-point VPNs are connected and neither has an address range or interface pin, **when** Hostbay refreshes, **then** both stay unlabeled, per-destination mode refuses routes for them, and the user is asked to pin one.
- AE5. **Covers R26, R31.** **Given** the helper is installed, **when** a non-Hostbay program sends it a route request, **then** the helper refuses it; **when** Hostbay applies a plan and one route fails, **then** the report names the failed route and the profile shows "partly applied".
- AE6. **Covers R33.** **Given** browser profile "Work" maps to an external profile with no SOCKS endpoint, **when** the user launches it, **then** Hostbay does not launch it and says the VPN has no SOCKS endpoint.
- AE7. **Covers R35.** **Given** two profiles tagged `atlas`, **when** an agent connects by tag `atlas`, **then** nothing connects and the tool returns both profile names.
- AE8. **Covers R21.** **Given** a pinned gateway certificate, **when** the gateway presents a different certificate, **then** the connection is refused and the user sees both fingerprints.
- AE9. **Covers R43.** **Given** an `.ovpn` with `up /tmp/x.sh` and `script-security 2`, **when** it is imported, **then** the profile saves without them and the report lists both lines as ignored.

### Scope Boundaries

- **Deferred for later:** a kill switch; VPN traffic graphs and history; routing programs other than Hostbay and Chromium-family browsers; IPv6 per-destination routes.
- **Outside this product's identity:** acting as a VPN server or gateway; replacing vendor clients whose sign-in works only in the vendor's app; running VPN config scripts.

### Dependencies / Assumptions

- Assumption: a bundled OpenConnect runs without admin rights on macOS, Linux and Windows by handing traffic to Hostbay instead of a tunnel device (OpenConnect's `--script-tun`). Windows support for that mode is unconfirmed; a protocol that cannot run app-scoped on a platform drops to system-wide there.
- Assumption: OpenConnect's Fortinet-compatible protocol has no browser sign-in, so an SSL VPN that requires SAML runs system-wide through a separate client or stays external.
- Assumption: Hostbay has no browser sign-in flow today (the Claude Code provider asks the user to run `claude auth login`), so SAML sign-in is new work.
- Assumption: Harmony SASE offers no third-party protocol access except an admin-issued `.ovpn`, so it stays external unless that file is imported.
- Assumption: Hostbay's existing host `tags` field, which the UI does not yet edit, is the shared tag store.

### Outstanding Questions

**Deferred to Planning**

- Which OpenConnect build ships per platform, and the LGPL obligations of bundling it.
- Whether WireGuard, Tailscale and ZeroTier run in-process or in a helper process, given memory use and crash isolation.
- How the helper is installed, updated and authenticated on each platform (launchd daemon, Windows service, Linux capability or polkit), and the exact request schema R31 allows.
- How R24's labeling works on Windows and Linux, where interfaces carry names and descriptions that macOS `utun` lacks.
- The exact file locations R39 scans on each platform.

### Sources / Research

- TunnelHub, `~/Documents/Santhosh/tunnel-hub` (machine-local): `README.md`, `CLAUDE.md`, `src-tauri/src/routing.rs` (detection, labeling, modes, greedy-route narrowing), `src-tauri/src/helper.rs` (root helper), `src-tauri/src/lib.rs` (tray, auto-apply, helper install).
- `internal/sshx/dial.go`, `internal/sshx/proxy.go`, `internal/app/conn.go`: the shared dial step and the no-direct-fallback rule.
- `internal/app/rdp.go`, `internal/app/transfer.go`, `internal/terminal/mosh.go`: the dial paths that bypass it.
- `internal/store/proxies.go`, `internal/sync/holds.go`: the sealed, synced, held pattern R4 and R5 follow.
- `internal/mcp/tools.go`, `internal/agentexec/gate.go`: MCP tools and the approval gate R34 extends.
- `internal/tunnel/socks.go`: the SOCKS5 server R32 reuses.
- OpenConnect non-root mode: https://www.infradead.org/openconnect/nonroot.html; protocol changelog: https://www.infradead.org/openconnect/changelog.html
- WireGuard userspace netstack: https://git.zx2c4.com/wireguard-go/tree/tun/netstack
- Harmony SASE OpenVPN tunnel: https://support.perimeter81.com/docs/openvpn-tunnel.md
- `docs/plans/2026-10-22-1100-feat-hostbay-go-desktop-port-plan.md`: KTD5 (leases), KTD18 (certificate pinning), KTD22 (no direct fallback), R33 (loopback by default).
