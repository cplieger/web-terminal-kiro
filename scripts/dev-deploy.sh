#!/usr/bin/env bash
# Deploy the locally-built web-terminal-kiro-dev-bin into the web-terminal-kiro-dev
# container on the dev box (the rebuild experiment target, host port 9849) by
# swapping the binary and restarting — no image rebuild, no GitHub Actions. Pairs
# with scripts/dev-build.sh. Leaves prod web-terminal-kiro (9848) untouched.
#
# Set DEPLOY_HOST to your dev box (an ssh host alias or IP).
set -euo pipefail
cd "$(dirname "$0")/.."
HOST="${DEPLOY_HOST:?set DEPLOY_HOST to your dev box (ssh host or IP)}"
BIN="web-terminal-kiro-dev-bin"
[ -f "$BIN" ] || {
  echo "missing $BIN — run scripts/dev-build.sh first" >&2
  exit 1
}

echo "scp -> ${HOST}:/tmp/$BIN"
scp -q "$BIN" "${HOST}:/tmp/$BIN"
echo "docker cp + restart web-terminal-kiro-dev"
# $BIN is a fixed local constant (not user input); expanding it client-side into
# the remote command is intentional — SC2029's client-vs-server caveat is moot here.
# shellcheck disable=SC2029
ssh "$HOST" "sudo docker cp /tmp/$BIN web-terminal-kiro-dev:/app/web-terminal-kiro && sudo docker restart web-terminal-kiro-dev"
sleep 6
code=$(curl -sf -m5 -o /dev/null -w "%{http_code}" "http://${HOST}:9849/api/health" || echo "ERR")
echo "web-terminal-kiro-dev health: $code  (UI: http://${HOST}:9849/)"
