#!/usr/bin/env bash
# End-to-end test on a throwaway Debian 12 VM (qemu + KVM, user-mode network).
#
#   test/e2e.sh <stage>     stage: install | add | doctor | backup
#
# Stages are cumulative: `add` also runs the `install` checks, and so on.
# The VM never sees the host's /dev or cgroups; it only exposes its SSH port
# and mailbox ports on 127.0.0.1. The cloud image is cached in ~/.cache/mcp-agent-mailbox-e2e.
# Needs: qemu-system-x86_64, qemu-img, genisoimage, curl, jq, ssh, go.
# Prints PASS on success; exits non-zero on the first failed check.
#
# E2E_VIA_INSTALL_SH=1 installs through install.sh instead of copying the
# binary: the host serves install.sh, the binary and SHA256SUMS (laid out like
# a GitHub Release) on 127.0.0.1:$E2E_HTTP_PORT, which the VM sees as 10.0.2.2.
set -euo pipefail

STAGE=${1:?usage: test/e2e.sh install|add|doctor|backup}
case $STAGE in install) LEVEL=1 ;; add) LEVEL=2 ;; doctor) LEVEL=3 ;; backup) LEVEL=4 ;;
  *) echo "unknown stage: $STAGE" >&2; exit 2 ;; esac

IMAGE_URL=https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2
CACHE=${XDG_CACHE_HOME:-$HOME/.cache}/mcp-agent-mailbox-e2e
SSH_PORT=${E2E_SSH_PORT:-2222}
HTTP_PORT=${E2E_HTTP_PORT:-18800}
MAILBOX_PORT=${E2E_MAILBOX_PORT:-2223}
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

# mbxssh <who> <ssh args...>: ssh to the mailbox's own SSH server with <who>'s
# key, checking its host key against the known_hosts line printed by `add`.
mbxssh() {
  local who=$1; shift
  ssh -o LogLevel=ERROR -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=5 \
      -o StrictHostKeyChecking=yes -o UserKnownHostsFile="$W/known_hosts" \
      -o HostKeyAlias='[e2e-vps]:2222' -p "$MAILBOX_PORT" -i "$W/$who" "$@"
}
# mbx <who> [command]: run a mailbox command as <who>.
mbx() { local who=$1; shift; mbxssh "$who" mailbox@127.0.0.1 "$@"; }
# call <who> <tool> <json args>: one MCP tool call over `ssh ... mcp`; prints its JSON-RPC response.
call() {
  { printf '%s\n' \
      '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e","version":"0"}}}' \
      '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
      "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"$2\",\"arguments\":$3}}"
    sleep 1; } | mbx "$1" mcp | jq -c 'select(.id == 2)'
}
# sc: the tool result; iserr: succeeds if the call failed.
sc() { jq -c .result.structuredContent; }
iserr() { jq -e '(.error != null) or (.result.isError == true)' >/dev/null; }

