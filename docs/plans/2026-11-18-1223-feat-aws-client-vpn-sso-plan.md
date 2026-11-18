---
title: AWS Client VPN with SSO sign-in - Plan
type: feat
date: 2026-11-18
topic: aws-client-vpn-sso
artifact_contract: ce-unified-plan/v1
product_contract_source: ce-brainstorm
execution: code
---

# AWS Client VPN with SSO sign-in - Plan

## Goal Capsule

- **Objective:** staff connect to an AWS Client VPN endpoint that uses SSO (SAML) sign-in from Hostbay on macOS, Windows and Linux, signing in through their own browser, with no AWS VPN Client installed.
- **Means:** an AWS SSO profile kind that runs system-wide on a Hostbay-built patched OpenVPN, used for AWS SSO profiles only.
- **Authority:** this plan's Requirements win on behaviour. The 2026-10-30 VPN profiles plan (`docs/plans/2026-10-30-1022-feat-vpn-profiles-plan.md`) governs everything this plan does not change. For AWS SSO profiles only, this plan overrides the 2026-11-15 plan's boundary that browser sign-in stays app-scoped (`docs/plans/2026-11-15-1247-feat-macos-system-wide-openconnect-plan.md`).
- **Open blockers:** none for planning. The first live test waits for an AWS `.ovpn` from the user.

---

## Product Contract

### Summary

Hostbay recognises an AWS Client VPN `.ovpn` that uses SSO and connects it natively. Connecting opens the user's browser at the SSO page, Hostbay catches the sign-in on `127.0.0.1:35001`, and the tunnel comes up system-wide on a patched OpenVPN that Hostbay builds and ships for macOS, Windows and Linux. Other OpenVPN profiles are unchanged.

### Problem Frame

Some corporate networks sit behind AWS Client VPN endpoints that sign users in through SSO. The only supported way onto them is AWS's own VPN Client app, which the user does not want to install beside Hostbay.

Hostbay cannot connect these endpoints today. Its import drops `auth-federate` and refuses `remote-random-hostname` at connect (`internal/importer/vpnconf/ovpn.go`, `internal/vpn/openvpn/config.go`). Its management code answers one password request and ends the run on a second (`internal/vpn/openvpn/manage.go`). Nothing handles the CRV1 challenge AWS uses.

Stock OpenVPN cannot complete the sign-in either. The SAML response is about 6 to 12 KB. OpenVPN 2.7.8 sends the password through a 2048-byte TLS control buffer (`common.h:70`) and caps it at 4096 bytes (`misc.h:64-67`), so the second connect fails at about 1.5 KB. AWS states that OpenVPN-based clients cannot connect to SAML endpoints. Every open-source client that does it ships a patched OpenVPN.

### Key Decisions

- **Ship a patched OpenVPN instead of relying on the AWS VPN Client.** (session-settled: user-directed — chosen over keeping the AWS VPN Client for SSO endpoints: the user does not want to install it.) Governs R4, R13.
- **The patched OpenVPN serves AWS SSO profiles only.** (session-settled: user-directed — chosen over one patched OpenVPN for every OpenVPN profile, and over an embedded sign-in window: a patch bug can then break only AWS SSO.) Governs R12.
- **Sign-in runs in the user's default browser.** It reuses the IdP's existing session, so a reconnect is usually one click. The embedded sign-in window exists only on macOS. Governs R4.
- **All three platforms are built together, with no macOS-first spike.** (session-settled: user-directed — chosen over proving the sign-in on macOS before building Windows and Linux.) Governs R11. Conflict: Windows and Linux bundle no OpenVPN today, and the first live test against a real endpoint comes only after all three are built.
- **A dropped tunnel signs in again by itself.** (session-settled: user-directed — chosen over a Reconnect button the user clicks, and over staying disconnected.) Governs R9.
- **Guard the automatic sign-in against a flood of browser windows.** A flaky network would otherwise open a window on every drop. Governs R10.
- **Browser sign-in runs on a system-wide profile.** OpenVPN runs only system-wide through `hostbay-netd`, so AWS SSO cannot be app-scoped. Governs R2, R4.

### Requirements

**Profile and import**

- R1. Importing an AWS Client VPN `.ovpn` that carries `auth-federate` creates an AWS SSO profile without the user choosing a type.
- R2. An AWS SSO profile runs system-wide through the helper, and its listed sites and networks route through it as they do for other system-wide profiles.
- R3. Both connects of one sign-in reach the same AWS server, even when the endpoint uses `remote-random-hostname`.

**Sign-in**

- R4. Connecting opens the user's default browser at the endpoint's SSO page, and the tunnel comes up once the IdP returns the user, with no AWS VPN Client installed.
- R5. Hostbay receives the sign-in on `127.0.0.1:35001` only, accepts one response per sign-in, and stops listening when that sign-in ends.
- R6. The SAML response is treated as a credential: never logged, saved, or shown in an error.
- R7. When port 35001 is already in use, the connect fails with a message naming the app that holds it where the operating system can tell.
- R8. A sign-in nobody finishes times out and leaves the profile disconnected with that reason shown.

**Reconnect**

- R9. When an AWS SSO tunnel drops or the network changes, Hostbay reconnects and opens the browser sign-in without the user clicking.
- R10. Hostbay never opens a second sign-in while one is waiting, and after 3 failed sign-ins in a row it stops and shows a Reconnect button.

**Patched OpenVPN and platforms**

