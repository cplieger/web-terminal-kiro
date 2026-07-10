#!/bin/bash
# web-terminal-kiro entrypoint. Ensures the pinned kiro-cli version is installed
# (downloads on first boot or whenever the on-disk version drifts from
# the pin), then hands off to the Go web server. Matches vibekit's
# licensing pattern: we download kiro-cli at runtime rather than bake
# it into the image so we don't redistribute proprietary AWS Content.

set -u

TOOLS="/config/tools"
BIN="$TOOLS/bin/kiro-cli"

# Parse the version kiro-cli reports (last field of `--version`). Centralized
# so the three call sites (install verify, drift check, readiness marker)
# share one parse if kiro-cli ever reworks its --version output.
kiro_cli_version() {
  "$1" --version 2>/dev/null | awk '{print $NF}'
}

# kiro-cli is pinned via Renovate against the public install manifest at
# https://desktop-release.q.us-east-1.amazonaws.com/index.json. Bumping
# either literal triggers a reinstall on next container start (see the
# version-drift check below). Auto-update inside the binary is disabled
# so what runs always matches the version baked into the image tag.
# KIRO_CLI_SHA256 is the sha256 of the x86_64-linux headless zip; on
# aarch64 the hash is logged but not enforced (Renovate tracks one arch).
# COUPLING (re-verify on every bump): routes.go's status classifier matches
# kiro-cli's EXACT OSC 9 notification strings "Response complete" (turn end ->
# done dot) and "Permission required" (tool approval -> needs-input dot),
# verified against this version. A bump that reworded either string silently
# stops the per-tab status dots from latching (no error; only a Debug log in
# routes.go). The feature also depends on the chat.enableNotifications +
# chat.notificationMethod=osc9 settings set below and web-terminal-engine's
# WithKeepUnfocused() in routes.go -- keep all four in lockstep.
# renovate: datasource=custom.kiro-cli depName=kiro-cli
KIRO_CLI_VERSION="2.12.1"
KIRO_CLI_SHA256="410bf0d6da4c570f46f765826c2fbbafffce42c06c25e04151dfb3b954c07cfd"

mkdir -p "$TOOLS/bin" "$HOME/.local/bin" "$HOME/.ssh" "$HOME/.kiro" \
  || {
    printf 'ERROR: failed to create config directories (is /config mounted and writable?)\n' >&2
    sleep 10
    exit 1
  }

# Seed tools.json on first boot from the image default.
if [ ! -f /config/tools.json ]; then
  cp /opt/web-terminal-kiro/tools.json /config/tools.json
  printf 'First boot: created /config/tools.json from defaults\n'
fi

