#!/bin/sh
# Runtime image smoke test for vibecli. Invoked by the central CI docker job:
#   sh tests/image-smoke.sh <image-ref>
#
# Starts the assembled image and waits for the container's own HEALTHCHECK
# (HTTP GET /api/health on :9848) to report "healthy" — proving the web
# terminal server binds, the embedded UI is present, and the health endpoint
# serves. vibecli's first boot is blocking: entrypoint.sh downloads and
# verifies kiro-cli and runs setup-tools.sh in the foreground before the
# server binds, so /api/health stays down until that finishes. The image's
# HEALTHCHECK start-period (180s) holds off counting failed probes until then;
# once it elapses, failed probes count and the container flips to "unhealthy"
# after retries x interval. This test fails on the first "unhealthy" reading,
# so TIMEOUT must cover the start-period plus a couple of probe intervals (not
# merely the download) or a slow-but-OK cold boot is failed prematurely.
set -eu

IMG="${1:?usage: image-smoke.sh <image-ref>}"
NAME="smoke-vibecli-$$"
TIMEOUT=240

# shellcheck disable=SC2329  # invoked indirectly via trap
cleanup() {
	echo "--- container logs (tail) ---"
	docker logs "$NAME" 2>&1 | tail -40 || true
	docker rm -f "$NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

docker run -d --name "$NAME" "$IMG" >/dev/null

i=0
status=starting
while [ "$i" -lt "$TIMEOUT" ]; do
	status=$(docker inspect --format '{{ if .State.Health }}{{ .State.Health.Status }}{{ else }}no-healthcheck{{ end }}' "$NAME" 2>/dev/null || echo gone)
	case "$status" in
	healthy) echo "vibecli image smoke: ok (healthy after ${i}s)"; exit 0 ;;
	unhealthy) echo "FAIL: vibecli reported unhealthy"; exit 1 ;;
	no-healthcheck) echo "FAIL: image has no HEALTHCHECK to assert against"; exit 1 ;;
	gone) echo "FAIL: vibecli container exited early"; exit 1 ;;
	esac
	i=$((i + 1))
	sleep 1
done
echo "FAIL: vibecli did not become healthy within ${TIMEOUT}s (last status: $status)"
exit 1
