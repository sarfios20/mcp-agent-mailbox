# Contract

What no task reopens. Starting point: the private `agentes-coordinados`
server, already working, published here without history and in English.

## Language

English everywhere: code, comments, errors, MCP tool descriptions, docs and
client files. Nothing in the repo names a real person, host, IP or email,
except the copyright line in `LICENSE`.

## Stack

- Go 1.27, module `github.com/sarfios20/mcp-agent-mailbox`, one static binary
  `mcp-agent-mailbox` built with `CGO_ENABLED=0`. No Docker.
- MCP: `github.com/modelcontextprotocol/go-sdk` v1.8.0, streamable HTTP,
  `Stateless: true`, `JSONResponse: true`, path `/mcp`.
- SQLite via `database/sql` + `modernc.org/sqlite`, WAL, no ORM.
- Supported servers: Debian 12+ / Ubuntu 22.04+, systemd, OpenSSH, amd64 and
  arm64. Anything else fails at `install` naming the detected OS.

## Files

| File | Responsibility |
|---|---|
| `mailbox/mailbox.go` | Schema and all mailbox operations. Knows nothing of MCP, HTTP or the OS. |
| `config.go` | Every tunable value, together and commented. Reads and validates the environment. |
| `main.go` | Subcommand dispatch, bearer auth and the four MCP tools. |
| `admin/` | `install`, `add`, `remove`, `doctor`, `backup`, `uninstall`. Embeds systemd units and the sshd drop-in with `go:embed`. |
| `install.sh` | Downloads the release binary, checks SHA256, runs `install`. Nothing else. |
| `client/inbox.md`, `client/codex-inbox/SKILL.md` | `/inbox` for Claude Code and Codex. Both apply the trust file. |
| `README.md` | For humans and their agents: what it is, how it works, setup, trust, operations. An agent can install from it alone. |
| `test/e2e.sh` | End-to-end test on a throwaway Debian 12 VM (qemu + KVM). Never a privileged container. |

## Data and states

Unchanged from the private server. Two tables, `threads(id, subject, a, b,
created_at)` and `messages(id, thread_id, sender, recipient, content,
created_at, read_at)`. RFC 3339 UTC, set by the server. A thread has exactly
two participants. A message is `unread` (`read_at IS NULL`) → `read`, only when
its recipient calls `read_thread`.

## MCP tools

`inbox()`, `read_thread(thread_id)`, `reply(thread_id, text)`,
`send(to, subject, text)`. None accepts the sender: it comes from the token.

## Identity

- Tokens file is JSON `{"name": "token"}`, 256-bit hex tokens. It may be empty:
  the server then starts and answers 401 to everything.
- Names match `^[a-z][a-z0-9-]{0,31}$`.
- No token or unknown token: 401. The token fixes `from`.

## CLI

Every subcommand is non-interactive, prints one JSON object on stdout, and on
failure prints `{"error": "...", "detail": ...}` on stderr. Exit 0 ok, 1 failed,
2 bad usage. All except `serve` require root and are idempotent.

- `serve`: the server. Config from `MAILBOX_DB`, `MAILBOX_TOKENS_FILE`,
  `MAILBOX_ADDR` (must be loopback).
- `install [--port N]` (default 8765): system user `mailbox`, data in
  `/var/lib/mcp-agent-mailbox`, config in `/etc/mcp-agent-mailbox`
  (`tokens.json`, `config.json` with the port), backups in
  `/var/backups/mcp-agent-mailbox`, units `mcp-agent-mailbox.service`,
  `mcp-agent-mailbox-backup.service` and `.timer`, enabled and started.
  Copies itself to `/usr/local/bin/mcp-agent-mailbox`. Fails naming the process if the port
  is taken.
- `add <name> --pubkey <file|-> --host <ssh-host>`: Unix user `mailbox-<name>`
  in group `mailbox-tunnel`, `authorized_keys` with
  `restrict,port-forwarding,permitopen="127.0.0.1:<port>"`, new token, service
  restart. Same name and key again returns the same token. Output keys:
  `name`, `token`, `ssh_user`, and `client` with ready-to-paste strings
  `ssh_config`, `tunnel_systemd`, `tunnel_launchd`, `tunnel_windows`,
  `claude_code`, `codex_toml`.
- `remove <name>`: deletes the token and the Unix user. Keeps messages.
- `doctor`: prints `{"ok": bool, "checks": [{"name", "pass", "detail"}]}` on
  stdout and exits 1 if any check fails (the one exception to the error
  convention). Checks: service active, listening on loopback only, 401
  without token, sshd drop-in present and `sshd -t` valid, backup timer
  active, last backup younger than 48 h (or none yet and installed less than
  48 h ago).
- `backup`: Go port of `buzon-backup.py`, same retention (3 days, 2 weeks,
  12 months, 1 year), never deletes unverified files.
- `uninstall`: stops and removes units, tunnel users, group `mailbox-tunnel`
  and the drop-in. Keeps the `mailbox` system user, data, config and backups,
  and says where they are.

## SSH safety

The server never opens a port. Access is only through SSH tunnels. `install`
adds one drop-in, `/etc/ssh/sshd_config.d/10-mcp-agent-mailbox.conf`, matching
`Group mailbox-tunnel`, and never edits `sshd_config`. It fails if
`sshd_config` does not include `sshd_config.d`. It runs `sshd -t` before
reloading; on failure it removes its drop-in and does not reload. `add` never
touches sshd config.

## Trust

The server only guarantees who sent a message. What an agent does with it is
the receiver's choice, in `~/.config/mcp-agent-mailbox/trust`:

```
default = data
alice   = instructions
```

`data`: read, report, do not act. `instructions`: act as if the local user had
asked. Missing file means `default = data`. Read by the client files. The
README explains the risk of trusting a sender whose agent got
prompt-injected. The server does not read it.

## Release

CI on every push: `go test ./...`, `go vet ./...`. On tag `v*`: binaries for
linux amd64 and arm64 plus `SHA256SUMS` attached to a GitHub Release.

## Tasks

| # | Files | Verification | Who |
|---|---|---|---|
| 1 | Clean copy of the private repo, `LICENSE` | `grep -rniE --exclude=LICENSE --exclude=CONTRACT.md --exclude-dir=.git '<private names>' .` finds nothing, `go test ./...` | me |
| 2 | `*.go`, `mailbox/`, `go.mod` | `go test ./...` green; no Spanish left in `*.go` | codex |
| 3a | `main.go`, `config.go`, `admin/` (install, uninstall, units, drop-in), old `deploy/` units | `test/e2e.sh install` | codex |
| 3b | `admin/` (add, remove) | `test/e2e.sh add` | codex |
| 3c | `admin/` (doctor) | `test/e2e.sh doctor` | codex |
| 3d | `admin/` (backup), drop `deploy/*.py` | `test/e2e.sh backup` and retention unit tests ported from Python | codex |
| 4 | `install.sh`, `.github/workflows/` | tag on a fork produces the Release; `install.sh` installs it in the e2e VM | codex |
| 5 | `client/`, `README.md` | a fresh `claude -p` given only the README installs in the e2e VM and exchanges a message | me, prose shown first |

## Out

Docker, public HTTPS without tunnel, web UI, threads with more than two
participants, pagination, notifications, MCP registry listing, project domain.
