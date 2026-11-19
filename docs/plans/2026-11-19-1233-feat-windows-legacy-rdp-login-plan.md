---
title: Windows Legacy RDP Login - Plan
type: feat
date: 2026-11-19
artifact_contract: ce-unified-plan/v1
product_contract_source: ce-plan-bootstrap
execution: code
---

# Windows Legacy RDP Login - Plan

## Goal Capsule

- **Objective:** A Hostbay user can sign in to a Windows server that has NLA turned off, or whose NLA refuses a sign-in the classic Windows logon screen would accept, and always knows when a session opened without NLA. A host the user set to "NLA only" never signs in any weaker way.
- **Means:** "Automatic" falls back once to TLS with the Windows logon screen (KTD8). The saved password signs in there automatically (KTD2). The session survives the logon-screen handover (KTD9). The status bar says "without NLA" (KTD10). Clear labels and messages (KTD1, KTD3), one account-name rule (KTD6), and `.rdp` import (KTD7) complete it.
- **Authority:** the user's words relayed by the coordinator: "Server with NLA off" is the failing case, and "this we need as fallback". Then the coordinator's brief, then this plan. `CLAUDE.md` (public repository) overrides everything.
- **Stop conditions:** stop and report if a change would let a host saved as "nla" negotiate anything but CredSSP, would send a credential to a server whose certificate the user has not accepted, or would offer Standard RDP Security in any mode.
- **Execution profile:** one branch, one commit per unit, no push. The coordinator merges into `main`.

---

## Product Contract

### Summary

Today, "Automatic" offers TLS and CredSSP together. A Windows server with NLA turned off picks TLS, and the session opens at the Windows logon screen. But the saved password is not used there, so the user types it again. If that user already has a disconnected session, the server swaps desktops after logon, and Hostbay ignores that step. When NLA refuses an expired password, the user gets a raw error, and nothing falls back.

This plan adds one fallback from NLA to the logon screen in "Automatic" and automatic logon. It handles the desktop swap, marks sessions without NLA, and makes the labels, messages, account names and `.rdp` import right. Standard RDP Security (no TLS) is not added: the pinned IronRDP cannot encrypt it.

### Problem Frame

A Windows server reaches Hostbay in one of three ways:

- NLA (CredSSP over TLS). This works today.
- TLS with the classic logon screen, when NLA is turned off on the server.
- The legacy RDP security layer, with RC4 and no TLS.

The user's failing case is the second one. Evidence for what "auto" did before this change:

- `build_config` in `rdp-helper/src/rdp.rs` sets `enable_tls` and `enable_credssp` for "auto". IronRDP then requests `SSL|HYBRID|HYBRID_EX` (`ironrdp-connector` `connection.rs`, `enabled_security_protocols`).
- A server with CredSSP off selects `SSL`. The connector skips CredSSP (`EnhancedSecurityUpgrade` → `BasicSettingsExchangeSendInitial`) and sends Client Info with the password but without `AUTOLOGON`, because the helper sets `autologon: false`. Windows therefore shows the logon screen with the fields empty.
- After logon, a server that reconnects the user to an existing session sends Deactivate All. `run_active` drops `ActiveStageOutput::DeactivateAll` (`_ => {}`), so the desktop never comes back.
- A server that supports CredSSP still selects HYBRID even when NLA is not required. An NLA refusal there (expired password, must change) ends the connection with a raw error.

### Requirements

**Choices and fallback**

- R1. The form labels and hints let a Windows user see which choice reaches a server with NLA off. Stored values `auto`, `nla` and `tls` keep their meaning, so saved hosts load unchanged.
- R2. The TLS logon-screen choice shows a warning naming what it gives up: the server opens a session before it checks the account. "Automatic" says it may open the logon screen. "NLA only" says it refuses servers without NLA.
- R3. "Automatic" falls back once, without CredSSP, when NLA refuses for a reason the logon screen can handle: password expired, password must change, account restriction, or logon type not granted. It never falls back on a wrong password, an account lockout, or an ambiguous failure.
- R4. "NLA only" never falls back and never offers TLS without CredSSP. No mode ever offers Standard RDP Security.
- R5. The fallback connection repeats the same certificate check and pin, so the password reaches only a server whose certificate the user accepted.

**Sign-in and session**

