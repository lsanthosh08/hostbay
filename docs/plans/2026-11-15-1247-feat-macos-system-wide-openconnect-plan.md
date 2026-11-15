---
title: System-wide OpenConnect on macOS - Plan
type: feat
date: 2026-11-15
artifact_contract: ce-unified-plan/v1
product_contract_source: ce-plan-bootstrap
execution: code
---

# System-wide OpenConnect on macOS - Plan

## Goal Capsule

- **Objective:** with an OpenConnect VPN profile such as Corp Internal connected in Hostbay, any app on the Mac, including DBeaver, reaches the hosts behind that VPN at their real addresses, the way it does with FortiClient.
- **Means:** an opt-in system-wide tier for OpenConnect profiles on macOS, run by `hostbay-netd` on a utun, with the profile's routes applied by Hostbay's routing engine (KTD1, KTD2).
- **Authority:** this plan's Requirements win on behaviour; its KTDs win on mechanism; the 2026-10-30 VPN profiles plan (`docs/plans/2026-10-30-1022-feat-vpn-profiles-plan.md`) governs everything this plan does not change.
- **Stop conditions:** stop and report if a change would let a gateway-supplied value reach a shell, a root-run path the user can write, or an interface the helper did not choose (R5). Stop if the root openconnect turns out to load user-writable config or modules and no environment override closes it (KTD8).
- **Execution profile:** one PR, committed per unit on `main`, never pushed (user's standing rule).
- **Finishing:** `ce-work` builds and verifies; the user reinstalls the helper (one admin prompt) and runs the DBeaver check.

---

## Product Contract

### Summary

OpenConnect profiles (Fortinet SSL VPN and the other OpenConnect types) gain a system-wide option on macOS. When chosen, Hostbay's privileged helper runs openconnect on a utun and Hostbay routes the profile's networks through it, so every app on the Mac uses the VPN for those networks. App-scoped stays the default.

### Problem Frame

Corp Internal is app-scoped: only Hostbay's own traffic goes through it. Inside Hostbay the internal SQL Server works; DBeaver times out connecting to `sql.corp.example:1433` (public DNS answer `172.16.58.221`), because nothing outside Hostbay routes that address into the VPN. The same profile in FortiClient works for DBeaver directly. The VPN port forward shipped earlier this session makes DBeaver work only if it points at `127.0.0.1`, which the user rejected.

### Key Decisions

- **System-wide tier for OpenConnect on macOS, not a local port forward.** The user wants FortiClient behaviour: apps use the real host name and port. (session-settled: user-directed — chosen over a Hostbay local port forward through the app-scoped VPN: the user does not want to repoint DBeaver at 127.0.0.1.) Governs R1, R2.

### Requirements

**Choosing the tier**
- R1. On macOS, an OpenConnect profile can be set to system-wide in the profile editor; new and existing profiles stay app-scoped until the user changes them.
- R2. Choosing system-wide for an OpenConnect profile turns on applying its routes on connect, which the user can still turn off.

**Connecting**
- R3. Connecting a system-wide OpenConnect profile on macOS brings up a utun carrying the VPN's address, routes each of the profile's networks through it, and any app on the Mac then reaches hosts in those networks at their real addresses.
- R4. Disconnecting, or the VPN dropping, removes the utun and every route Hostbay added for it.
- R5. No value the VPN gateway sends reaches a shell, and the helper only configures an interface it chose itself.

**Compatibility**
- R6. Hosts and VPN port forwards bound to the profile keep connecting when the profile is system-wide.
- R7. A helper installed before this change gets a reinstall offer instead of failing with an "only on Windows" error.
- R8. On Linux, a system-wide OpenConnect profile still fails, with a message that names macOS and Windows as the supported systems.

### Acceptance Examples

- AE1. Covers R2, R3. Given Corp Internal set to system-wide with routes `10.62.0.0/16` and `172.16.58.0/24`, when it connects, `route -n get 172.16.58.221` names a utun and DBeaver connects to `sql.corp.example:1433` unchanged.
- AE2. Covers R4. Given that connection, when the user disconnects, the utun is gone and `route -n get 172.16.58.221` names the default gateway again.
- AE3. Covers R5. Given a gateway that sends `TUNDEV=utun3; rm -rf /` or a `TUNDEV` other than the utun the helper chose, the script configures nothing and the connection fails.

### Scope Boundaries

- Linux system-wide OpenConnect stays out of scope (R8 only fixes its message).
- VPN DNS servers and search domains are not pushed into macOS; the target RDS name resolves through public DNS.
- Only password sign-in runs system-wide. Certificate, browser (SAML) and one-time-code sign-in stay app-scoped: the editor does not offer system-wide for them, and the app refuses them at connect with a clear message.
- Not built: an automatic tier switch from FortiClient detection. Nothing asked for it.
- Not built: carrying the saved password across a tier change. The secret is sealed to the tier (`internal/store/vpns.go` `SecretAAD`), the editor already warns that the password is asked again, and unsealing across tiers would weaken that binding.

---

## Planning Contract

### Key Technical Decisions

- KTD1. **The helper runs openconnect as root on a utun it picks.** The helper chooses a free `utunN`, passes `--interface=utunN` and its own `oc-script` with that name as its argument, and reports that name as the tunnel's interface, mirroring the Windows adapter shape in `internal/netd/tunnel/openconnect.go`. Picking the name up front keeps "the helper only configures an interface it chose" checkable (R5). Rejected: letting openconnect allocate and having the script report `TUNDEV` back, which adds a report channel a gateway banner could spoof. A race with another utun creator makes openconnect fail with "Failed to connect utun unit" and Hostbay's reconnect retries.
- KTD2. **Opt-in tier, app-scoped default.** `vpnTier` stays as is: a stored `system` tier already reaches `launchSystem`. The change is the helper's darwin branch plus an editor control. Rejected: forcing OpenConnect system-wide on macOS as Windows does, which would need admin rights for every user and change existing profiles.
- KTD3. **The darwin `oc-script` trusts nothing from the environment beyond addresses.** On `reason=connect` only, it requires `TUNDEV` to equal its argument, parses `INTERNAL_IP4_ADDRESS`/`INTERNAL_IP4_NETMASKLEN` and `INTERNAL_IP6_ADDRESS`/`INTERNAL_IP6_NETMASK` with `netip`, and calls the existing darwin `setAddrs` (exec of `/sbin/ifconfig`, no shell). Every other reason and variable is ignored; MTU stays with the routing engine (`KindMTU`). Governs R5.
- KTD4. **The editor turns on auto-apply when it switches an OpenConnect profile to system-wide.** Without it `AutoApplies()` is false for system profiles and the profile shows Connected with nothing routed. Rejected: changing `AutoApplies()` to default true for every system profile, which would silently change existing OpenVPN and WireGuard system profiles. Governs R2.
- KTD5. **System OpenConnect gets the gateway pin system WireGuard has.** Extend the endpoint-pin condition in `internal/app/routing.go` `plan()` to system-tier OpenConnect, so a Full-mode or overlapping route cannot loop openconnect's own TLS into its utun. One condition; also closes the same gap on Windows.
- KTD6. **netd protocol version 6.** What the helper accepts changes; the bump makes an old helper produce the existing reinstall offer (R7). The build-hash staleness check alone would miss a development run.
- KTD7. **The script is invoked shell-quoted with fixed, helper-owned values only.** `'<helper dir>/hostbay-netd' oc-script utunN`, built with the existing `shellQuote` in `internal/vpn/openconnect/launch.go`; openconnect runs `--script` through `/bin/sh -c` on Unix. Nothing from the profile enters this string. Governs R5.
- KTD8. **The root openconnect never trusts user-writable files.** The bundled GnuTLS, p11-kit and stoken carry compiled-in Homebrew paths (`/opt/homebrew/etc/gnutls/cert.pem`, `/opt/homebrew/etc/gnutls/config`, the p11-kit module and config dirs) that a non-admin user can write. On darwin system-wide runs:
  1. `clientEnv` sets `GNUTLS_SYSTEM_PRIORITY_FILE` to an empty root-owned file the helper writes in its run dir, as it does for OpenSSL.
  2. A profile with no pinned certificate gets `--no-system-trust --cafile=/etc/ssl/cert.pem` (macOS's root-owned bundle) from the helper; a pinned profile already turns system trust off.
  3. During the live check, `fs_usage` on the openconnect pid, for a pinned and an unpinned profile, shows no open or failed open of any `/opt/homebrew` path. A p11-kit module load has no environment override, so seeing one triggers the stop condition.
  Governs R5.

### High-Level Technical Design

```mermaid
sequenceDiagram
  participant UI as Hostbay app
  participant N as hostbay-netd (root)
  participant OC as openconnect (root)
  participant S as oc-script (root)
  participant K as macOS kernel
  UI->>N: OpenConnectStart (type, config, password)
  N->>N: pick free utunN, CheckClient(openconnect)
  N->>OC: --interface=utunN --script='.../hostbay-netd' oc-script utunN, password on stdin
  OC->>K: create utunN
  OC->>S: reason=connect, TUNDEV, INTERNAL_IP4_*
  S->>S: TUNDEV == utunN? parse addresses
  S->>K: ifconfig utunN inet addr addr alias; up
  OC-->>N: stdout "Configured as addr, ..."
  N-->>UI: TunnelUp{Iface: utunN, Address}
  UI->>N: route add 10.62.0.0/16, 172.16.58.0/24 via utunN; pin gateway via default
  Note over UI,K: Disconnect: SIGTERM openconnect, utun and its routes vanish, Hostbay deletes its journaled pin
```

### Assumptions

- openconnect prints its `Configured as` line after the connect script has run, so "up" means the utun has its address. If not, routes added a moment early still bind to the utun by name.
- The single-architecture Homebrew openconnect the bundle ships covers the machines this runs on (arm64), as it already does for app-scoped profiles.

---

## Implementation Units

### U1. Helper runs OpenConnect system-wide on macOS

- **Goal:** `hostbay-netd` starts, reports and stops a system-wide OpenConnect tunnel on a utun on macOS.
- **Requirements:** R3, R4, R5, R7, R8 (KTD1, KTD3, KTD6, KTD7, KTD8).
- **Dependencies:** none.
- **Files:**
  - `internal/netd/tunnel/openconnect.go` (split the Windows adapter path from a darwin utun path)
  - `internal/netd/tunnel/system_darwin.go` (free-utun picker, darwin `Script`)
  - `internal/netd/tunnel/system_other.go`, `internal/netd/tunnel/system_linux.go` if needed (Linux message, R8)
  - `internal/vpn/openconnect/launch.go` (only if `SystemArgs` needs the script quoted differently on Unix)
  - `cmd/hostbay-netd/main.go` (pass the argument to `oc-script`, usage text)
  - `internal/netd/api.go` (`Version` 6, comments)
  - `internal/netd/tunnel/tunnel.go` comments
  - Tests: `internal/netd/tunnel/tunnel_test.go`, a new `internal/netd/tunnel/script_darwin_test.go` or equivalent, `internal/netd/server_test.go` / `api_test.go` version literals
- **Approach:**
  1. Keep the Windows branch byte-for-byte in behaviour; move the OS-specific parts (interface name, adapter, script path) behind per-OS helpers.
  2. darwin: pick the lowest `utunN` not present in `net.Interfaces()` (start above the kernel's usual low units), pass it to `SystemArgs`, build the script string per KTD7, report `Iface: utunN` on the `Configured as` line.
  3. darwin `Script(args)`: per KTD3, a pure parse function returning the interface and prefixes, then `setAddrs(nil, …)`.
  4. darwin trust hardening per KTD8 (priority-file env, CA flags for unpinned profiles).
  5. Linux keeps refusing, with the R8 message.
- **Patterns to follow:** the Windows `Script` in `internal/netd/tunnel/system_windows.go`; `setAddrs` in `system_darwin.go`; `writePrivate`/`clientEnv` in `internal/netd/tunnel/`.
- **Execution note:** write the script parse function test-first; it is the trust boundary.
- **Test scenarios:**
  - darwin start: args carry `--interface=utunN`, `--script='<dir>/hostbay-netd' oc-script utunN`, the password goes on stdin, and a `Configured as 10.0.0.5,` line yields a `TunnelUp` with `Iface: utunN` and address `10.0.0.5`.
  - The Windows start test keeps its current assertions, run only on Windows (or with the adapter faked, under a name that says so).
  - Script parse: `reason=connect`, `TUNDEV=utun9`, arg `utun9`, `INTERNAL_IP4_ADDRESS=10.62.4.7`, `INTERNAL_IP4_NETMASKLEN=32` returns `utun9` and `10.62.4.7/32`.
  - Script parse: `TUNDEV` differs from the argument returns an error and configures nothing. Covers AE3.
  - Script parse: `TUNDEV=utun3; rm -rf /`, or an argument not matching `utun[0-9]+`, returns an error. Covers AE3.
  - Script parse: an unparsable address returns an error; a missing netmask length falls back to /32 as on Windows.
  - Script parse: `reason=disconnect` or `reconnect` does nothing and succeeds.
  - Utun picker: with `utun0`–`utun5` present, it returns a name not in that set.
  - darwin start for a profile with no pinned certificate passes `--no-system-trust --cafile=/etc/ssl/cert.pem`; a pinned profile passes neither; the child environment carries `GNUTLS_SYSTEM_PRIORITY_FILE` pointing into the helper's run dir.
  - An old-version hello is refused with the reinstall code at version 6.
- **Verification:** netd tests pass with `-race` on darwin; the Linux build compiles; the live trace meets KTD8 step 3.

### U2. App routes and dials through a system-wide OpenConnect profile on macOS

- **Goal:** a stored system-tier OpenConnect profile on darwin connects through the helper, gets its routes and gateway pin, and keeps serving hosts and VPN forwards.
- **Requirements:** R3, R4, R6 (KTD2, KTD5).
- **Dependencies:** U1 for the live check only; tests use the fake helper.
- **Files:**
  - `internal/app/routing.go` (endpoint-pin condition, KTD5)
  - `internal/app/vpnsystem.go` (sign-in refusals, comments describing which OS runs what)
  - Tests: `internal/app/vpnsystem_test.go`
- **Approach:**
  1. Extend the pin condition next to system WireGuard to `vpnTier(*v) == system && proto.IsOpenConnect(v.Type)`.
  2. In `startTunnel`, set `Browser` in the system-wide `openConnectConfig` for an AnyConnect browser-sign-in profile so `SystemArgs`' existing refusal fires, and refuse one-time-code sign-in with a clear message (openconnect runs `--non-inter` and the helper has no code prompt for it).
  3. Update the vpnsystem.go header comment; no tier logic change (KTD2).
- **Patterns to follow:** `TestWindowsOpenConnectRunsSystemWide`, `TestSystemWideRoutesApplyOnConnect`, `TestHostOnASystemWideVPNConnectsItAndDialsThroughItsRoute` in `internal/app/vpnsystem_test.go`.
- **Test scenarios:**
  - With `vpnGOOS = "darwin"`, a `system` `sslvpn` profile sends `OpenConnectStart` and starts no hostbay-vpn helper.
  - With `vpnGOOS = "darwin"`, an `app` `sslvpn` profile still starts hostbay-vpn and sends nothing to the helper.
  - A connected system `sslvpn` profile with auto-apply on and `Iface: utun7` gets routes `10.62.0.0/16` and `172.16.58.0/24` on `utun7` and a pin for its gateway address via the default route. Covers AE1.
  - The same profile disconnecting removes the pin and its routes go with the interface. Covers AE2.
  - A host bound to the system `sslvpn` profile dials through its route (the existing dial-path test, with an OpenConnect profile).
  - A system `anyconnect` profile with browser sign-in fails to connect with the browser refusal and sends nothing to the helper.
  - A system OpenConnect profile with one-time-code sign-in fails to connect with a message naming the sign-in, and sends nothing to the helper.
- **Verification:** `internal/app` tests pass with `-race`.

### U3. Editor offers system-wide for OpenConnect profiles on macOS

- **Goal:** the user can switch an OpenConnect profile between app-scoped and system-wide on macOS, with auto-apply turned on when choosing system-wide.
- **Requirements:** R1, R2 (KTD2, KTD4).
- **Dependencies:** none for tests; U1 and U2 for the live check.
- **Files:**
  - `src/shell/vpnForm.ts` (tier choices per type and OS)
  - `src/shell/settings/Vpns.tsx` (tier control; the type change must not discard a chosen tier the new type allows)
  - `src/i18n/en.ts` and the other locale files (new keys; update the `vpnForm.typeLater` hint)
  - Tests: `src/shell/vpnForm.test.ts`, a Vpns form test mocking `../platform` as `src/components/VpnPanel.test.tsx` does
- **Approach:**
  1. Show the tier control only for OpenConnect types and only when `isMac()`; on other systems the form behaves as today.
  2. Offer system-wide only while the sign-in is password; switching the sign-in to anything else returns the tier to app-scoped (Scope Boundaries).
  3. Choosing system-wide sets `autoApply: true`; choosing app-scoped leaves auto-apply as it was.
  4. Keep the existing "password is asked again" warning when the tier changes.
- **Patterns to follow:** existing selects and the auto-apply checkbox in `src/shell/settings/Vpns.tsx`; `isMac()` from `src/platform.ts`.
- **Test scenarios:**
  - On macOS, an `sslvpn` profile shows the tier control with app-scoped selected.
  - Choosing system-wide sets tier `system` and auto-apply on.
  - Choosing app-scoped again sets tier `app`.
  - Changing the type from `sslvpn` to `anyconnect` keeps a chosen system-wide tier; changing to `wireguard` falls back to that type's own default.
  - An `anyconnect` profile with browser sign-in, or any OpenConnect profile with certificate or one-time-code sign-in, cannot select system-wide; switching a system-wide profile's sign-in to one of those returns it to app-scoped.
  - Off macOS, no tier control is shown and `tierFor` is unchanged.
- **Verification:** `npm test` and `npm run build` pass.

### U4. Record the revised platform split

- **Goal:** the VPN profiles plan states that OpenConnect types run system-wide on request on macOS.
- **Requirements:** R1.
- **Dependencies:** U1.
- **Files:** `docs/plans/2026-10-30-1022-feat-vpn-profiles-plan.md` (supported-types rows for AnyConnect, SSL VPN and the same-split rows; that plan's KTD8, the `hostbay-netd` helper decision).
- **Approach:** edit the existing rows and that plan's helper KTD in place; do not add a history note.
- **Test expectation:** none -- documentation only.
- **Verification:** the rows and the VPN profiles plan's helper KTD match what U1 builds.

---

## Verification Contract

| Gate | Command or check | Proves |
|---|---|---|
| Go tests | `go test -race ./internal/netd/... ./internal/app/... ./internal/vpn/...` | U1, U2 |
| Go vet and cross-builds | `go vet ./...`; `GOOS=linux go build ./...`; `GOOS=windows go build ./...` | OS splits compile |
| UI | `npm test`; `npm run build` | U3 |
| Bindings | regenerate with `wails3 generate bindings -d src/bindings -ts -i` only if a Go type the UI uses changed | no drift |
| Live | package with `wails3 task package`, install to `/Applications`, reinstall the helper from Settings, Routing (admin prompt), set Corp Internal to system-wide and re-enter its password, connect | AE1, AE2, R7 |
| Live trace | `fs_usage` on the root openconnect pid during connect | KTD8 |

The live rows need the user's admin prompt and VPN password; the executor runs everything up to that point and the user finishes the DBeaver check.

## Definition of Done

- U1 to U4 committed, one commit per unit, on `main`, not pushed, author per the repo's local `user.email`.
- All automated gates above pass.
- The live check reaches AE1 and AE2, or the hand-off names exactly which live step is left for the user.
- No abandoned-attempt code left in the diff.
