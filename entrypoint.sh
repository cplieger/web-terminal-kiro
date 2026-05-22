#!/bin/bash
# vibecli entrypoint. Ensures kiro-cli is installed (first boot only),
# then hands off to the Go web server. Matches vibekit's licensing
# pattern: we download kiro-cli at runtime rather than bake it into
# the image so we don't redistribute proprietary AWS Content.

set -u

TOOLS="/config/tools"
BIN="$TOOLS/bin/kiro-cli"

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
    printf 'First boot: installing kiro-cli\n'
    printf '  kiro-cli is proprietary AWS Content; by installing you accept\n'
    printf '  the AWS Customer Agreement. License: https://kiro.dev/license/\n'

    # Direct download from the AWS-hosted zip per the docs:
    # https://kiro.dev/docs/cli/installation/ ("With a zip file" section).
    # Bypassing https://cli.kiro.dev/install lets us hash-verify the
    # binary before exec, avoiding the "pipe-remote-script-to-bash"
    # supply-chain risk. The /latest/ URL has no version suffix in
    # AWS's documented scheme; we capture and log the SHA-256 of every
    # download so drift is visible in container startup logs.
    local arch zip_url tmpdir zip
    case "$(uname -m)" in
        x86_64)  arch="x86_64-linux"  ;;
        aarch64) arch="aarch64-linux" ;;
        *)
            printf 'ERROR: unsupported architecture: %s\n' "$(uname -m)" >&2
            return 1
            ;;
    esac
    zip_url="https://desktop-release.q.us-east-1.amazonaws.com/latest/kirocli-${arch}.zip"

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

    # Compute and log SHA-256 for the audit trail. EXPECTED_KIRO_CLI_SHA256
    # is opt-in: when set (compose env, Dockerfile ARG, or local export),
    # a mismatch refuses install. When unset, we log the hash and continue
    # so the homelab default does not break when AWS publishes a new
    # binary; operators harden by bumping the env var.
    local actual
    actual=$(sha256sum "$zip" | awk '{print $1}')
    printf 'kiro-cli zip SHA-256: %s (url=%s)\n' "$actual" "$zip_url"
    if [ -n "${EXPECTED_KIRO_CLI_SHA256:-}" ]; then
        if [ "$actual" != "$EXPECTED_KIRO_CLI_SHA256" ]; then
            printf 'ERROR: kiro-cli SHA-256 mismatch\n' >&2
            printf '  expected: %s\n' "$EXPECTED_KIRO_CLI_SHA256" >&2
            printf '  actual:   %s\n' "$actual" >&2
            printf '  refusing install; bump EXPECTED_KIRO_CLI_SHA256 in compose to accept the new binary\n' >&2
            rm -rf "$tmpdir"
            return 1
        fi
        printf 'kiro-cli SHA-256 matches EXPECTED_KIRO_CLI_SHA256; integrity verified\n'
    else
        printf 'kiro-cli integrity unverified (set EXPECTED_KIRO_CLI_SHA256=%s to enforce)\n' "$actual"
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

if [ ! -x "$BIN" ]; then
    if ! install_kiro_cli; then
        printf 'WARNING: kiro-cli install failed; web UI will still start\n' >&2
        printf '         but the terminal will error until kiro-cli is present.\n' >&2
    fi
fi

# Tell kiro-cli to skip telemetry by default. User can flip it via
# `kiro-cli settings telemetry.enabled true` inside the terminal.
# Also disable auto-update so the binary kiro-cli runs is the one we
# verified at install time. Without this, kiro-cli silently replaces
# itself in the background on every release (https://kiro.dev/docs/cli/installation/#upgrading),
# which would invalidate our SHA-256 audit trail. Re-enable manually
# via `kiro-cli settings "app.disableAutoupdates" "false"` if desired.
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