- R6. In every mode, a saved password is sent for automatic logon. With no password, the Windows logon screen appears empty.
- R7. `DOMAIN\user`, `MACHINE\user`, `.\user`, `user@domain` and a bare `user` reach the server in the form Windows expects, in every mode.
- R8. A failure after a credential was sent is never retried automatically. The only exception is the one R3 fallback, which sends no wrong password.
- R9. A session that survives the server's Deactivate All keeps running, at the server's new desktop size.
- R10. A session that opened without CredSSP says so in the RDP tab's status ("connected without NLA"). The helper also logs it.

**Messages**

- R11. NLA failures name the host setting to change, by its form label. TLS handshake failures from servers that offer only old TLS versions or cipher suites get a plain message. So do servers set to the legacy RDP security layer, which their administrator can change, and servers too old to offer TLS at all.
- R12. Each friendly message keeps the raw cause after it.

**Import**

- R13. `.rdp` import reads mstsc's UTF-16 files. It keeps `DOMAIN\user`, and combines `domain:s:` with a bare `username:s:`. It maps `enablecredsspsupport:i:0` to the TLS logon-screen choice, but never lowers a host saved as "NLA only". A security change made by an import is shown in its own toast.

### Scope Boundaries

- Standard RDP Security is not built (KTD5).
- TLS 1.0/1.1 and non-AEAD or RSA-key-exchange cipher suites are not added. rustls does not offer them, so Server 2008 R2 and older stay unreachable over TLS; they get the R11 message.
- No fallback is made on ambiguous NLA failures, such as a connection drop during CredSSP: such a drop cannot be told apart from a wrong password, and repeating it risks a lockout.
- `authentication level` in `.rdp` files is not mapped. It is a certificate-warning policy, and mapping it would weaken pinning.
- Kerberos-only servers stay unsupported: the helper passes no KDC.
- Helper messages stay in English, like the other helper errors.

---

## Planning Contract

### Key Technical Decisions

- KTD1. **Relabel; add no stored value.** Labels:
  - `auto`: "Automatic: NLA, or the Windows logon screen if NLA fails"
  - `tls`: "Windows logon screen, no NLA (also xrdp)"
  - `nla`: "NLA only, never the logon screen"

  The `tls` hint uses the editor's `text-warning` style, like the other unencrypted or weakened choices. The other two hints are muted.
- KTD2. **`autologon: !password.is_empty()` in every mode.** IronRDP's Client Info carries the password in every mode whatever the flag says (`create_client_info_pdu`), so the flag sends nothing new. mstsc and FreeRDP set it the same way. xrdp then skips its own login dialog when a password is saved.
- KTD3. **Typed classification.** `connect` turns errors into a `ConnectFail { message, stage, fallback }`.
  - Negotiation failures are read from `ConnectorErrorKind::Negotiation(f).code()`.
  - NTSTATUS is read from `sspi::Error::nstatus`, found by walking `source()`.
  - The pre-negotiation "server selected STANDARD_RDP_SECURITY" case is a plain `Reason` string. Its test drives a real `ClientConnector` with an empty Connection Confirm, so upstream rewording fails the test.
  - TLS handshake failures are classified from `rustls::Error` inside `tls_upgrade`.
  - Labels named in messages are Rust constants, and a test checks them against `src/i18n/en.ts`.