- R11. AWS SSO profiles connect on macOS, Windows and Linux.
- R12. Only AWS SSO profiles use the patched OpenVPN; every other OpenVPN profile keeps the binary it uses today.
- R13. Hostbay builds the patched OpenVPN from upstream OpenVPN 2.7.8 plus its own patch on all three platforms, and the helper installs and runs it as it does the current binary.
- R14. Every release that ships the patched OpenVPN publishes its source, the patch and the build scripts, and NOTICE describes it as modified.

### Key Flows

- F1. First connect
  - **Trigger:** the user clicks Connect on an AWS SSO profile.
  - **Steps:** Hostbay picks one AWS server for this sign-in; the first connect returns the SSO challenge; Hostbay starts listening on `127.0.0.1:35001` and opens the browser; the user signs in; the response arrives; Hostbay connects again to the same server with it; the tunnel comes up and its routes apply.
  - **Covered by:** R2 to R6, R8.
- F2. Drop and sign in again
  - **Trigger:** the tunnel drops or the network changes.
  - **Steps:** Hostbay marks the profile reconnecting and runs F1 without a click, unless a sign-in is already waiting or 3 sign-ins in a row have failed.
  - **Covered by:** R9, R10.

### Acceptance Examples

- AE1. **Covers R7.** Given the AWS VPN Client is running and holds port 35001, when the user connects an AWS SSO profile, then the connect fails with a message naming the AWS VPN Client, and no browser opens.
- AE2. **Covers R10.** Given a sign-in window is open and waiting, when the network changes again, then no second browser window opens.
- AE3. **Covers R10.** Given 3 sign-ins in a row have failed or timed out, when the tunnel drops again, then Hostbay opens no browser and shows the profile with a Reconnect button.
- AE4. **Covers R12.** Given an ordinary OpenVPN profile without `auth-federate`, when it connects, then it runs on the same OpenVPN binary as before this change.
- AE5. **Covers R8.** Given the browser opened and the user closed it without signing in, when the sign-in times out, then the profile is disconnected with a reason saying the sign-in was not completed.
- AE6. **Covers R6.** Given a sign-in fails at the second connect, then the error and the logs carry no part of the SAML response.

### Scope Boundaries

- AWS endpoints that use certificate-only sign-in. They may already work as plain OpenVPN profiles; this is unverified.
- Downloading the `.ovpn` from AWS's self-service portal.
- Detecting a running AWS VPN Client app as an external VPN.
- An app-scoped AWS SSO profile.
- An embedded sign-in window on any platform.
- Moving ordinary OpenVPN profiles onto the patched OpenVPN.

### Dependencies / Assumptions

- No AWS `.ovpn` is available yet, so nothing has been tested against a live endpoint.
- AWS supports only its own client for SAML sign-in and may change server behaviour without notice.
- The endpoint's IdP application sends the sign-in to `http://127.0.0.1:35001`, AWS's default.
- Hostbay releases are public GitHub Releases (`docs/release-signing.md`), so shipping a modified OpenVPN is GPLv2 distribution. NOTICE already covers the unmodified OpenVPN with a written source offer.
- On Windows and Linux, nothing supplies an OpenVPN binary today; the helper looks for one in its client directory (`internal/netd/install.go`).

### Outstanding Questions

**Deferred to Planning**

- Does the patch need the key-method-2 length change (u16 to u32) beyond raising `USER_PASS_LEN` and `TLS_CHANNEL_BUF_SIZE`? Research found it probably does not for an 11 KB response, but did not confirm this against current AWS servers.
- Does Homebrew-style OpenSSL 3 need the self-signed-certificate patch the aws-vpn-client project ships, for AWS's certificate chain?
- How the patched OpenVPN is built, signed and bundled on each platform, and how the helper verifies it before running it as root.
- How the second sign-in reaches OpenVPN: two process runs, or one run with the challenge answered over the management interface (multi-line base64 input, `version 6`, OpenVPN 2.7.2 and later).
- The sign-in timeout length.
- What counts as a network change for a system-wide OpenVPN profile, and whether Hostbay already detects it.

### Sources / Research

- OpenVPN 2.7.8 limits: `src/openvpn/misc.h:64-67` (`USER_PASS_LEN`), `src/openvpn/common.h:70` (`TLS_CHANNEL_BUF_SIZE`), `src/openvpn/ssl.c:2188` (auth write failure), `src/openvpn/manage.c:1041-1121` (multi-line password input), `src/openvpn/options.c:5262` (unknown option is fatal).
- AWS: SAML endpoints need the AWS-provided client, https://docs.aws.amazon.com/vpn/latest/clientvpn-user/connect.html and https://docs.aws.amazon.com/vpn/latest/clientvpn-admin/federated-authentication.html.
- Protocol walkthrough and SAMLResponse size (about 11 KB): https://smallhacks.wordpress.com/2020/07/08/aws-client-vpn-internals/.
- Reference patches for 2.4.9, 2.5.1 and 2.6.12: https://github.com/aws-vpn-client/aws-vpn-client.
- Hostbay today: `internal/vpn/openvpn/manage.go` (one password answer, SCRV1 only), `internal/importer/vpnconf/ovpn.go` (allowlist, first `remote` only), `internal/vpn/openvpn/config.go` (directive allowlist), `build/darwin/Taskfile.yml` (macOS bundles Homebrew's OpenVPN), `internal/app/routing.go` (routes and sites for system-wide profiles), `internal/store/vpns.go` (auto-apply default), `NOTICE` (OpenVPN entry).
