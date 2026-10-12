# Contract

What no task reopens. Starting point: the private `agentes-coordinados`
server, published here without history and in English, then moved from SSH
tunnels to its own SSH server.

## Language

English everywhere: code, comments, errors, MCP tool descriptions, docs and
client files. Nothing in the repo names a real person, host, IP or email,
except the copyright line in `LICENSE`.

## Stack

- Go 1.27, module `github.com/sarfios20/mcp-agent-mailbox`, one static binary
  `mcp-agent-mailbox` built with `CGO_ENABLED=0`. No Docker.
- Transport: the binary's own SSH server, `github.com/gliderlabs/ssh` over
  `golang.org/x/crypto/ssh`. MCP runs over the stdio of an SSH exec channel
  (`github.com/modelcontextprotocol/go-sdk` v1.8.0, IO transport). No HTTP.
- SQLite via `database/sql` + `modernc.org/sqlite`, WAL, no ORM.
- Runs on any OS Go builds for. `install` needs Linux with systemd; without
  it, `serve` is started by hand or any supervisor. Releases: linux amd64 and
  arm64.

## Files

| File | Responsibility |
|---|---|
| `mailbox/mailbox.go` | Schema and all mailbox operations, participants included. Knows nothing of MCP, SSH or the OS. |
| `config.go` | Every tunable value, together and commented. |
| `main.go` | Subcommand dispatch and the JSON output convention. |
| `tools.go` | The MCP tools. Translates between MCP and `mailbox`. |
| `ssh.go` | SSH server: host key, key auth, exec routing, limits. |
| `admin/` | `install`, `add`, `remove`, `doctor`, `backup`, `uninstall`. Embeds systemd units with `go:embed`. |
| `install.sh` | Downloads the release binary, checks SHA256, runs `install`. Nothing else. |
| `client/inbox.md`, `client/codex-inbox/SKILL.md` | `/inbox` for Claude Code and Codex. Both apply the trust file. |
| `README.md` | For humans and their agents: what it is, how it works, setup, trust, operations. An agent can install from it alone. |
| `test/e2e.sh` | End-to-end test on a throwaway Debian 12 VM (qemu + KVM). Never a privileged container. |

## Data and states

```sql
participants(name, public_key, added_at)          -- PK name, public_key UNIQUE
threads(id, subject, created_by, created_at)
thread_members(thread_id, participant, added_at)  -- PK (thread_id, participant)
messages(id, thread_id, sender, content, created_at)
receipts(message_id, participant, read_at NULL)   -- PK (message_id, participant)
```

- RFC 3339 UTC, set by the server. No migrations: there are no installs yet.
- Names match `^[a-z][a-z0-9-]{0,31}$`. A public key belongs to one
  participant at most. `public_key` is the authorized_keys form
  (`type base64`), without comment.
- A thread has 2 or more members. Members are only ever added, never removed.
- A new message gets one receipt per member except its sender, at insert time.
- A receipt is `unread` (`read_at IS NULL`) → `read`, only when its
  participant calls `read_thread`. One member reading never affects another.
- A member added later sees the whole history and gets receipts for messages
  sent after joining only.
- Removing a participant deletes their `participants` row only. Their
  messages, memberships and receipts stay.

The data directory (default `/var/lib/mcp-agent-mailbox`) holds `mailbox.db`,
`host_ed25519_key` (created on first `serve`, never replaced) and
`server.json` (`{"port": N}`, written by `serve` at start). Every process that
opens `mailbox.db` runs as the owner of the data directory: run as root, admin
subcommands switch to that user before opening it.

## MCP tools

- `send(to, subject, text)`: `to` is a list of names, or `["*"]` for every
  current participant except the sender, expanded at send time.
- `reply(thread_id, text)`: to every member except the sender.
- `read_thread(thread_id)`: marks the caller's receipts read. Each message
  carries `unread` for the caller. Thread carries `participants`.
- `inbox()`: threads with unread receipts for the caller, with `participants`.
- `add_to_thread(thread_id, names)`: any member adds participants.
- `participants()`: every participant name.

None accepts the sender: it comes from the SSH key. Errors: empty or
duplicate `to`, the sender in `to`, unknown names, a non-member reading,
replying or adding, adding someone already in the thread.
Size limits in `config.go`, checked in `tools.go`: message text 64 KiB,
subject 256 bytes.

## SSH server

- Listens on all interfaces, default port 2222. The SSH user name is ignored:
  the public key alone identifies the participant. Unknown keys are refused.
  Every tool call, and `wait` before it answers, checks that the key still
  belongs to that participant, so an open session dies with `remove`.