- KTD4. **Retry transient resets only before credentials.** A retry needs both a pre-`connect_finalize` stage and the existing reset patterns. Negotiation refusals and certificate refusals are never retried. All credentials leave inside `connect_finalize`: CredSSP, and Client Info in the basic settings exchange. `connect_begin` stops at the security upgrade.
- KTD5. **No Standard RDP Security.** IronRDP `baa9a65` supports it only at `ENCRYPTION_LEVEL_NONE` (`lib.rs` doc on `enable_standard_rdp_security`, `connection.rs` "RDP Security Commencement"). Windows never negotiates that level, and adding RC4 means writing protocol cryptography.
- KTD6. **One account-name rule.** A name containing `\` splits on the first `\`. A name with `@` and no `\` stays whole as a UPN, with no domain, which matches sspi's `AuthIdentityBuffers` encoding. Otherwise the name is the account alone. `.\user` keeps `.` as its domain. sssd's default regex accepts `user@domain`, so xrdp hosts keep working.
- KTD7. **`.rdp` parsing in `src/shell/rdpFile.ts`, with decoding in Go.** `ReadTextFile` decodes a UTF-16LE BOM and strips a UTF-8 BOM before the text crosses into the page. `importRdp` is its only caller.
- KTD8. **Fallback lives in the helper's `run` loop.** On a `fallback` failure with security "auto", it reconnects once with "tls". The full `connect` runs again, so the certificate check (`Out::Cert`, then `await_trust`) repeats; Go's handshake loop already accepts several `cert` messages. NTSTATUS codes that trigger the fallback: `PASSWORD_EXPIRED`, `PASSWORD_MUST_CHANGE`, `ACCOUNT_RESTRICTION`, `LOGON_TYPE_NOT_GRANTED`. Each of these means the password was right, so the fallback never repeats a wrong one.
- KTD9. **Deactivation-Reactivation.**
  1. On `DeactivateAll`, drive `conn_result.activation_factory.create()` with `read_by_hint`, retrying on the 12 ms read timeout up to a deadline.
  2. When it reaches `Finalized`, call `stage.reactivate(..)` and rebuild the image at the new size.
  3. Send `Out::Size`. This follows IronRDP's own client (`ironrdp-client` `rdp.rs`, `DeactivateAll` arm).
- KTD10. **"Without NLA" travels on the size message.** The helper's `size` gains `noNla` (absent means false, so old and new stay compatible without a version bump). Go appends a flags byte to `MsgSize`. `rdpView` passes `noNla`, and RdpTab shows "connected without NLA".

---

## Implementation Units

### U1. Security choice labels and hints

- **Goal:** R1, R2.
- **Files:** `src/i18n/en.ts`, `src/shell/HostEditor.tsx` (RDP section only).
- **Approach:** use the KTD1 labels and three hint keys. Render the `tls` hint as a `text-xs text-warning` line, as the editor does for other weakened choices.
- **Test scenarios:** the i18n key test, which covers new keys. The label cross-check lives in U4.
- **Verification:** `npx tsc --noEmit` and `npx vitest run` pass.

### U2. Automatic logon

- **Goal:** R6.
- **Files:** `rdp-helper/src/rdp.rs`.
- **Approach:** see KTD2.
- **Test scenarios:**
  - With a password, every mode sets `autologon`.
  - With an empty password, no mode sets it.
- **Verification:** `cargo test` passes.

### U3. Account-name splitting

- **Goal:** R7.
- **Files:** `rdp-helper/src/rdp.rs`.
- **Approach:** add `split_account` under KTD6.
- **Test scenarios:**
  - `CORP\user`
  - `PC01\user`
  - `.\user`
  - `user@example.com`, which stays whole
  - `MicrosoftAccount\a@example.com`
  - bare `user`
  - `\user`, which gives no domain
  - Each result passes `sspi::Username::new` with the expected UPN or down-level format.
- **Verification:** `cargo test` passes.

### U4. Messages and retry stage

- **Goal:** R8, R11, R12.
- **Files:** `rdp-helper/src/rdp.rs`.
- **Approach:** implement KTD3 and KTD4.
  - `explain` covers `PASSWORD_EXPIRED` / `PASSWORD_MUST_CHANGE` / `ACCOUNT_RESTRICTION` / `LOGON_TYPE_NOT_GRANTED`. For a "nla" host, the message points to "Automatic"; for an "auto" host, it is used only if the fallback also fails.
  - It also covers `LOGON_FAILURE`, `HYBRID_REQUIRED_BY_SERVER` (the server needs NLA), and `SSL_REQUIRED_BY_SERVER` (the server has no NLA; points to "Automatic").
  - `SSL_NOT_ALLOWED_BY_SERVER`: the server is set to the legacy RDP Security Layer, and its administrator can set Negotiate or SSL.
  - A pre-negotiation reply means the server is too old for TLS.
  - A TLS protocol-version or handshake-failure alert means the server offers only old TLS versions or cipher suites.
- **Test scenarios:**
  - Each mapping above, built from the real library error types.
  - The pre-negotiation case, driven through a real `ClientConnector`.
  - An unrelated error gives no explanation.
  - Labels in Rust match `en.ts`.
  - A post-credential reset is not retried.
  - A negotiation refusal is not retried.
- **Verification:** `cargo test` and `cargo clippy` are clean.

### U5. One fallback from NLA to the logon screen

- **Goal:** R3, R4, R5.
- **Files:** `rdp-helper/src/rdp.rs`.
- **Approach:** implement KTD8.
  1. Add a pure `next_security(security, &ConnectFail, fell_back) -> Option<&str>` decision.
  2. Have the retry loop use it.
  3. Log the fallback to stderr.
- **Test scenarios:**
  - auto + `PASSWORD_EXPIRED` gives "tls".
  - auto + `LOGON_FAILURE` gives none.
  - auto + an ambiguous CredSSP drop gives none.
  - auto after one fallback gives none.
  - nla + `PASSWORD_EXPIRED` gives none.
  - tls gives none.
  - Negotiation flags per mode: nla offers CredSSP only, tls offers TLS only, auto offers both, and no mode enables Standard RDP Security (extends `nla_mode_never_offers_plain_tls`).
- **Verification:** `cargo test` passes.

### U6. "Connected without NLA" status

- **Goal:** R10.
- **Files:**
  - `rdp-helper/src/protocol.rs` and `rdp-helper/src/rdp.rs`
  - `internal/rdp/supervisor.go` and `internal/rdp/supervisor_test.go`
  - `src/api.ts`, `src/tabs/RdpTab.tsx`, `src/i18n/en.ts`
- **Approach:** implement KTD10.
  - The helper records `connector.should_perform_credssp()` before finalize.
  - Go keeps the flag from the first `size` and adds it to every `MsgSize`.
- **Test scenarios:**
  - The helper's `size` serializes `noNla`.
  - The Go fake helper's `size` with `noNla: true` yields a `MsgSize` with the flag byte set; without it, the byte is clear.
- **Verification:** `go test ./internal/rdp/...`, `npx tsc --noEmit` and `npx vitest run` pass.

### U7. Deactivation-Reactivation

- **Goal:** R9.
- **Files:** `rdp-helper/src/rdp.rs`.
- **Approach:** implement KTD9.
- **Execution note:** no local server sends Deactivate All on demand, so prove it by compiling against the real API and by following IronRDP's own client. List it as unverified without a Windows host.
- **Test scenarios:** none runnable without a server sending Deactivate All. The step-loop helper that tolerates read timeouts gets a unit test with a stream that returns `WouldBlock` once.
- **Verification:** `cargo test` and `cargo clippy` are clean.

### U8. `.rdp` import

- **Goal:** R13.
- **Files:**
  - `internal/app/hosts.go` and `internal/app/hosts_test.go`
  - `src/shell/rdpFile.ts` and `src/shell/rdpFile.test.ts` (new)
  - `src/shell/HostEditor.tsx` (`importRdp` only)
  - `src/i18n/en.ts`
- **Approach:** implement KTD7.
  - `importRdp` applies `tls` only when the host is unset or "auto".
  - When it does, a warning toast names the new choice.
- **Test scenarios:**
  - Go: UTF-16LE with BOM decodes; a UTF-8 BOM is stripped; plain UTF-8 is unchanged.
  - TS: `full address` with and without a port.
  - TS: `CORP\user` is kept.
  - TS: `domain:s:` plus a bare user gives `CORP\user`.
  - TS: a UPN with `domain:s:` is left alone.
  - TS: `enablecredsspsupport:i:0` gives `tls`; `:1` or a missing key gives none.
  - TS: `authentication level` is ignored.
  - TS: the security merge gives tls over unset, tls over auto, and keeps nla when the file says tls.
- **Verification:** `go test ./internal/app/...` and `npx vitest run` pass.

---

## Verification Contract

| Area | Command |
|------|---------|
| Helper | `cargo test`, `cargo clippy` in `rdp-helper/` |
| Go | `go vet ./...`, `go test ./...` (after `npm ci` and `npm run build`) |
| UI | `npx tsc --noEmit`, `npx vitest run` |

Not verifiable here, because no Windows host is available:

- What a real server selects with NLA off.
- The NTSTATUS codes each Windows version sends.
- Autologon at the logon screen.
- Deactivate All after logon.
- NTLM with `.\user`.
- TLS against Server 2008 R2.

## Definition of Done

- U1 to U8 are each committed with their tests.
- All checks above pass.
- No experimental code is left in the diff.
- The report lists what was unverified and what was not built.