install_kiro_cli() {
  printf 'Installing kiro-cli %s\n' "$KIRO_CLI_VERSION"
  printf '  kiro-cli is proprietary AWS Content; by installing you accept\n'
  printf '  the AWS Customer Agreement. License: https://kiro.dev/license/\n'

  # Direct download from the AWS-hosted zip per the docs:
  # https://kiro.dev/docs/cli/installation/ ("With a zip file" section).
  # We pin the version (not /latest/) so a given image tag is reproducible,
  # and verify the sha256 before running install.sh.
  local arch zip_url tmpdir zip
  case "$(uname -m)" in
    x86_64) arch="x86_64-linux" ;;
    aarch64) arch="aarch64-linux" ;;
    *)
      printf 'ERROR: unsupported architecture: %s\n' "$(uname -m)" >&2
      return 1
      ;;
  esac
  zip_url="https://desktop-release.q.us-east-1.amazonaws.com/${KIRO_CLI_VERSION}/kirocli-${arch}.zip"

  tmpdir=$(mktemp -d) || return 1
  zip="$tmpdir/kirocli.zip"

  if ! curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL \
    --connect-timeout 20 --max-time 300 --retry 3 --retry-delay 5 \
    "$zip_url" -o "$zip"; then
    printf 'ERROR: failed to download kiro-cli zip from %s\n' "$zip_url" >&2
    rm -rf "$tmpdir"
    return 1
  fi
  if [ ! -s "$zip" ]; then
    printf 'ERROR: kiro-cli zip is empty (partial download?)\n' >&2
    rm -rf "$tmpdir"
    return 1
  fi

  # Verify SHA-256. KIRO_CLI_SHA256 is the x86_64-linux hash from the
  # install manifest, kept in lockstep with KIRO_CLI_VERSION by Renovate.
  # On aarch64 we log the hash for the audit trail but do not enforce
  # because we don't track a second per-arch literal.
  local actual
  actual=$(sha256sum "$zip" | awk '{print $1}')
  printf 'kiro-cli zip SHA-256: %s (url=%s)\n' "$actual" "$zip_url"
  if [ "$arch" = "x86_64-linux" ]; then
    if [ "$actual" != "$KIRO_CLI_SHA256" ]; then
      printf 'ERROR: kiro-cli SHA-256 mismatch\n' >&2
      printf '  expected: %s\n' "$KIRO_CLI_SHA256" >&2
      printf '  actual:   %s\n' "$actual" >&2
      printf '  refusing install; bump KIRO_CLI_VERSION/KIRO_CLI_SHA256 together\n' >&2
      rm -rf "$tmpdir"
      return 1
    fi
    printf 'kiro-cli SHA-256 verified against pinned hash\n'
  else
    printf 'kiro-cli SHA-256 unverified on %s (no pinned hash for this arch)\n' "$arch"
  fi

  if ! unzip -q "$zip" -d "$tmpdir"; then
    printf 'ERROR: failed to extract kiro-cli zip\n' >&2
    rm -rf "$tmpdir"
    return 1
  fi

  # Run upstream install.sh. Don't gate on its exit code — the kiro-cli
  # installer touches shell profiles and other side surfaces that
  # legitimately fail in our minimal root container; what matters is
  # whether the binary it drops at $HOME/.local/bin/kiro-cli reports
  # the version we pinned. Capture install.sh output to a tempfile so
  # we can surface it on failure.
  local install_log install_rc
  install_log=$(mktemp)
  "$tmpdir/kirocli/install.sh" --no-confirm </dev/null >"$install_log" 2>&1
  install_rc=$?
  rm -rf "$tmpdir"

  if [ ! -f "$HOME/.local/bin/kiro-cli" ]; then
    printf 'ERROR: install.sh did not produce %s/.local/bin/kiro-cli (rc=%d)\n' \
      "$HOME" "$install_rc" >&2
    printf 'install.sh output:\n' >&2
    cat "$install_log" >&2
    rm -f "$install_log"
    return 1
  fi
  local installed
  installed=$(kiro_cli_version "$HOME/.local/bin/kiro-cli")
  if [ "$installed" != "$KIRO_CLI_VERSION" ]; then
    printf 'ERROR: installed binary reports version %s, wanted %s (install.sh rc=%d)\n' \
      "${installed:-unknown}" "$KIRO_CLI_VERSION" "$install_rc" >&2
    printf 'install.sh output:\n' >&2
    cat "$install_log" >&2
    rm -f "$install_log"
    return 1
  fi
  rm -f "$install_log"

  # Promote to the canonical /config/tools/bin/ location so PATH
  # ordering (which puts /config/tools/bin first) and any in-process
  # absolute-path references resolve to the freshly installed binary.
  mv -f "$HOME/.local/bin/kiro-cli" "$BIN" || return 1
  mv -f "$HOME/.local/bin/kiro-cli-chat" "$TOOLS/bin/kiro-cli-chat" 2>/dev/null || true
  mv -f "$HOME/.local/bin/kiro-cli-term" "$TOOLS/bin/kiro-cli-term" 2>/dev/null || true
}

# Reinstall when either the binary is missing or the on-disk version
# drifts from KIRO_CLI_VERSION. The binary lives on the persistent
# /config volume, so a freshly bumped image needs this drift check to
# actually pick up the new version on restart.
needs_kiro_cli_install() {
  if [ ! -x "$BIN" ]; then
    return 0
  fi
  local current
  current=$(kiro_cli_version "$BIN")
  if [ "$current" != "$KIRO_CLI_VERSION" ]; then
    printf 'kiro-cli version drift: installed=%s pinned=%s; reinstalling\n' \
      "${current:-unknown}" "$KIRO_CLI_VERSION"
    return 0
  fi
  return 1
}

if needs_kiro_cli_install; then
  if ! install_kiro_cli; then
    printf 'level=warn msg="kiro-cli install failed; web UI starts but the terminal errors until kiro-cli is present" component=entrypoint\n' >&2
  fi
fi

