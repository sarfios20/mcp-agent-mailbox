#!/usr/bin/env bash
# End-to-end test on a throwaway Debian 12 VM (qemu + KVM, user-mode network).
#
#   test/e2e.sh <stage>     stage: install | add | doctor | backup
#
# Stages are cumulative: `add` also runs the `install` checks, and so on.
# The VM never sees the host's /dev or cgroups; it only exposes its SSH port
# on 127.0.0.1. The cloud image is cached in ~/.cache/mcp-agent-mailbox-e2e.
# Needs: qemu-system-x86_64, qemu-img, genisoimage, curl, jq, ssh, go.
# Prints PASS on success; exits non-zero on the first failed check.
set -euo pipefail

STAGE=${1:?usage: test/e2e.sh install|add|doctor|backup}
case $STAGE in install) LEVEL=1 ;; add) LEVEL=2 ;; doctor) LEVEL=3 ;; backup) LEVEL=4 ;;
  *) echo "unknown stage: $STAGE" >&2; exit 2 ;; esac

IMAGE_URL=https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2
CACHE=${XDG_CACHE_HOME:-$HOME/.cache}/mcp-agent-mailbox-e2e
SSH_PORT=${E2E_SSH_PORT:-2222}
TUNNEL_A=18801
TUNNEL_B=18802
REPO=$(cd "$(dirname "$0")/.." && pwd)
W=$(mktemp -d)
PIDS=()

cleanup() {
  for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null || true; done
  [ -f "$W/qemu.pid" ] && kill "$(cat "$W/qemu.pid")" 2>/dev/null || true
  rm -rf "$W"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo "== $*"; }

SSHO=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR
      -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=5 -p "$SSH_PORT")
vm() { ssh "${SSHO[@]}" -i "$W/admin" debian@127.0.0.1 "$@"; }

# mcp <port> <token> <json-rpc body>: one MCP call through a tunnel.
mcp() {
  curl -sS -H "Authorization: Bearer $2" -H 'Content-Type: application/json' \
    -H 'Accept: application/json, text/event-stream' -d "$3" "http://127.0.0.1:$1/mcp"
}
call() { mcp "$1" "$2" "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"$3\",\"arguments\":$4}}"; }

