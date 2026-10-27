# Copilot Instructions for EC2 Maintainer (2025-08)

## Project Overview

- Next.js 15 app (TypeScript, Tailwind, App Router) for managing AWS EC2 instances over SSH.
- Key features:
  - Live terminal (xterm.js) with robust connect/disconnect, resize, and clipboard support.
  - App status checks (nginx, docker) with loading/cancellation.
  - Quick install script buttons (data-driven, see `src/utils/commandButtons.ts`).
  - SSH host management (read/append ~/.ssh/config, auto-fill username/key).
  - Nginx config editor: list, read, edit, test, and reload any .conf under `/etc/nginx/*.conf`, `/etc/nginx/*/*.conf`, `/etc/nginx/conf/*.conf`.

## Architecture & Patterns

- Main app code: `src/app/`
- UI components: `src/components/`
- SSH/EC2 logic: `src/lib/ssh.ts`, `src/lib/terminal.ts`
- Data-driven quick scripts: `src/utils/commandButtons.ts`
- Nginx config API: `src/app/api/nginx/` (list, read, save, test, reload)
- Terminal API: `src/app/api/terminal/`

## Developer Workflows

- Install: `npm install`
- Dev server: `npm run dev`
- Lint: `npm run lint`
- Build: `npm run build`

## Conventions

- TypeScript for all code.
- Tailwind CSS for styling.
- Reusable UI in `src/components/`.
- SSH/EC2 logic in `src/lib/`.
- Add new quick script buttons in `src/utils/commandButtons.ts`.
- Nginx config editor supports any .conf file in `/etc/nginx` (one subdir max, e.g. `conf/site.conf`).

## Integration Points

- SSH via `ssh2` (dynamic import, Node-only).
- xterm.js for terminal UI (client-only, dynamic import).
- Nginx config API: supports list/read/save/test/reload for .conf files in `/etc/nginx` and subdirs.
- Sudo required for nginx save/test/reload; user must have passwordless sudo for those commands.

## Examples

- See `src/components/NginxEditor.tsx` for config editor UI.
- See `src/components/LiveTerminal.tsx` for terminal UI and imperative API.
- See `src/app/api/nginx/` for backend config file logic.
- See `src/utils/commandButtons.ts` for quick script button config.

---

Update this file as new features, patterns, or workflows emerge.
