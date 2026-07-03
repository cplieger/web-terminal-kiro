#!/usr/bin/env bash
# Deploy the locally-built vibecli-dev-bin into the vibecli-dev container on
# the dev box (the rebuild experiment target, host port 9849) by swapping the
# binary and restarting — no image rebuild, no GitHub Actions. Pairs with
# scripts/dev-build.sh. Leaves prod vibecli (9848) untouched.
#
# Set DEPLOY_HOST to your dev box (an ssh host alias or IP).
set -euo pipefail
cd "$(dirname "$0")/.."
HOST="${DEPLOY_HOST:?set DEPLOY_HOST to your dev box (ssh host or IP)}"
BIN="vibecli-dev-bin"
[ -f "$BIN" ] || { echo "missing $BIN — run scripts/dev-build.sh first" >&2; exit 1; }

echo "scp -> ${HOST}:/tmp/$BIN"
scp -q "$BIN" "${HOST}:/tmp/$BIN"
echo "docker cp + restart vibecli-dev"
ssh "$HOST" "sudo docker cp /tmp/$BIN vibecli-dev:/app/vibecli && sudo docker restart vibecli-dev"
sleep 6
code=$(curl -sf -m5 -o /dev/null -w "%{http_code}" "http://${HOST}:9849/api/health" || echo "ERR")
echo "vibecli-dev health: $code  (UI: http://${HOST}:9849/)"
