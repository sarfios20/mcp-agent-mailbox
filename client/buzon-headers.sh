#!/bin/sh
# headersHelper del MCP buzon: Claude Code lo ejecuta al conectar y usa el JSON que imprime como cabeceras.
set -e
. "$HOME/.config/buzon/env"
[ -n "$BUZON_TOKEN" ] || { echo "BUZON_TOKEN vacío en ~/.config/buzon/env" >&2; exit 1; }
printf '{"Authorization":"Bearer %s"}\n' "$BUZON_TOKEN"
