#!/bin/bash
# Reads /config/tools.json on container boot, checks for updates, and
# installs enabled tools into /config/tools (persisted across restarts).
#
# web-terminal-kiro has no management UI, so everything is driven by tools.json:
#   enabled      bool        install this entry (missing -> true)
#   auto_update  bool        bump version on boot (missing -> true; false pins)
#   requires     []"sec.name" deps that must be enabled first (e.g. lsp gopls
#                            requires runtimes.go)
#   shims        {name: cmd} wrapper scripts written to BIN exec'ing cmd —
#                            lets tsc masquerade as typescript-language-server
#                            and pyrefly as pyright (the names kiro-cli probes)
#   version / update{method,...} / install
#   lsp.method   binary|go   which install pipeline an lsp entry uses
#
# Sections (install order): runtimes, binary, custom, lsp, apt.
set -uo pipefail

TOOLS="${TOOLS:-/config/tools}"
BIN="$TOOLS/bin"
RUNTIMES="$TOOLS/runtimes"
GOBIN="$TOOLS/go/bin"
MANIFEST="${MANIFEST:-/config/tools.json}"

# Cap each install so a half-open socket can't hang the blocking
# foreground boot forever (the server only execs after setup-tools
# returns). 600s is generous for the ~190 MB Go runtime on a slow link.
INSTALL_TIMEOUT="${INSTALL_TIMEOUT:-600}"

export GOROOT="$RUNTIMES/go"
export GOPATH="$TOOLS/go"
export GOBIN
export PATH="$BIN:$GOBIN:$RUNTIMES/go/bin:$RUNTIMES/node/bin:$PATH"

mkdir -p "$BIN" "$GOBIN" "$RUNTIMES" "$TOOLS/lib"

if [ ! -f "$MANIFEST" ]; then
  printf "ERROR: %s not found\n" "$MANIFEST"
  exit 1
fi

printf "[%s] Tool setup starting\n" "$(date -Iseconds)"

# Count install failures so the run can exit non-zero — lets the entrypoint
# surface a WARNING instead of a partial install passing silently.
FAILURES=0

# --- Architecture detection (consumed via expand() placeholders) ---
case "$(uname -m)" in
  aarch64 | arm64)
    ARCH_X64_OR_ARM64="arm64"
    ARCH_AMD64_OR_ARM64="arm64"
    ARCH_X86_64_OR_AARCH64="aarch64"
    ARCH_X64_OR_AARCH64="aarch64"
    ARCH_X86_64_OR_ARM64="arm64"
    ;;
  *)
    ARCH_X64_OR_ARM64="x64"
    ARCH_AMD64_OR_ARM64="amd64"
    ARCH_X86_64_OR_AARCH64="x86_64"
    ARCH_X64_OR_AARCH64="x64"
    ARCH_X86_64_OR_ARM64="x86_64"
    ;;
esac

# --- GitHub auth for higher API rate limits (optional) ---
GH_AUTH=""
if command -v gh >/dev/null 2>&1; then
  GH_AUTH=$(gh auth token 2>/dev/null) || true
fi
gh_curl() {
  if [ -n "$GH_AUTH" ]; then
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL --connect-timeout 10 --max-time 15 -H "Authorization: Bearer $GH_AUTH" "$@" 2>/dev/null
  else
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL --connect-timeout 10 --max-time 15 "$@" 2>/dev/null
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
  cmd="${cmd//\$\{RUNTIMES\}/$RUNTIMES}"
  cmd="${cmd//\$\{GOBIN\}/$GOBIN}"
  cmd="${cmd//\$\{HOME\}/$HOME}"
  cmd="${cmd//\$\{ARTIFACT\}/${ARTIFACT:-}}"
  cmd="${cmd//\$\{ARCH_X64_OR_ARM64\}/$ARCH_X64_OR_ARM64}"
  cmd="${cmd//\$\{ARCH_AMD64_OR_ARM64\}/$ARCH_AMD64_OR_ARM64}"
  cmd="${cmd//\$\{ARCH_X86_64_OR_AARCH64\}/$ARCH_X86_64_OR_AARCH64}"
  cmd="${cmd//\$\{ARCH_X64_OR_AARCH64\}/$ARCH_X64_OR_AARCH64}"
  cmd="${cmd//\$\{ARCH_X86_64_OR_ARM64\}/$ARCH_X86_64_OR_ARM64}"
  printf '%s' "$cmd"
}