# Tell kiro-cli to skip telemetry by default. User can flip it via
# `kiro-cli settings telemetry.enabled true` inside the terminal.
# Disable in-binary auto-update: KIRO_CLI_VERSION above is the source
# of truth, kept current by Renovate against the public install
# manifest. Letting kiro-cli silently replace itself would invalidate
# the pinned SHA and break image-tag reproducibility.
if [ -x "$BIN" ]; then
  "$BIN" settings telemetry.enabled false >/dev/null 2>&1 || true
  "$BIN" settings "app.disableAutoupdates" "true" >/dev/null 2>&1 || true
  # Enable kiro-cli's OSC 9 desktop-notification escape so web-terminal-kiro's tab
  # activity monitor can classify turn-end ("Response complete") and
  # tool-approval ("Permission required") into per-tab status dots. osc9 emits
  # the notification inline in the PTY stream (the only method that reaches a
  # browser terminal); the server holds each session "unfocused" so kiro-cli's
  # focus-gated notifier keeps firing even with no focused browser tab.
  "$BIN" settings chat.enableNotifications true >/dev/null 2>&1 || true
  "$BIN" settings chat.notificationMethod osc9 >/dev/null 2>&1 || true
  # Explicitly disable kiro-cli's dynamic terminal title. Its OSC 0 title only
  # reflects the cwd for a live session (it reloads its session title just on a
  # session-id change, not per turn). The web-terminal-ui tabs feature PREFERS
  # the process OSC title over its own fallback, so leaving this on would make
  # every tab read "kiro: ~/workspace" instead of something useful. Set false
  # (not merely unset) so a container that previously persisted it true gets it
  # turned off on restart. With it off, the tabs feature titles each tab from
  # the user's last submitted line instead.
  "$BIN" settings chat.terminalTitle false >/dev/null 2>&1 || true
fi

# Readiness marker consumed by the Go server's /api/health (main.go reads
# KIRO_CLI_READY_MARKER; routes.go Stats it). kiro-cli is web-terminal-kiro's core
# dependency, yet the HTTP listener comes up even when the first-boot install
# failed (degraded-not-dead start, per the install WARNING above). Record here
# whether a runnable, correctly-versioned binary is present so the health signal
# reflects the core dependency. Verified ONCE at boot via --version (do NOT
# relaunch kiro-cli per health probe — spawning a heavy PTY process every probe
# would be an anti-pattern). This is a READINESS signal: under
# `restart: unless-stopped` nothing restarts on the resulting unhealthy state,
# so a broken kiro-cli shows as `unhealthy` in `docker ps` + the monitoring
# probe with no restart loop. If ever run under Swarm/k8s, wire /api/health to a
# readinessProbe, not a livenessProbe, to keep it loop-free.
KIRO_CLI_READY_MARKER="$TOOLS/.kiro-cli-ready"
export KIRO_CLI_READY_MARKER
if [ -x "$BIN" ] && [ "$(kiro_cli_version "$BIN")" = "$KIRO_CLI_VERSION" ]; then
  touch "$KIRO_CLI_READY_MARKER"
else
  rm -f "$KIRO_CLI_READY_MARKER"
fi

# Install/update tools from /config/tools.json, FOREGROUND (blocking) so LSPs
# and other tools are on PATH before the server can spawn kiro-cli — kiro-cli
# scans PATH for language servers at code-intelligence init, and a non-blocking
# install here would race that scan on first boot, leaving LSPs undetected.
# Logged so an incomplete/failed run is diagnosable rather than silent.
if [ -s /config/tools.json ]; then
  SETUP_LOG="/tmp/setup-tools.log"
  printf 'Running setup-tools.sh (log: %s)\n' "$SETUP_LOG"
  bash /opt/web-terminal-kiro/setup-tools.sh 2>&1 | tee "$SETUP_LOG"
  if [ "${PIPESTATUS[0]}" -ne 0 ]; then
    printf 'level=warn msg="setup-tools.sh reported failures" log=%s component=entrypoint\n' "$SETUP_LOG" >&2
  fi
fi

# Hardcode dark theme. kiro-cli's "default" diff preset resolves
# added-line bg to #00FF00 through the truecolor path — unreadable.
# Pinning both baseTheme and diffPreset to "dark" avoids this.
theme_file="$HOME/.kiro/settings/kiro_cli_theme.json"
mkdir -p "$(dirname "$theme_file")"
printf '{"baseTheme":"dark","diffPreset":"dark"}\n' >"$theme_file"

exec /app/web-terminal-kiro
