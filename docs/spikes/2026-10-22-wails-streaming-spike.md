# Wails v3 streaming spike (U2)

Date: 2026-10-22. Wails v3.0.0-beta.27, Go 1.27.3, macOS 26.5 (WKWebView), packaged
(production) asset server, origin `wails://localhost`. Windows and Linux are not measured
yet; repeat this spike there before U6 and U14 rely on the result.

The plan's Open Questions asked whether Wails events carry terminal output fast enough,
and whether the packaged webview may open a WebSocket to `127.0.0.1`.

| Path | Result |
|---|---|
| `app.Event.Emit` of 4 KB strings, paced at the 5 MB/s target | 5.3 MB/s received in JS, no lag over 5 s |
| `app.HandleStream` / JS `Stream()` (GoStream), 64 KB binary frames | ~2.5 GB/s received in JS |
| Loopback WebSocket from the page to `ws://127.0.0.1:<port>` | Allowed; ~1.75 GB/s; request `Origin` is `wails://localhost` |

Unpaced events are not safe: emitting as fast as Go can (gigabytes per second of
queued events) starved the page so badly that nothing else ran. KTD9's bounded
emit loop is required, not optional.

## What this changes

- Terminal output (KTD9, U6) can use Wails events behind the bounded emit loop.
- GoStream gives a bounded, backpressured, origin-bound binary channel with no
  listening port. RDP frames (KTD3, U14) should use it instead of a loopback
  WebSocket: frames go from the helper to Go over the control pipe or a pipe of
  their own, then to the page over GoStream. With no local listener, the token,
  `Origin`, `Host` and single-upgrade checks KTD3 lists for the WebSocket are no
  longer needed, and neither DNS rebinding nor other local users can reach the
  frame channel. The Rust helper decision itself is unchanged.
- If a loopback WebSocket is ever used, check `Origin == "wails://localhost"`
  (macOS value; confirm the Windows and Linux values).

The spike source is not kept; it was a throwaway Wails app in the session scratchpad.