has_bin() {
  [ -x "$BIN/$1" ] || [ -x "$GOBIN/$1" ]
}

section_empty() {
  local count
  count=$(jq -r "(.${1} // {}) | length" "$MANIFEST" 2>/dev/null || echo 0)
  [ "$count" = "0" ]
}

# enabled/auto_update default to true when the field is absent. jq's `//`
# treats false as empty, so test `!= false` (true when true OR absent).
entry_enabled() { [ "$(jq -r "${1}.enabled != false" "$MANIFEST")" = "true" ]; }
entry_auto_update() { [ "$(jq -r "${1}.auto_update != false" "$MANIFEST")" = "true" ]; }

# Write wrapper scripts for each .shims entry so kiro-cli can spawn the
# real binary under the LSP name it expects.
write_shims() {
  local jq_path="$1" shim_name shim_cmd
  [ "$(jq -r "(${jq_path}.shims // {}) | length" "$MANIFEST" 2>/dev/null || echo 0)" = "0" ] && return 0
  while IFS=$'\t' read -r shim_name shim_cmd; do
    [ -z "$shim_name" ] && continue
    printf '#!/bin/sh\nexec %s "$@"\n' "$shim_cmd" >"$BIN/$shim_name"
    chmod 755 "$BIN/$shim_name"
    printf "    shim: %s -> %s\n" "$shim_name" "$shim_cmd"
  done < <(jq -r "${jq_path}.shims // {} | to_entries[] | \"\(.key)\t\(.value)\"" "$MANIFEST")
}

remove_shims() {
  local jq_path="$1" shim_name
  while IFS= read -r shim_name; do
    [ -z "$shim_name" ] && continue
    rm -f "$BIN/$shim_name"
  done < <(jq -r "${jq_path}.shims // {} | keys[]" "$MANIFEST" 2>/dev/null)
}

# Check upstream for a newer version and rewrite the manifest. Returns 0 if
# changed. Honors auto_update:false. Methods: github, gomod, url.
check_update() {
  local jq_path="$1" current="$2" method="$3" latest=""
  entry_auto_update "$jq_path" || return 1
  case "$method" in manual | null) return 1 ;; esac
  # Skip pure-hex commit pins (not comparable); everything else is a tag.
  case "$current" in
    *[!0-9a-f]*) ;;
    [0-9a-f]*[0-9a-f]) return 1 ;;
  esac
  case "$method" in
    github)
      local repo
      repo=$(jq -r "${jq_path}.update.repo" "$MANIFEST")
      latest=$(gh_curl "https://api.github.com/repos/${repo}/releases/latest" | jq -r '.tag_name // empty') || true
      ;;
    gomod)
      local mod
      mod=$(jq -r "${jq_path}.update.module" "$MANIFEST")
      latest=$(curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL --connect-timeout 10 --max-time 15 "https://proxy.golang.org/${mod}/@latest" 2>/dev/null | jq -r '.Version // empty') || true
      ;;
    url)
      local url prefix raw
      url=$(jq -r "${jq_path}.update.url" "$MANIFEST")
      prefix=$(jq -r "${jq_path}.update.strip_prefix // empty" "$MANIFEST")
      raw=$(curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL --connect-timeout 10 --max-time 15 "$url" 2>/dev/null | head -1) || true
      [ -n "$raw" ] && latest="${raw#"$prefix"}"
      ;;
  esac
  [ -z "$latest" ] && {
    printf "    update: fetch failed\n"
    return 1
  }
  # Reject version strings with shell metacharacters (eval'd into install).
  case "$latest" in *[!a-zA-Z0-9._+-]*)
    printf "    update: rejected upstream version (illegal chars), keeping pinned\n"
    return 1
    ;;
  esac
  [ "$current" = "$latest" ] && return 1
  printf "    update: %s -> %s\n" "$current" "$latest"
  local tmp
  if ! tmp=$(jq --arg v "$latest" "${jq_path}.version = \$v" "$MANIFEST") || [ -z "$tmp" ] || ! printf '%s' "$tmp" | jq empty >/dev/null 2>&1; then
    printf "    update: jq rewrite failed, keeping pinned version\n"
    return 1
  fi
  printf '%s\n' "$tmp" >"${MANIFEST}.tmp" && mv "${MANIFEST}.tmp" "$MANIFEST"
  return 0
}