- Only public key auth. Key types: ed25519, ecdsa, rsa of 2048 bits or more.
- Only `session` channels with an `exec` request are accepted. Commands:
  - `mcp`: an MCP session over the channel's stdin/stdout as that participant.
  - `wait`: blocks until the participant has unread receipts, prints
    `{"unread": n}` and exits 0. Woken in-process when a message is stored.
  - Anything else, no command, shell, pty, subsystems, env, port or agent
    forwarding: refused, exit status 1 and a one-line reason on stderr.
- Limits in `config.go`: failed auth attempts per IP per minute, concurrent
  sessions per participant, handshake timeout, server keepalive interval.

## CLI

Every subcommand is non-interactive, prints one JSON object on stdout, and on
failure prints `{"error": "...", "detail": ...}` on stderr. Exit 0 ok, 1 failed,
2 bad usage. Admin subcommands are idempotent. `--data <dir>` (default
`/var/lib/mcp-agent-mailbox`) is accepted by `serve`, `add`, `remove` and
`backup`.

- `serve [--data DIR] [--port N]`: the server. Creates the host key and
  database if missing, writes `server.json`. Refuses a data directory that
  group or others can access.
- `install [--port N]`: root and systemd required (else the error says to run
  `serve` by hand). Fails naming the process if the port is taken. System user
  `mailbox`, data directory owned by it, backups in
  `/var/backups/mcp-agent-mailbox` owned by it, units
  `mcp-agent-mailbox.service`, `mcp-agent-mailbox-backup.service` and `.timer`
  (both services run as `mailbox`), enabled and started. Copies itself to
  `/usr/local/bin/mcp-agent-mailbox`.
- `add <name> --pubkey <file|-> --host <host>`: registers the key. Same name
  and key again changes nothing. Name or key already used otherwise: error.
  Output: `name`, and `client` with ready-to-paste strings `known_hosts` (the
  line for `[host]:port`), `claude_code` (the `claude mcp add` command),
  `codex_toml`, `wait` (the `ssh ... wait` command). Client commands use
  `ssh -p <port> -i ~/.ssh/mcp-agent-mailbox -o IdentitiesOnly=yes mailbox@<host>`.
- `remove <name>`: deletes the participant. Output `{"removed": bool, "name"}`.
- `doctor`: prints `{"ok": bool, "checks": [{"name", "pass", "detail"}]}` on
  stdout and exits 1 if any check fails (the one exception to the error
  convention). Checks: service active, port listening, SSH handshake on
  localhost presents the stored host key, backup timer active, last backup
  younger than 48 h (or none yet and installed less than 48 h ago).
- `backup`: verified SQLite snapshot, retention 3 days, 2 weeks, 12 months,
  1 year, never deletes unverified files.
- `uninstall`: stops and removes the units. Keeps the `mailbox` user, data and
  backups, and says where they are.

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
linux amd64 and arm64, `SHA256SUMS` and `install.sh` attached to a GitHub
Release.

## Tasks

Done: 1 clean copy, 2 English, 3a-3d admin subcommands, 4 releases, 6 N-member
threads, 7 `add_to_thread` (all on the tunnel design).

| # | Files | Verification | Who |
|---|---|---|---|
| 8 | `main.go`, `tools.go`, `ssh.go`, `config.go`, `main_test.go`, `mailbox/`, `admin/` (add, remove, install unit), `go.mod`, `go.sum` | `test/e2e.sh add`, `go test ./...` | codex |
| 9 | `admin/` (drop sshd drop-in, tunnel users, OS check, `/etc` config; doctor, uninstall, backup as `mailbox`) | `test/e2e.sh backup`, `go test ./...` | codex |
| 10 | `ssh.go`, `mailbox/` (`wait`) | `test/e2e.sh backup` with the wait checks | codex |
| 11 | `client/`, `README.md` | a fresh `claude -p` given only the README installs in the e2e VM and exchanges a message | me, prose shown first |

## Out

Invite/join flow, Docker, HTTPS, Codex wait loop, web UI, removing thread
members, participant descriptions, pagination, MCP registry listing, project
domain.

Accepted for now, since every participant is a key someone added: no storage
quota per participant, no cap on pipelined requests in one MCP session, no
cap on authenticated connections that open no channel, and `send` validating
recipients just before its transaction (a participant removed in between
still gets the thread). `backup --data` with a non-default directory shares
`/var/backups/mcp-agent-mailbox` with the default one.
