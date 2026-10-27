# Remote SSH Manager — Standalone Desktop App (v1.0)

**Date:** 2026-06-30
**Status:** Approved for implementation
**Type:** Full rewrite (Next.js web app → Tauri desktop app)

## 1. Goal

Turn the current Next.js SSH manager (a local-only, partly-broken web prototype) into a
**complete, professional cross-platform desktop application** for managing remote Linux
servers over SSH. Ships signed installers for **macOS (.dmg/.app)** and **Windows
(.msi/.exe)**. No browser, no HTTP server, no Node runtime in the shipped product.

## 2. Why standalone

The app is already 100% local and single-user: it reads the user's `~/.ssh`, runs SSH
from the user's machine, has no multi-user/server concern. The Next.js server + browser
is pure overhead. A desktop shell removes the HTTP hop, keeps private keys in the native
process (never in browser memory or repeated request bodies), and yields a real
installable product.

## 3. Stack

| Layer    | Choice |
|----------|--------|
| Shell    | Tauri 2 |
| Backend  | Rust (`#[tauri::command]` + events) |
| SSH      | `russh` + `russh-keys` + `russh-sftp` (pure Rust; no Node, no system ssh) |
| Frontend | React 19 + Vite + TypeScript (current components reused/restructured) |
| Styling  | Tailwind v4 + shadcn-style component primitives |
| Editor   | Monaco (file editing, syntax highlight) |
| Terminal | xterm.js + xterm-addon-fit (kept from current app) |
| Icon     | Custom generated app icon (internal team tool) |

## 4. What is broken in the current app (root causes the rewrite fixes)

- `eval("require")("ssh2")` bundler hack → gone (Rust native SSH).
- Private key sent in **every** request body (status 4×, every keystroke) → key stays in
  Rust; frontend gets an opaque `sessionId`, never the key after connect.
- New SSH connection **per API call** (status = 4 connects) → one pooled persistent
  session per host, reused by all features.
- Broken scripts: `apt-get update | apt-get install` and `usermod | newgrp` use a pipe
  where they need `&&` → fixed.
- SSE terminal stream splits payload on `\n` (fragile, mangles escape sequences) →
  binary-safe Tauri event channel.
- Hardcoded `ubuntu` user and `/home/ubuntu/ssl/` paths → generic, user-configurable.
- `setStatusLoading(false)` called twice; failures swallowed with `.catch(()=>{})` →
  proper error surfacing + global error boundary + toasts.
- No host-key verification (MITM-open) → verify against `known_hosts`, prompt on unknown.
- Dead commented code across 4 files → removed.

## 5. Architecture

```
Tauri 2 Window (WebView)
  React + Vite + TypeScript
  Sidebar: hosts + connection status
  Tabs: Dashboard · Terminal · Files · Docker · Services
  Command palette (Cmd/Ctrl-K) · toasts · global error boundary
        |  invoke() + event listeners
Rust core (src-tauri/src/)
  session.rs  one russh session per host, pooled, auto-reconnect, host-key verify
  exec.rs     run command -> {stdout, stderr, code}
  pty.rs      interactive shell -> streamed `terminal://{id}` events
  sftp.rs     list/stat/read/write/mkdir/rename/remove/copy
  sudo.rs     EACCES -> `base64 -d | sudo tee` write fallback
  docker.rs   containers · images · compose · stats · logs (streamed)
  services.rs nginx test/reload/status · systemd unit control
  hosts.rs    app host store (hosts.json) + ~/.ssh/config import/sync + export/import
  keys.rs     discover ~/.ssh keys · PPK -> OpenSSH PEM (pure Rust)
  state.rs    Tauri managed state: session map + in-memory key handling
  lib.rs      command registration + app setup