# Remove a tool's install footprint so the next loop re-downloads it.
clear_tool() {
  local section="$1" name="$2"
  local jq_path=".${section}[\"$name\"]"
  case "$section" in
    runtimes) rm -rf "${RUNTIMES:?}/$name" ;;
    binary | custom) rm -f "$BIN/$name" ;;
    lsp)
      case "$(jq -r "${jq_path}.method // \"binary\"" "$MANIFEST")" in
        go) for b in $(jq -r "${jq_path}.binaries[]?" "$MANIFEST" 2>/dev/null); do rm -f "$GOBIN/$b"; done ;;
        *) rm -f "$BIN/$name" ;;
      esac
      ;;
  esac
  remove_shims "$jq_path"
}

# Refuse to install an entry until every "section.name" in .requires is
# enabled (transitive deps, e.g. gopls -> runtimes.go).
requires_satisfied() {
  local jq_path="$1" req sec n ok
  while IFS= read -r req; do
    [ -z "$req" ] && continue
    sec="${req%%.*}"
    n="${req#*.}"
    ok=$(jq -r "(.${sec}[\"${n}\"] != null) and (.${sec}[\"${n}\"].enabled != false)" "$MANIFEST" 2>/dev/null)
    if [ "$ok" != "true" ]; then
      printf "    skipped: requires %s (not enabled)\n" "$req"
      return 1
    fi
  done < <(jq -r "${jq_path}.requires[]?" "$MANIFEST" 2>/dev/null)
  return 0
}

