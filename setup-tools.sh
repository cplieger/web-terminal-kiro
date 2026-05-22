#!/bin/bash
# Reads /config/tools.json, checks for updates, and installs missing tools.
# Runs on every container boot; tools persist in /config/tools/ volume.
set -uo pipefail

TOOLS="/tools"
BIN="$TOOLS/bin"
MANIFEST="/config/tools.json"

export PATH="$BIN:$PATH"

mkdir -p "$BIN"

if [ ! -f "$MANIFEST" ]; then
    printf "ERROR: %s not found\n" "$MANIFEST"
    exit 1
fi

printf "[%s] Tool setup starting\n" "$(date -Iseconds)"

# --- GitHub auth for rate limits ---

GH_AUTH=""
if command -v gh >/dev/null 2>&1; then
    GH_AUTH=$(gh auth token 2>/dev/null) || true
fi

gh_curl() {
    if [ -n "$GH_AUTH" ]; then
        curl -fsSL --connect-timeout 10 --max-time 15 \
            -H "Authorization: Bearer $GH_AUTH" "$@" 2>/dev/null
    else
        curl -fsSL --connect-timeout 10 --max-time 15 "$@" 2>/dev/null
    fi
}

# --- Helpers ---

expand() {
    local cmd="$1" version="$2"
    local version_nopfx="${version#v}"
    cmd="${cmd//\$\{VERSION\}/$version}"
    cmd="${cmd//\$\{VERSION_NOPFX\}/$version_nopfx}"
    cmd="${cmd//\$\{BIN\}/$BIN}"
    cmd="${cmd//\$\{TOOLS\}/$TOOLS}"
    cmd="${cmd//\$\{HOME\}/$HOME}"
    printf '%s' "$cmd"
}

has_bin() {
    [ -f "$BIN/$1" ]
}

section_empty() {
    local count
    count=$(jq -r "(.${1} // {}) | length" "$MANIFEST" 2>/dev/null || echo 0)
    [ "$count" = "0" ]
}

check_update() {
    local jq_path="$1" name="$2" current="$3" method="$4"
    local latest=""

    case "$method" in
        manual|null) return 1 ;;
    esac

    case "$current" in
        v*|[0-9]*) ;;
        *) return 1 ;;
    esac

    case "$method" in
        github)
            local repo
            repo=$(jq -r "${jq_path}.update.repo" "$MANIFEST")
            latest=$(gh_curl \
                "https://api.github.com/repos/${repo}/releases/latest" \
                | jq -r '.tag_name // empty') || true
            ;;
    esac

    if [ -z "$latest" ] || [ "$current" = "$latest" ]; then
        return 1
    fi

    printf "    update: %s -> %s\n" "$current" "$latest"
    local tmp
    if ! tmp=$(jq --arg v "$latest" "${jq_path}.version = \$v" "$MANIFEST"); then
        printf "    update: jq rewrite failed, keeping pinned version\n"
        return 1
    fi
    if [ -z "$tmp" ] || ! printf '%s' "$tmp" | jq empty >/dev/null 2>&1; then
        printf "    update: jq produced invalid output, keeping pinned version\n"
        return 1
    fi
    printf '%s\n' "$tmp" > "${MANIFEST}.tmp" && mv "${MANIFEST}.tmp" "$MANIFEST"
    return 0
}

clear_tool() {
    rm -f "$BIN/$1"
}

# --- Binary tools ---

printf "\n=== Binary tools ===\n"
if section_empty binary; then
    printf "  (none configured)\n"
else
    for name in $(jq -r '.binary | keys[]' "$MANIFEST"); do
        version=$(jq -r ".binary[\"$name\"].version" "$MANIFEST")
        method=$(jq -r ".binary[\"$name\"].update.method" "$MANIFEST")
        printf "  %s (%s):\n" "$name" "$version"
        if check_update ".binary[\"$name\"]" "$name" "$version" "$method"; then
            clear_tool "$name"
            version=$(jq -r ".binary[\"$name\"].version" "$MANIFEST")
        fi
        if ! has_bin "$name"; then
            install_cmd=$(jq -r ".binary[\"$name\"].install" "$MANIFEST")
            printf "    installing...\n"
            eval "$(expand "$install_cmd" "$version")"
        else
            printf "    installed\n"
        fi
    done
fi

# --- Custom tools ---

printf "\n=== Custom tools ===\n"
if section_empty custom; then
    printf "  (none configured)\n"
else
    for name in $(jq -r '.custom | keys[]' "$MANIFEST"); do
        version=$(jq -r ".custom[\"$name\"].version" "$MANIFEST")
        method=$(jq -r ".custom[\"$name\"].update.method" "$MANIFEST")
        printf "  %s (%s):\n" "$name" "$version"
        if check_update ".custom[\"$name\"]" "$name" "$version" "$method"; then
            clear_tool "$name"
            version=$(jq -r ".custom[\"$name\"].version" "$MANIFEST")
        fi
        if ! has_bin "$name"; then
            install_cmd=$(jq -r ".custom[\"$name\"].install" "$MANIFEST")
            printf "    installing...\n"
            eval "$(expand "$install_cmd" "$version")"
        else
            printf "    installed\n"
        fi
    done
fi

# --- System packages (apt) ---

printf "\n=== System packages (apt) ===\n"
if section_empty apt; then
    printf "  (none configured)\n"
else
    apt_list=""
    for name in $(jq -r '.apt | keys[]' "$MANIFEST"); do
        if ! command -v "$name" > /dev/null 2>&1; then
            apt_list="$apt_list $name"
        else
            printf "  %s: installed\n" "$name"
        fi
    done
    if [ -n "$apt_list" ]; then
        printf "  installing:%s\n" "$apt_list"
        # shellcheck disable=SC2086 # word-splitting intentional: $apt_list is a space-separated package list
        apt-get update -qq && apt-get install -y -qq --no-install-recommends $apt_list 2>&1 | tail -3
        rm -rf /var/lib/apt/lists/*
    fi
fi

printf "\n[%s] Tool setup complete\n" "$(date -Iseconds)"
