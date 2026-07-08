#!/bin/sh
# Runtime image smoke test for web-terminal-kiro. Invoked by the central CI docker job:
#   sh tests/image-smoke.sh <image-ref>
#
# Starts the assembled image and waits for the container's own HEALTHCHECK
# (HTTP GET /api/health on :9848) to report "healthy" — proving the web
# terminal server binds, the embedded UI is present, and the health endpoint
# serves. web-terminal-kiro's first boot is blocking: entrypoint.sh downloads and
# verifies kiro-cli and runs setup-tools.sh in the foreground before the
# server binds, so /api/health stays down until that finishes. The image's
# HEALTHCHECK start-period (180s) holds off counting failed probes until then;
# once it elapses, failed probes count and the container flips to "unhealthy"
# after retries x interval. This test fails on the first "unhealthy" reading,
# so TIMEOUT must cover the start-period plus a couple of probe intervals (not
# merely the download) or a slow-but-OK cold boot is failed prematurely.
set -eu

IMG="${1:?usage: image-smoke.sh <image-ref>}"
NAME="smoke-web-terminal-kiro-$$"
TIMEOUT=240 # see header: covers the first-boot kiro-cli download + 180s start-period

# shellcheck disable=SC2329  # invoked indirectly via trap
cleanup() {
  code=$?
  # Dump container logs only on failure (a passing run stays quiet).
  if [ "$code" -ne 0 ]; then
    printf '%s\n' "--- container logs (tail) ---" >&2
    docker logs "$NAME" 2>&1 | tail -40 >&2 || true
  fi
  docker rm -f "$NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

docker run -d --name "$NAME" "$IMG" >/dev/null

i=0
status=starting
while [ "$i" -lt "$TIMEOUT" ]; do
  # Fail fast on an early exit: poll .State.Running before the health status so
  # a crash-boot is caught by its exit code (more debuggable than "unhealthy")
  # and the verdict never depends on what health a stopped container reports.
  if [ "$(docker inspect --format '{{ .State.Running }}' "$NAME" 2>/dev/null || echo missing)" != "true" ]; then
    ec=$(docker inspect --format '{{ .State.ExitCode }}' "$NAME" 2>/dev/null || echo '?')
    printf 'FAIL: web-terminal-kiro container exited early (exit code %s)\n' "$ec" >&2
    exit 1
  fi
  status=$(docker inspect --format '{{ if .State.Health }}{{ .State.Health.Status }}{{ else }}no-healthcheck{{ end }}' "$NAME" 2>/dev/null || echo gone)
  case "$status" in
    healthy)
      printf 'web-terminal-kiro image smoke: ok (healthy after %ss)\n' "$i"
      exit 0
      ;;
    unhealthy)
      printf 'FAIL: web-terminal-kiro reported unhealthy\n' >&2
      exit 1
      ;;
    no-healthcheck)
      printf 'FAIL: image has no HEALTHCHECK to assert against\n' >&2
      exit 1
      ;;
    gone)
      printf 'FAIL: web-terminal-kiro container is gone\n' >&2
      exit 1
      ;;
  esac
  i=$((i + 1))
  sleep 1
done
printf 'FAIL: web-terminal-kiro did not become healthy within %ss (last status: %s)\n' "$TIMEOUT" "$status" >&2
exit 1