```

### Key/session security model
1. `connect(alias|hostParams)` → Rust loads private key from disk, opens russh session,
   stores it in managed state keyed by `sessionId`, returns `sessionId`.
2. All later commands (`exec`, `sftp_*`, `docker_*`, `pty_*`) take only `sessionId`.
3. Private key never crosses IPC after connect. One connect serves all features.
4. Host key verified against `~/.ssh/known_hosts`; unknown host → frontend prompt to
   trust (append to known_hosts) or abort.

## 6. Features

### 6.1 Connection / Host store (`hosts.rs`, `state.rs`)
- App-owned host store at `app_data_dir/hosts.json`. Each host: `alias`, `hostname`,
  `port`, `user`, `auth` (key path), `tags`, `lastConnected`.
- CRUD in UI (add / edit / delete).
- **Import** existing `~/.ssh/config` entries on first run (and on demand).
- **Sync back** to `~/.ssh/config` (optional toggle) so the `ssh` CLI still works. App
  store is source of truth; `~/.ssh/config` is a mirror.
- **Export / Import** the whole store + settings as a single `.json` file (prompt to
  encrypt if inline keys are ever included). Lets the user move setup between machines.

### 6.2 Terminal (`pty.rs`)
- Real PTY (`request_pty` + `shell`) over the pooled session.
- Output streamed via `terminal://{id}` events (binary-safe).
- Multiple terminal tabs per host. Resize, reconnect, copy/paste (built properly — the
  current app's commented-out clipboard code, done right).

### 6.3 File Manager (`sftp.rs` + `sudo.rs`) — replaces nginx + ssl editors
- Browse any path from `/`; breadcrumb + bookmarks (`/etc/nginx`, `/etc/ssl`, home,
  `/var/log`).
- List with name, size, perms, owner, mtime, type icon.
- Open text file → Monaco editor inline → save.
- New file/folder, rename, delete (confirm), chmod.
- Copy / cut / paste (remote↔remote).
- Upload ↔ download (local↔remote via Tauri dialogs).
- **Sudo-aware write:** plain SFTP write first; on `EACCES` retry
  `echo <b64> | base64 -d | sudo tee <path>` (generalizes current nginx trick to any
  file). Assumes passwordless sudo (EC2 default); password prompt deferred to backlog.

### 6.4 Docker (`docker.rs`)
- **Containers:** `docker ps -a` table; start/stop/restart/remove; live logs
  (`docker logs -f` streamed); exec → opens terminal tab `docker exec -it <id> sh`.
- **Images:** list; pull; remove; prune dangling.
- **Compose:** discover compose files (home + user-set project paths); per project
  `up -d` / `down` / `restart` / `ps` / `logs`.
- **Stats/system:** `docker stats --no-stream` polled (live CPU/mem); `docker system df`;
  prune-all (confirm).
- Permission-denied on `docker ps` → one-click fix (`sudo usermod -aG docker $USER`,
  note re-login).

### 6.5 Dashboard (per host)
- OS / uptime / load, disk usage, nginx/docker/azure-agent status — fired **once** over
  the pooled session (fixes the 4×-connect status bug), cached, manual refresh.

### 6.6 Services (`services.rs`)
- nginx: test (`nginx -t`), reload, status.
- systemd: list units, start/stop/restart.
- Quick-install scripts (fixed `&&`), reboot.

### 6.7 Scripts
- `commandButtons` → data-driven, **user-editable** list persisted in store
  (add/edit/remove own scripts).

### 6.8 PPK → PEM (`keys.rs`)
- Pure-Rust PPK parse → OpenSSH PEM. No `puttygen` dependency. Works offline on any OS.

### 6.9 UI redesign
- Tailwind v4 + shadcn-style primitives, dark/light, sidebar + tabbed workspace,
  Cmd/Ctrl-K command palette, toast notifications, global error boundary, real
  loading/empty/error states. Keyboard-first.

## 7. Production bundle
- Tauri bundler → `.dmg` + `.app` (macOS), `.msi` + `.exe` (Windows).
- **macOS deliverable for this engagement:** a built, locally-installed `.app` (ad-hoc /
  unsigned dev build acceptable for local testing). Notarization (Apple Developer ID)
  documented but deferred until the user provides credentials.
- Windows signing (Authenticode) documented, deferred.
- Custom app icon + brand theme (internal team tool), dark + light mode.
- Auto-update (Tauri updater + GitHub Releases) — deferred to backlog.
- CI (GitHub Actions mac+win matrix) — deferred to backlog.

## 8. Phased build sequence

1. **Scaffold** — Tauri 2 + Vite/React/TS; move current components; Tailwind v4; default icon.
2. **SSH core** — `session.rs` (pooled connect, host-key verify), `keys.rs` (discovery + PPK→PEM).
3. **Terminal** — `pty.rs` + multi-tab xterm over events.
4. **Files** — `sftp.rs` + `sudo.rs` + Monaco file manager + up/download.
5. **Docker** — `docker.rs` + all four panels.
6. **Hosts/Dashboard/Services/Scripts** — store, import/export, `~/.ssh/config` sync,
   status dashboard, services, editable scripts.
7. **UI polish** — palette, toasts, error boundary, dark/light, empty/error states.
8. **Bundle** — build + locally install macOS `.app`.

Each phase lands working and is committed separately.

## 9. Out of scope (v1.0 backlog)
- macOS notarization / Windows Authenticode signing (need credentials).
- Auto-update feed + CI release pipeline.
- Sudo password prompt (assume passwordless sudo for now).
- SSH agent / password auth (key-file auth only for v1).
- Port-forwarding / tunnels UI.

## 10. Testing
- Rust: unit tests for `keys.rs` (PPK→PEM round-trip), `hosts.rs` (config parse/merge),
  `sudo.rs` path/escape safety.
- Manual: connect to a real host, exercise terminal/files/docker end-to-end before the
  bundle step.