# Optional download-then-verify. If the entry provides a "url" (the artifact to
# fetch) AND a per-arch "sha256" ({"amd64":..,"arm64":..}), the artifact is
# downloaded to a temp file, its sha256 is checked, and the install command
# extracts from ${ARTIFACT} (the verified file) instead of re-fetching. Entries
# without BOTH fields install exactly as before (their install command does its
# own curl|tar). Verification is opt-in per entry: a missing checksum never fails.
run_install() {
  local jq_path="$1" version="$2" install_cmd url sha256 ARTIFACT="" rc actual
  install_cmd=$(jq -r "${jq_path}.install" "$MANIFEST")
  if [ "$install_cmd" = "null" ] || [ -z "$install_cmd" ]; then
    printf "    error: no install command\n"
    return 1
  fi
  url=$(jq -r "${jq_path}.url // empty" "$MANIFEST")
  sha256=$(jq -r --arg a "$ARCH_AMD64_OR_ARM64" "${jq_path}.sha256[\$a] // empty" "$MANIFEST")
  # Surface a per-arch integrity downgrade: an entry that opted into a sha256 map but omits the
  # running arch would otherwise install UNVERIFIED on that arch with no diagnostic.
  if [ -n "$url" ] && [ -z "$sha256" ] \
    && [ "$(jq -r "(${jq_path}.sha256 // empty) | type" "$MANIFEST" 2>/dev/null)" = "object" ]; then
    printf 'level=warn msg="sha256 map present but no entry for %s; installing UNVERIFIED" component=setup-tools\n' "$ARCH_AMD64_OR_ARM64" >&2
  fi
  if [ -n "$url" ] && [ -n "$sha256" ]; then
    ARTIFACT=$(mktemp)
    if ! curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL --connect-timeout 20 --max-time "$INSTALL_TIMEOUT" -o "$ARTIFACT" "$(expand "$url" "$version")"; then
      printf "    error: download failed\n"
      rm -f "$ARTIFACT"
      return 1
    fi
    actual=$(sha256sum "$ARTIFACT" | awk '{print $1}')
    if [ "$actual" != "$sha256" ]; then
      printf "    error: sha256 mismatch (want %s, got %s)\n" "$sha256" "$actual"
      rm -f "$ARTIFACT"
      return 1
    fi
    printf "    sha256 verified\n"
  fi
  timeout "$INSTALL_TIMEOUT" bash -uo pipefail -c "$(expand "$install_cmd" "$version")"
  rc=$?
  [ -n "$ARTIFACT" ] && rm -f "$ARTIFACT"
  return "$rc"
}

# --- Sections (order matters: runtimes before lsp which may require them) ---

printf "\n=== Runtimes ===\n"
if section_empty runtimes; then printf "  (none configured)\n"; else
  for name in $(jq -r '.runtimes | keys[]' "$MANIFEST"); do
    jq_path=".runtimes[\"$name\"]"
    version=$(jq -r "${jq_path}.version" "$MANIFEST")
    method=$(jq -r "${jq_path}.update.method // \"manual\"" "$MANIFEST")
    printf "  %s (%s):\n" "$name" "$version"
    entry_enabled "$jq_path" || {
      printf "    disabled\n"
      continue
    }
    if check_update "$jq_path" "$version" "$method"; then
      clear_tool runtimes "$name"
      version=$(jq -r "${jq_path}.version" "$MANIFEST")
    fi
    probe=$(jq -r "${jq_path}.probe // \"\"" "$MANIFEST")
    [ -z "$probe" ] && probe="$RUNTIMES/$name/bin/$name"
    if [ ! -e "$probe" ]; then
      printf "    install: %s\n" "$version"
      run_install "$jq_path" "$version" || {
        printf "    error: runtime install failed\n"
        FAILURES=$((FAILURES + 1))
      }
    else printf "    installed\n"; fi
  done
fi

printf "\n=== Binary tools ===\n"
if section_empty binary; then printf "  (none configured)\n"; else
  for name in $(jq -r '.binary | keys[]' "$MANIFEST"); do
    jq_path=".binary[\"$name\"]"
    version=$(jq -r "${jq_path}.version" "$MANIFEST")
    method=$(jq -r "${jq_path}.update.method // \"manual\"" "$MANIFEST")
    printf "  %s (%s):\n" "$name" "$version"
    entry_enabled "$jq_path" || {
      printf "    disabled\n"
      continue
    }
    if check_update "$jq_path" "$version" "$method"; then
      clear_tool binary "$name"
      version=$(jq -r "${jq_path}.version" "$MANIFEST")
    fi
    if ! has_bin "$name"; then
      printf "    install: %s\n" "$version"
      run_install "$jq_path" "$version" || {
        printf "    error: install failed\n"
        FAILURES=$((FAILURES + 1))
      }
    else printf "    installed\n"; fi
    write_shims "$jq_path"
  done
fi

printf "\n=== Custom tools ===\n"
if section_empty custom; then printf "  (none configured)\n"; else
  for name in $(jq -r '.custom | keys[]' "$MANIFEST"); do
    jq_path=".custom[\"$name\"]"
    version=$(jq -r "${jq_path}.version" "$MANIFEST")
    method=$(jq -r "${jq_path}.update.method // \"manual\"" "$MANIFEST")
    printf "  %s (%s):\n" "$name" "$version"
    entry_enabled "$jq_path" || {
      printf "    disabled\n"
      continue
    }
    requires_satisfied "$jq_path" || continue
    if check_update "$jq_path" "$version" "$method"; then
      clear_tool custom "$name"
      version=$(jq -r "${jq_path}.version" "$MANIFEST")
    fi
    if ! has_bin "$name"; then
      printf "    install: %s\n" "$version"
      run_install "$jq_path" "$version" || {
        printf "    error: install failed\n"
        FAILURES=$((FAILURES + 1))
      }
    else printf "    installed\n"; fi
    write_shims "$jq_path"
  done
fi

printf "\n=== Language servers (lsp) ===\n"
if section_empty lsp; then printf "  (none configured)\n"; else
  for name in $(jq -r '.lsp | keys[]' "$MANIFEST"); do
    jq_path=".lsp[\"$name\"]"
    version=$(jq -r "${jq_path}.version" "$MANIFEST")
    method=$(jq -r "${jq_path}.update.method // \"manual\"" "$MANIFEST")
    install_method=$(jq -r "${jq_path}.method // \"binary\"" "$MANIFEST")
    printf "  %s (%s, via %s):\n" "$name" "$version" "$install_method"
    entry_enabled "$jq_path" || {
      printf "    disabled\n"
      continue
    }
    requires_satisfied "$jq_path" || continue
    if check_update "$jq_path" "$version" "$method"; then
      clear_tool lsp "$name"
      version=$(jq -r "${jq_path}.version" "$MANIFEST")
    fi
    primary=$(jq -r "${jq_path}.primary // \"$name\"" "$MANIFEST")
    if has_bin "$primary"; then
      printf "    installed\n"
      write_shims "$jq_path"
      continue
    fi
    printf "    install: %s\n" "$version"
    case "$install_method" in
      binary)
        run_install "$jq_path" "$version" || {
          printf "    error: install failed\n"
          FAILURES=$((FAILURES + 1))
        }
        ;;
      go)
        if ! command -v go >/dev/null 2>&1; then
          printf "    skipped: go not available\n"
          continue
        fi
        pkg=$(jq -r "${jq_path}.package" "$MANIFEST")
        pkg="${pkg//\$\{VERSION\}/$version}"
        timeout "$INSTALL_TIMEOUT" go install "$pkg" || {
          printf "    error: go install failed\n"
          FAILURES=$((FAILURES + 1))
        }
        ;;
      *) printf "    error: unknown install method '%s'\n" "$install_method" ;;
    esac
    write_shims "$jq_path"
  done
