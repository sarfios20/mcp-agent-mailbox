#!/bin/sh
set -eu

# Release asset location; override to install from another release source.
BASE_URL=${MCP_AGENT_MAILBOX_BASE_URL:-https://github.com/sarfios20/mcp-agent-mailbox/releases/latest/download}

if [ "$(id -u)" -ne 0 ]; then
    echo "install.sh requires root; run it with sudo sh." >&2
    exit 1
fi

machine=$(uname -m)
case "$machine" in
    x86_64) arch=amd64 ;;
    aarch64) arch=arm64 ;;
    *) echo "Unsupported architecture: $machine" >&2; exit 1 ;;
esac

workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' 0
trap 'exit 1' HUP INT TERM
binary=mcp-agent-mailbox-linux-$arch
curl -fsSL "$BASE_URL/$binary" -o "$workdir/$binary"
curl -fsSL "$BASE_URL/SHA256SUMS" -o "$workdir/SHA256SUMS"
cd "$workdir"

# Exactly one SHA256SUMS line must name the binary, and its digest must match.
expected=$(awk -v binary="$binary" '
    $2 == binary || $2 == "*" binary { count++; digest = $1 }
    END { if (count == 1) print digest }
' SHA256SUMS)
actual=$(sha256sum "$binary" | cut -d ' ' -f 1)
if [ -z "$expected" ] || [ "$expected" != "$actual" ]; then
    echo "Checksum verification failed for $binary: SHA256SUMS has '$expected', the download is $actual." >&2
    exit 1
fi

chmod +x "$binary"
# Keep the parent shell to clean up after the exec'd installer exits.
(exec "$workdir/$binary" install "$@")