step "build"
(cd "$REPO" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$W/mcp-agent-mailbox" .)

step "boot VM"
mkdir -p "$CACHE"
[ -f "$CACHE/base.qcow2" ] || curl -fsSL -o "$CACHE/base.qcow2" "$IMAGE_URL"
qemu-img create -q -f qcow2 -b "$CACHE/base.qcow2" -F qcow2 "$W/disk.qcow2" 4G
for k in admin alice bob carol dave mallory; do ssh-keygen -q -t ed25519 -N '' -f "$W/$k"; done
mkdir "$W/seed"
printf 'instance-id: e2e\nlocal-hostname: e2e\n' > "$W/seed/meta-data"
printf '#cloud-config\nssh_authorized_keys:\n  - %s\n' "$(cat "$W/admin.pub")" > "$W/seed/user-data"
genisoimage -quiet -output "$W/seed.iso" -volid cidata -joliet -rock "$W/seed/user-data" "$W/seed/meta-data"
qemu-system-x86_64 -enable-kvm -cpu host -m 1024 -smp 2 -display none \
  -serial "file:$W/console.log" -daemonize -pidfile "$W/qemu.pid" \
  -drive "file=$W/disk.qcow2,if=virtio" -drive "file=$W/seed.iso,if=virtio,media=cdrom" \
  -nic "user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:$SSH_PORT-:22,hostfwd=tcp:127.0.0.1:$MAILBOX_PORT-:2222"
for _ in $(seq 90); do vm true 2>/dev/null && break; sleep 2; done
vm true || fail "VM did not come up (console: $W/console.log)"
vm 'cloud-init status --wait >/dev/null; command -v curl >/dev/null && command -v sqlite3 >/dev/null || (sudo apt-get -qq update && sudo apt-get -qq install -y curl sqlite3 >/dev/null)'
if [ "${E2E_VIA_INSTALL_SH:-0}" = 1 ]; then
  step "install.sh"
  mkdir "$W/release"
  cp "$W/mcp-agent-mailbox" "$W/release/mcp-agent-mailbox-linux-amd64"
  cp "$REPO/install.sh" "$W/release/install.sh"
  (cd "$W/release" && sha256sum mcp-agent-mailbox-linux-amd64 > SHA256SUMS)
  python3 -m http.server -b 127.0.0.1 -d "$W/release" "$HTTP_PORT" >/dev/null 2>&1 & PIDS+=($!)
  sleep 1
  vm "curl -fsSL http://10.0.2.2:$HTTP_PORT/install.sh | sudo env MCP_AGENT_MAILBOX_BASE_URL=http://10.0.2.2:$HTTP_PORT sh" > "$W/installsh.out" \
    || fail "install.sh failed: $(cat "$W/installsh.out")"
  tail -1 "$W/installsh.out" | jq -e '.installed' >/dev/null || fail "install.sh did not end with install's JSON"
  echo corrupted >> "$W/release/mcp-agent-mailbox-linux-amd64"
  vm "curl -fsSL http://10.0.2.2:$HTTP_PORT/install.sh | sudo env MCP_AGENT_MAILBOX_BASE_URL=http://10.0.2.2:$HTTP_PORT sh" >/dev/null 2>&1 \
    && fail "install.sh accepted a binary with a wrong checksum"
else
  scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -P "$SSH_PORT" -i "$W/admin" -q "$W/mcp-agent-mailbox" debian@127.0.0.1:
  vm 'sudo install -m755 mcp-agent-mailbox /usr/local/bin/mcp-agent-mailbox'
fi

# ---------------------------------------------------------------- install
step "install"
vm 'mcp-agent-mailbox install' >/dev/null 2>"$W/err" && fail "install as non-root succeeded"
jq -e .error "$W/err" >/dev/null || fail "non-root error is not JSON: $(cat "$W/err")"
vm 'sudo mcp-agent-mailbox install' | jq -e . >/dev/null || fail "install output is not JSON"
vm 'sudo mcp-agent-mailbox install' | jq -e . >/dev/null || fail "second install (idempotent) failed"
vm 'systemctl is-active mcp-agent-mailbox' | grep -qx active || fail "service not active"
vm 'systemctl is-active mcp-agent-mailbox-backup.timer' | grep -qx active || fail "backup timer not active"
vm "sudo ss -ltnH 'sport = :2222'" | grep -q 2222 || fail "not listening on 2222"
vm 'sudo test -f /var/lib/mcp-agent-mailbox/host_ed25519_key' || fail "no host key"
ssh -o LogLevel=ERROR -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -p "$MAILBOX_PORT" -i "$W/mallory" mailbox@127.0.0.1 mcp </dev/null 2>/dev/null && fail "unknown key accepted"

if [ $LEVEL -ge 2 ]; then
# -------------------------------------------------------------------- add
step "add"
add() { vm "sudo mcp-agent-mailbox add $1 --pubkey - --host e2e-vps" < "$W/$1.pub"; }
add alice > "$W/alice.json" || fail "add alice failed"
add bob > "$W/bob.json" || fail "add bob failed"
add carol > "$W/carol.json" || fail "add carol failed"
for k in client.known_hosts client.claude_code client.codex_toml client.wait; do
  jq -e ".$k | strings | length > 0" "$W/alice.json" >/dev/null || fail "add output lacks $k"
done
jq -r .client.known_hosts "$W/alice.json" > "$W/known_hosts"
grep -q '^\[e2e-vps\]:2222 ssh-ed25519 ' "$W/known_hosts" || fail "known_hosts line is not for [e2e-vps]:2222: $(cat "$W/known_hosts")"
add alice | jq -e '.name == "alice"' >/dev/null || fail "re-adding alice with the same key failed"
vm 'sudo mcp-agent-mailbox add dave --pubkey - --host e2e-vps' < "$W/alice.pub" >/dev/null 2>&1 && fail "one key accepted for two participants"
vm 'sudo mcp-agent-mailbox add Bad_Name --pubkey - --host e2e-vps' < "$W/mallory.pub" >/dev/null 2>&1 && fail "invalid name accepted"

mbx alice </dev/null >/dev/null 2>&1 && fail "session without command accepted"
mbx alice ls </dev/null >/dev/null 2>&1 && fail "unknown command accepted"
mbxssh alice -tt mailbox@127.0.0.1 mcp </dev/null >/dev/null 2>&1 && fail "pty accepted"
mbxssh alice -N -o ExitOnForwardFailure=yes -R 18891:127.0.0.1:22 mailbox@127.0.0.1 </dev/null >/dev/null 2>&1 & fp=$!
sleep 3; kill -0 $fp 2>/dev/null && { kill $fp; fail "remote port forwarding accepted"; }
# A local forward is only requested when something connects to it: connect and
# check that the VM's own sshd banner never comes back.
mbxssh alice -N -L 18890:127.0.0.1:22 mailbox@127.0.0.1 </dev/null >/dev/null 2>&1 & fp=$!; PIDS+=($fp)
sleep 2
banner=$(timeout 3 bash -c 'exec 3<>/dev/tcp/127.0.0.1/18890 && head -c 4 <&3' 2>/dev/null || true)
kill $fp 2>/dev/null || true
[ "$banner" = "SSH-" ] && fail "local port forwarding accepted"

# Thread 1: alice -> bob.
call alice participants '{}' | sc | jq -e '.participants | sort == ["alice","bob","carol"]' >/dev/null || fail "participants is not alice, bob, carol"
call alice send '{"to":["bob"],"subject":"e2e","text":"hello bob"}' | sc | jq -e '.thread_id == 1' >/dev/null || fail "alice could not send"
call bob inbox '{}' | sc | jq -e '.threads[0].participants | sort == ["alice","bob"]' >/dev/null || fail "bob does not see alice's thread"
call bob read_thread '{"thread_id":1}' | sc | jq -e '.thread.messages[0] | .content == "hello bob" and .unread == true' >/dev/null || fail "bob cannot read the new message"
call bob read_thread '{"thread_id":1}' | sc | jq -e '.thread.messages[0].unread == false' >/dev/null || fail "message still unread after reading"
call carol read_thread '{"thread_id":1}' | iserr || fail "carol read a thread she is not in"

# Thread 2: alice -> bob, carol; bob replies to both.
call alice send '{"to":["bob","carol"],"subject":"group","text":"hello both"}' | sc | jq -e '.thread_id == 2' >/dev/null || fail "group send failed"
call bob reply '{"thread_id":2,"text":"bob here"}' | sc | jq -e '.message_id' >/dev/null || fail "bob could not reply"
call carol inbox '{}' | sc | jq -e '.threads[] | select(.thread_id == 2) | .unread == 2' >/dev/null || fail "carol does not have 2 unread in the group"
call alice inbox '{}' | sc | jq -e '.threads[] | select(.thread_id == 2) | .unread == 1' >/dev/null || fail "alice does not have bob's reply unread"
call carol read_thread '{"thread_id":2}' >/dev/null
call bob inbox '{}' | sc | jq -e '.threads[] | select(.thread_id == 2) | .unread == 1' >/dev/null || fail "carol reading changed bob's unread"

# Thread 3: alice -> everyone.
call alice send '{"to":["*"],"subject":"all","text":"hello all"}' | sc | jq -e '.thread_id == 3' >/dev/null || fail "send to * failed"
call carol read_thread '{"thread_id":3}' | sc | jq -e '.thread.participants | sort == ["alice","bob","carol"]' >/dev/null || fail "* did not reach everyone"

# add_to_thread: carol joins thread 1 late.
call carol add_to_thread '{"thread_id":1,"names":["carol"]}' | iserr || fail "non-member added someone"
call alice add_to_thread '{"thread_id":1,"names":["carol"]}' | sc | jq -e . >/dev/null || fail "alice could not add carol"
call alice add_to_thread '{"thread_id":1,"names":["carol"]}' | iserr || fail "adding an existing member accepted"
call carol read_thread '{"thread_id":1}' | sc | jq -e '.thread.messages[0] | .content == "hello bob" and .unread == false' >/dev/null || fail "late member does not see history as read"
call alice reply '{"thread_id":1,"text":"welcome carol"}' >/dev/null
call carol inbox '{}' | sc | jq -e '.threads[] | select(.thread_id == 1) | .unread == 1' >/dev/null || fail "late member does not get new messages"

# wait: returns at once if something is unread; otherwise blocks until a message arrives.
timeout 5 bash -c "$(declare -f mbx mbxssh); W='$W' MAILBOX_PORT=$MAILBOX_PORT mbx carol wait </dev/null" | jq -e '.unread >= 1' >/dev/null || fail "wait did not return at once with unread mail"
call carol read_thread '{"thread_id":1}' >/dev/null
call carol inbox '{}' | sc | jq -e '.threads | length == 0' >/dev/null || fail "carol still has unread mail before the wait test"
mbx carol wait </dev/null > "$W/wait.out" 2>&1 & wp=$!; PIDS+=($wp)
sleep 3
kill -0 $wp 2>/dev/null || fail "wait returned with nothing unread: $(cat "$W/wait.out")"
call alice send '{"to":["carol"],"subject":"ping","text":"wake up"}' >/dev/null
for _ in 1 2 3 4 5 6; do kill -0 $wp 2>/dev/null || break; sleep 0.5; done
kill -0 $wp 2>/dev/null && fail "wait did not wake within 3 s of a new message"
wait $wp || fail "wait exited non-zero: $(cat "$W/wait.out")"
jq -e '.unread == 1' "$W/wait.out" >/dev/null || fail "wait output is not {\"unread\": 1}: $(cat "$W/wait.out")"

call alice send '{"to":[],"subject":"x","text":"x"}' | iserr || fail "empty to accepted"
call alice send '{"to":["bob","bob"],"subject":"x","text":"x"}' | iserr || fail "duplicate to accepted"
call alice send '{"to":["alice"],"subject":"x","text":"x"}' | iserr || fail "sending to yourself accepted"
call alice send '{"to":["nobody"],"subject":"x","text":"x"}' | iserr || fail "unknown recipient accepted"

# invite/join: dave joins with a key nobody registered by hand.
mbx mallory join nothing-pending </dev/null >/dev/null 2>&1 && fail "unknown key got a session with no invite pending"
vm 'sudo mcp-agent-mailbox invite alice --host e2e-vps' >/dev/null 2>&1 && fail "invite for an existing participant accepted"
vm 'sudo mcp-agent-mailbox invite dave --host e2e-vps' > "$W/invite.json" || fail "invite dave failed"
jq -e --arg kh "$(cat "$W/known_hosts")" '.name == "dave" and .expires_at and .client.known_hosts == $kh' "$W/invite.json" >/dev/null \
  || fail "invite output lacks name, expires_at or the same known_hosts line: $(cat "$W/invite.json")"
jq -r .client.join "$W/invite.json" | grep -q -- '-i ~/.ssh/mcp-agent-mailbox -o IdentitiesOnly=yes mailbox@e2e-vps join ' || fail "join command is not the add-style ssh command"
token=$(jq -r .client.join "$W/invite.json" | awk '{print $NF}')
mbx mallory mcp </dev/null >/dev/null 2>&1 && fail "unregistered key got mcp while an invite is pending"
mbx mallory wait </dev/null >/dev/null 2>&1 && fail "unregistered key got wait while an invite is pending"
mbx mallory join wrong-token </dev/null >/dev/null 2>&1 && fail "wrong invite token accepted"
mbx alice join "$token" </dev/null >/dev/null 2>&1 && fail "a registered key used an invite"
mbx dave join "$token" </dev/null > "$W/dave.json" || fail "dave could not join"
jq -e '.name == "dave" and (.client.claude_code | endswith("mailbox@e2e-vps mcp")) and (.client.wait | endswith("mailbox@e2e-vps wait"))' "$W/dave.json" >/dev/null \
  || fail "join output is not add's output: $(cat "$W/dave.json")"
mbx mallory join "$token" </dev/null >/dev/null 2>&1 && fail "used invite accepted again"
call dave participants '{}' | sc | jq -e '.participants | index("dave")' >/dev/null || fail "dave cannot use the mailbox after joining"
t1=$(vm 'sudo mcp-agent-mailbox invite erin --host e2e-vps' | jq -r .client.join | awk '{print $NF}')
t2=$(vm 'sudo mcp-agent-mailbox invite erin --host e2e-vps' | jq -r .client.join | awk '{print $NF}')
mbx mallory join "$t1" </dev/null >/dev/null 2>&1 && fail "replaced invite accepted"
vm 'sudo mcp-agent-mailbox remove erin' | jq -e '.removed == true' >/dev/null || fail "remove did not report the pending invite"
mbx mallory join "$t2" </dev/null >/dev/null 2>&1 && fail "invite accepted after remove"
mbx mallory mcp </dev/null >/dev/null 2>&1 && fail "unknown key accepted with no invite pending"

vm 'sudo mcp-agent-mailbox remove bob' | jq -e '.removed == true' >/dev/null || fail "remove bob failed"
vm 'sudo mcp-agent-mailbox remove bob' | jq -e '.removed == false' >/dev/null || fail "second remove is not idempotent"
mbx bob mcp </dev/null >/dev/null 2>&1 && fail "removed bob can still connect"
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
vm "sudo sh -c 'cd /var/backups/mcp-agent-mailbox && f=\$(ls *.sqlite | tail -1) && sha256sum --check --quiet \"\${f%.sqlite}.sha256\" && [ \"\$(sqlite3 \"\$f\" \"select count(*) from messages\")\" = \"\$(sqlite3 /var/lib/mcp-agent-mailbox/mailbox.db \"select count(*) from messages\")\" ]'" \
  || fail "backup missing, checksum wrong or messages lost"
vm 'sudo find /var/lib/mcp-agent-mailbox /var/backups/mcp-agent-mailbox ! -user mailbox' | grep . && fail "files not owned by mailbox"
fi

# -------------------------------------------------------------- uninstall
step "uninstall"
vm 'sudo mcp-agent-mailbox uninstall' | jq -e . >/dev/null || fail "uninstall failed"
vm 'systemctl list-unit-files | grep -q mcp-agent-mailbox' && fail "units still installed"
vm 'sudo test -f /var/lib/mcp-agent-mailbox/mailbox.db' || fail "uninstall deleted the data"
vm 'test -e /etc/ssh/sshd_config.d/10-mcp-agent-mailbox.conf' && fail "system sshd still has a mailbox drop-in"
vm 'sudo sshd -t' || fail "system sshd config invalid after uninstall"

echo PASS