fi

printf "\n=== System packages (apt) ===\n"
if section_empty apt; then printf "  (none configured)\n"; else
  apt_list=""
  for name in $(jq -r '.apt | keys[]' "$MANIFEST"); do
    entry_enabled ".apt[\"$name\"]" || {
      printf "  %s: disabled\n" "$name"
      continue
    }
    if ! command -v "$name" >/dev/null 2>&1 && ! dpkg -s "$name" >/dev/null 2>&1; then
      apt_list="$apt_list $name"
    else printf "  %s: installed\n" "$name"; fi
  done
  if [ -n "$apt_list" ]; then
    if [ "$(id -u)" -ne 0 ]; then
      printf '  WARNING: apt packages require root (set user: "0:0"); skipping:%s\n' "$apt_list"
      FAILURES=$((FAILURES + 1))
    else
      printf "  installing:%s\n" "$apt_list"
      apt_log=$(mktemp)
      # shellcheck disable=SC2086
      if apt-get -o Acquire::http::Timeout=60 -o Acquire::https::Timeout=60 update -qq \
        && apt-get -o Acquire::http::Timeout=60 -o Acquire::https::Timeout=60 install -y -qq --no-install-recommends $apt_list >"$apt_log" 2>&1; then
        tail -3 "$apt_log"
      else
        tail -3 "$apt_log"
        printf "  error: apt install failed:%s\n" "$apt_list"
        FAILURES=$((FAILURES + 1))
      fi
      rm -f "$apt_log"
      rm -rf /var/lib/apt/lists/*
    fi
  fi
fi

if [ "$FAILURES" -gt 0 ]; then
  printf "\n[%s] Tool setup complete with %d failure(s)\n" "$(date -Iseconds)" "$FAILURES"
  exit 1
fi
printf "\n[%s] Tool setup complete\n" "$(date -Iseconds)"
