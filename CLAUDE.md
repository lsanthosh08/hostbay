# Hostbay

Hostbay is a Wails 3 desktop app (Go backend in `internal/`, React and
TypeScript UI in `src/`) for SSH, RDP, VNC, databases, containers and VPNs.

## This repository is public

Everything committed here, including docs, plans, tests, fixtures and commit
messages, is published on GitHub, and history keeps it even after a later
commit deletes it. Write every file as if a stranger will read it.

Never commit:

- The name of any employer, customer or organization whose networks Hostbay
  is used on, or the names of their VPN profiles, servers, databases or
  internal systems.
- Real hostnames, domain names, cloud resource endpoints (RDS, Client VPN,
  EKS and so on), account IDs or public IP addresses of real systems.
- Real internal network ranges or tunnel addresses copied from a live setup.
- Keys, passwords, tokens, certificates, `.ovpn` files with real gateways, or
  any other credential, real or "only for testing" against a real service.
- Usernames, machine hostnames or local paths that identify a person.

Use stand-ins instead:

- Names: `Corp Internal`, `Branch`, `Office`, `vpn.example.test`,
  `sql.corp.example`.
- Domains: `example`, `example.com`, `example.test` (RFC 2606).
- Addresses: `192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24` (RFC 5737),
  or a made-up private range that does not match a live network.
- Secrets: obviously fake values such as `sk-ant-sample-key`.

When a plan or bug report describes a real setup, describe its shape (an
app-scoped SSL VPN, a SQL Server behind it on port 1433) rather than its
names. Before every push, check the commits being pushed for real names and
addresses (`git log -p origin/main..HEAD`).

## Commits

- Commit directly on `main`, one logical change per commit, as a new commit.
  Never amend.
- The author email for this repository is set in its local git config.
  Keep that override; never commit with a `*.local` hostname address.
- No `Co-Authored-By` trailer and no "Generated with" line.
- Never push unless asked. A push of a `v*` tag starts the Release workflow.

## Checks

- Go: `go test ./...` (the main package embeds `dist/`, so build the UI first
  with `npm run build` when `dist/` is missing).
- UI: `npx vitest run`.
