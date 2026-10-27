# In-App Windows Remote Desktop (RDP via IronRDP)

**Date:** 2026-07-04
**Status:** Approved design — build not yet started (large, phased)
**Depends on:** existing SSH Manager (Tauri 2 + Rust + React)

## Goal

Control a Windows host's desktop *inside* SSH Manager. Connect over RDP (NLA
user/password), render the live desktop in a canvas tab, full mouse + keyboard.
Built on IronRDP (Rust). Daily-heavy-use quality bar → phased delivery.

## Decisions (locked in brainstorming)

- **Use case:** daily heavy use → high bar, multiple phases.
- **Auth:** NLA username + password (CredSSP). IronRDP supports it.
- **Cred storage:** encrypted in the app store (app-held key). *Weaker than OS
  keychain — documented tradeoff; keychain is the upgrade path.*
- **v1 scope:** interactive single-monitor (connect, render, mouse+keyboard,
  fit/scale, reconnect). Clipboard, multi-monitor, perf tuning = later phases.

## Architecture

```
React RdpTab: <canvas> paints frames · captures mouse/keyboard · sends input
      │  invoke(rdp_connect/rdp_input/rdp_resize/rdp_close)
      │  ▲ event rdp-frame://{id}  (changed-region RGBA)
Rust  rdp.rs
  ironrdp-client session per host (own tokio task)
   TCP 3389 → X.224 → CredSSP/NLA (user/pass) → capability exchange
   loop: decode graphics (bitmap/RLE/RemoteFX) → RGBA framebuffer
        → emit changed rects as rdp-frame events
   input: mouse/key → RDP input PDUs
  RdpStore: sessionId → handle (mirrors SessionStore)
  password: reuse host password field, deobfuscated at connect
```

**Separate from SSH.** A host is SSH *or* RDP via a `kind` field. Nothing
touches the russh path.

**Frame transport (v1):** send changed-rectangle RGBA to the webview as events;
canvas blits. `ponytail:` this is the simple transport — if fps is poor at the
daily-heavy bar, phase 3 optimizes (shared buffer / damage batching / codec
passthrough). Ship correct first.

## Phases

1. **SPIKE (make-or-break, do first):** headless Rust test — connect + CredSSP
   auth + decode ONE frame to a BMP, against a REAL Windows host. Proves
   IronRDP + NLA work on the target hosts. If auth fails here, stop and
   reassess — this is where RDP integrations stall. No UI yet.
2. **Session backend:** `rdp.rs` connect/close + RdpStore + frame event stream +
   input commands. Host model gains `kind: "ssh" | "rdp"`.
3. **RdpTab UI:** canvas that paints frame events, captures pointer + keyboard,
   fit/scale, connect/disconnect. Wire into the existing tab system.
4. **Robustness:** reconnect, resolution/DPI handling, key mapping edge cases,
   error surfacing.
5. **Later (own specs):** clipboard (cliprdr), multi-monitor, perf transport.

## Risks

- **CredSSP/NLA auth** — hardest part; validated in the spike before anything
  else is built.
- **Frame throughput** — event-based transport may not hit the daily-heavy fps
  bar; has a known optimization path.
- **Windows-only testing** — needs a real Windows RDP host; can't be validated
  on macOS dev alone.

## Out of scope (v1)

RD Gateway / smartcard / SSO auth; clipboard; multi-monitor; audio; drive
redirection.

## Why not built yet

This is a multi-week feature, not a same-session change. It starts with the
Phase-1 spike against a real Windows host (which the current dev environment
can't reach). The spec is captured; implementation is a separate, sequenced
effort beginning with that spike.