step "build"
(cd "$REPO" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$W/mcp-agent-mailbox" .)

step "boot VM"
mkdir -p "$CACHE"
[ -f "$CACHE/base.qcow2" ] || curl -fsSL -o "$CACHE/base.qcow2" "$IMAGE_URL"
qemu-img create -q -f qcow2 -b "$CACHE/base.qcow2" -F qcow2 "$W/disk.qcow2" 4G
for k in admin alice bob; do ssh-keygen -q -t ed25519 -N '' -f "$W/$k"; done
mkdir "$W/seed"
printf 'instance-id: e2e\nlocal-hostname: e2e\n' > "$W/seed/meta-data"
printf '#cloud-config\nssh_authorized_keys:\n  - %s\n' "$(cat "$W/admin.pub")" > "$W/seed/user-data"
genisoimage -quiet -output "$W/seed.iso" -volid cidata -joliet -rock "$W/seed/user-data" "$W/seed/meta-data"
qemu-system-x86_64 -enable-kvm -cpu host -m 1024 -smp 2 -display none \
  -serial "file:$W/console.log" -daemonize -pidfile "$W/qemu.pid" \
  -drive "file=$W/disk.qcow2,if=virtio" -drive "file=$W/seed.iso,if=virtio,media=cdrom" \
  -nic "user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:$SSH_PORT-:22"
for _ in $(seq 90); do vm true 2>/dev/null && break; sleep 2; done
vm true || fail "VM did not come up (console: $W/console.log)"
vm 'cloud-init status --wait >/dev/null; command -v curl >/dev/null && command -v sqlite3 >/dev/null || (sudo apt-get -qq update && sudo apt-get -qq install -y curl sqlite3 >/dev/null)'
scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -P "$SSH_PORT" -i "$W/admin" -q "$W/mcp-agent-mailbox" debian@127.0.0.1:
vm 'sudo install -m755 mcp-agent-mailbox /usr/local/bin/mcp-agent-mailbox'

# ---------------------------------------------------------------- install
step "install"
vm 'mcp-agent-mailbox install' >/dev/null 2>"$W/err" && fail "install as non-root succeeded"
jq -e .error "$W/err" >/dev/null || fail "non-root error is not JSON: $(cat "$W/err")"
vm 'sudo mcp-agent-mailbox install' | jq -e . >/dev/null || fail "install output is not JSON"
vm 'sudo mcp-agent-mailbox install' | jq -e . >/dev/null || fail "second install (idempotent) failed"
vm 'systemctl is-active mcp-agent-mailbox' | grep -qx active || fail "service not active"
vm 'systemctl is-active mcp-agent-mailbox-backup.timer' | grep -qx active || fail "backup timer not active"
listen=$(vm "sudo ss -ltnH 'sport = :8765'")
echo "$listen" | grep -q '127.0.0.1:8765' || fail "not listening on 127.0.0.1:8765: $listen"
echo "$listen" | grep -qvE '127\.0\.0\.1:8765' && fail "also listening elsewhere: $listen"
code=$(vm "curl -s -o /dev/null -w '%{http_code}' -X POST http://127.0.0.1:8765/mcp")
[ "$code" = 401 ] || fail "no token gave $code, want 401"
vm 'test -f /etc/ssh/sshd_config.d/10-mcp-agent-mailbox.conf && sudo sshd -t' || fail "sshd drop-in missing or invalid"
vm 'sudo grep -q "^Match Group mailbox-tunnel" /etc/ssh/sshd_config.d/10-mcp-agent-mailbox.conf' || fail "drop-in does not match Group mailbox-tunnel"

if [ $LEVEL -ge 2 ]; then
# -------------------------------------------------------------------- add
step "add"
add() { vm "sudo mcp-agent-mailbox add $1 --pubkey - --host e2e-vps" < "$W/$1.pub"; }
add alice > "$W/alice.json" || fail "add alice failed"
add bob > "$W/bob.json" || fail "add bob failed"
for k in token ssh_user client.ssh_config client.tunnel_systemd client.tunnel_launchd \
         client.tunnel_windows client.claude_code client.codex_toml; do
  jq -e ".$k | strings | length > 0" "$W/alice.json" >/dev/null || fail "add output lacks $k"
done
TA=$(jq -r .token "$W/alice.json"); TB=$(jq -r .token "$W/bob.json")
[[ $TA =~ ^[0-9a-f]{64}$ ]] || fail "token is not 64 hex: $TA"
[ "$(add alice | jq -r .token)" = "$TA" ] || fail "re-adding alice changed her token"
vm 'sudo mcp-agent-mailbox add Bad_Name --pubkey - --host x' < "$W/bob.pub" >/dev/null 2>&1 && fail "invalid name accepted"

ssh "${SSHO[@]}" -i "$W/alice" mailbox-alice@127.0.0.1 true 2>/dev/null && fail "tunnel user got a shell"
ssh "${SSHO[@]}" -i "$W/alice" -N -o ExitOnForwardFailure=yes -L "$TUNNEL_A:127.0.0.1:8765" mailbox-alice@127.0.0.1 & PIDS+=($!)
ssh "${SSHO[@]}" -i "$W/bob" -N -o ExitOnForwardFailure=yes -L "$TUNNEL_B:127.0.0.1:8765" mailbox-bob@127.0.0.1 & PIDS+=($!)
sleep 3
call $TUNNEL_A "$TA" send '{"to":"bob","subject":"e2e","text":"hello bob"}' | jq -e '.result.structuredContent.thread_id' >/dev/null || fail "alice could not send"
call $TUNNEL_B "$TB" inbox '{}' | jq -e '.result.structuredContent.threads[0].with == "alice"' >/dev/null || fail "bob does not see alice's thread"
call $TUNNEL_B "$TB" read_thread '{"thread_id":1}' | jq -e '.result.structuredContent.thread.messages[0].content == "hello bob"' >/dev/null || fail "bob cannot read the message"
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:$TUNNEL_A/mcp")
[ "$code" = 401 ] || fail "no token through tunnel gave $code, want 401"

vm 'sudo mcp-agent-mailbox remove bob' | jq -e . >/dev/null || fail "remove bob failed"
sleep 2
code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TB" -X POST "http://127.0.0.1:$TUNNEL_A/mcp")
[ "$code" = 401 ] || fail "removed bob's token gave $code, want 401"
ssh "${SSHO[@]}" -i "$W/bob" -N -o ExitOnForwardFailure=yes -L 18803:127.0.0.1:8765 mailbox-bob@127.0.0.1 -o ConnectTimeout=5 &
bp=$!; sleep 3; kill -0 $bp 2>/dev/null && { kill $bp; fail "removed bob can still open a tunnel"; }
fi

if [ $LEVEL -ge 3 ]; then
# ----------------------------------------------------------------- doctor
step "doctor"
vm 'sudo mcp-agent-mailbox doctor' > "$W/doctor.json" || fail "doctor failed on a healthy install: $(cat "$W/doctor.json")"
jq -e '.checks | length > 0 and all(.pass)' "$W/doctor.json" >/dev/null || fail "doctor reports a failing check"
vm 'sudo systemctl stop mcp-agent-mailbox'
vm 'sudo mcp-agent-mailbox doctor' > "$W/doctor.json" && fail "doctor passed with the service stopped"
jq -e '[.checks[] | select(.pass | not)] | length > 0' "$W/doctor.json" >/dev/null || fail "doctor exit 1 but no failing check listed"
vm 'sudo systemctl start mcp-agent-mailbox'
fi

if [ $LEVEL -ge 4 ]; then
# ----------------------------------------------------------------- backup
step "backup"
vm 'sudo mcp-agent-mailbox backup' | jq -e . >/dev/null || fail "backup failed"
vm "sudo sh -c 'cd /var/backups/mcp-agent-mailbox && f=\$(ls *.sqlite | tail -1) && sha256sum --check --quiet \"\${f%.sqlite}.sha256\" && sqlite3 \"\$f\" \"select count(*) from messages\" | grep -qx 1'" \
  || fail "backup missing, checksum wrong or messages lost"
fi

# -------------------------------------------------------------- uninstall
step "uninstall"
vm 'sudo mcp-agent-mailbox uninstall' | jq -e . >/dev/null || fail "uninstall failed"
vm 'systemctl list-unit-files | grep -q mcp-agent-mailbox' && fail "units still installed"
vm 'test -e /etc/ssh/sshd_config.d/10-mcp-agent-mailbox.conf' && fail "sshd drop-in still there"
vm 'sudo sshd -t' || fail "sshd config invalid after uninstall"
vm 'sudo test -d /var/lib/mcp-agent-mailbox' || fail "uninstall deleted the data"

echo PASS
