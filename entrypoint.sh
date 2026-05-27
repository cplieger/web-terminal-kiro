#!/bin/bash
# vibecli entrypoint. Ensures the pinned kiro-cli version is installed
# (downloads on first boot or whenever the on-disk version drifts from
# the pin), then hands off to the Go web server. Matches vibekit's
# licensing pattern: we download kiro-cli at runtime rather than bake
# it into the image so we don't redistribute proprietary AWS Content.

set -u

TOOLS="/config/tools"
BIN="$TOOLS/bin/kiro-cli"

# kiro-cli is pinned via Renovate against the public install manifest at
# https://desktop-release.q.us-east-1.amazonaws.com/index.json. Bumping
# either literal triggers a reinstall on next container start (see the
# version-drift check below). Auto-update inside the binary is disabled
# so what runs always matches the version baked into the image tag.
# KIRO_CLI_SHA256 is the sha256 of the x86_64-linux headless zip; on
# aarch64 the hash is logged but not enforced (Renovate tracks one arch).
# renovate: datasource=custom.kiro-cli depName=kiro-cli
KIRO_CLI_VERSION="2.4.2"
KIRO_CLI_SHA256="7f7239d814fbfbb963edc19deb661fb2060eeb2f3d91a504491307bb1821998d"

mkdir -p "$TOOLS/bin" "$HOME/.local/bin" "$HOME/.ssh" "$HOME/.kiro" \
    || { printf 'ERROR: failed to create config directories (is /config mounted and writable?)\n' >&2
         sleep 10
         exit 1; }

# Seed tools.json on first boot from the image default.
if [ ! -f /config/tools.json ]; then
    cp /opt/vibecli/tools.json /config/tools.json
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
        x86_64)  arch="x86_64-linux"  ;;
        aarch64) arch="aarch64-linux" ;;
        *)
            printf 'ERROR: unsupported architecture: %s\n' "$(uname -m)" >&2
            return 1
            ;;
    esac
    zip_url="https://desktop-release.q.us-east-1.amazonaws.com/${KIRO_CLI_VERSION}/kirocli-${arch}.zip"

    tmpdir=$(mktemp -d) || return 1
    zip="$tmpdir/kirocli.zip"

    if ! curl --proto '=https' --tlsv1.2 -fsSL "$zip_url" -o "$zip"; then
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
    if ! "$tmpdir/kirocli/install.sh" --no-confirm > /dev/null 2>&1; then
        # AWS install.sh accepts neither --no-confirm nor a truly silent
        # mode in all releases; fall back to the install with stdin
        # redirected from /dev/null so any prompt aborts cleanly.
        if ! "$tmpdir/kirocli/install.sh" < /dev/null > /dev/null 2>&1; then
            printf 'ERROR: install.sh failed (rc=%d)\n' "$?" >&2
            rm -rf "$tmpdir"
            return 1
        fi
    fi
    rm -rf "$tmpdir"

    # The installer drops the binary in $HOME/.local/bin; move into
    # /config/tools/bin so the path is stable regardless of how
    # /config is mounted across restarts (and so $HOME remains
    # writable-but-cleanable).
    if [ -f "$HOME/.local/bin/kiro-cli" ]; then
        mv "$HOME/.local/bin/kiro-cli" "$BIN"
    fi
    mv "$HOME/.local/bin/kiro-cli-chat" "$TOOLS/bin/kiro-cli-chat" 2>/dev/null || true
    mv "$HOME/.local/bin/kiro-cli-term" "$TOOLS/bin/kiro-cli-term" 2>/dev/null || true
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
    current=$("$BIN" --version 2>/dev/null | awk '{print $NF}')
    if [ "$current" != "$KIRO_CLI_VERSION" ]; then
        printf 'kiro-cli version drift: installed=%s pinned=%s; reinstalling\n' \
            "${current:-unknown}" "$KIRO_CLI_VERSION"
        return 0
    fi
    return 1
}

if needs_kiro_cli_install; then
    if ! install_kiro_cli; then
        printf 'WARNING: kiro-cli install failed; web UI will still start\n' >&2
        printf '         but the terminal will error until kiro-cli is present.\n' >&2
    fi
fi

# Tell kiro-cli to skip telemetry by default. User can flip it via
# `kiro-cli settings telemetry.enabled true` inside the terminal.
# Disable in-binary auto-update: KIRO_CLI_VERSION above is the source
# of truth, kept current by Renovate against the public install
# manifest. Letting kiro-cli silently replace itself would invalidate
# the pinned SHA and break image-tag reproducibility.
if [ -x "$BIN" ]; then
    "$BIN" settings telemetry.enabled false > /dev/null 2>&1 || true
    "$BIN" settings "app.disableAutoupdates" "true" > /dev/null 2>&1 || true
fi

# Install/update tools from /config/tools.json manifest.
if [ -f /config/tools.json ]; then
    bash /opt/vibecli/setup-tools.sh
fi

# Hardcode dark theme. kiro-cli's "default" diff preset resolves
# added-line bg to #00FF00 through the truecolor path — unreadable.
# Pinning both baseTheme and diffPreset to "dark" avoids this.
theme_file="$HOME/.kiro/settings/kiro_cli_theme.json"
mkdir -p "$(dirname "$theme_file")"
printf '{"baseTheme":"dark","diffPreset":"dark"}\n' > "$theme_file"

exec /app/vibecli
