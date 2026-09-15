#!/usr/bin/env bash
# Local dev build of web-terminal-kiro against the LOCAL (working-tree) engine + UI:
# produces ./web-terminal-kiro-dev-bin with static assets embedded, built from the
# sibling ../web-terminal-engine (engine) and ../web-terminal-ui (UI) checkouts
# instead of the published Go module / npm packages — the way to try unpublished
# engine/UI changes against the real app. Run the binary directly
# (WORK_DIR=... ./web-terminal-kiro-dev-bin; see CONTRIBUTING "Local dev setup").
#
# Not for CI or release. go.work and web-terminal-kiro-dev-bin are gitignored.
# Override the sibling checkouts with ENGINE_DIR=... / UI_DIR=...
set -euo pipefail
cd "$(dirname "$0")/.."
ENGINE_DIR="${ENGINE_DIR:-../web-terminal-engine}"
UI_DIR="${UI_DIR:-../web-terminal-ui}"
ENGINE_PKG="static-src/node_modules/@cplieger/web-terminal-engine"
UI_PKG="static-src/node_modules/@cplieger/web-terminal-ui"
# The TS7 native compiler comes from static-src's @typescript/native devDep.
TSC="static-src/node_modules/.bin/tsc"
[ -x "$TSC" ] || {
  printf "error: %s not found — run 'cd static-src && npm install' first\n" "$TSC" >&2
  exit 1
}
# Reads one ARG's value, stopping at whitespace or a `#` comment (a trailing
# `# <name> <version>` trailer follows the digest pins below).
dockerfileArg() {
  sed -n "s/^ARG $1=\\([^[:space:]#]*\\).*/\\1/p" Dockerfile
}

# fetch_pinned <dep> <version-arg> <cache-name> <dest-dir>
#
# Downloads and verifies every asset the Dockerfile pins for one `# repin:`
# dep, from the markers themselves rather than from a second copy of the list:
# the URL template, the destination name and the sha ARG all come off the same
# two lines Renovate's postUpgradeTask rewrites, so this cannot drift from what
# the image fetches. A marker's `dest=` token names the destination file (it is
# how two projects both shipping a file called LICENSE are disambiguated);
# absent, the URL's own basename is used.
#
# Keying on the markers is what makes the asset list unable to drift from the
# pins: an asset with no marker has no sha256 ARG at all, so it could never be
# verified, and the image build refuses it on the case statement's `*)` arm.
fetch_pinned() {
  local dep=$1 version_arg=$2 cache_name=$3 dest_dir=$4
  local version
  version=$(dockerfileArg "$version_arg")
  [ -n "$version" ] || {
    echo "error: could not read ARG $version_arg from Dockerfile" >&2
    exit 1
  }

  # One "<dest> <url-template> <sha-arg>" row per marker for this dep. The ARG
  # must sit on the line immediately below its marker, which is the pairing
  # repin-sha.sh relies on too.
  local rows
  rows=$(awk -v dep="$dep" '
    /^#[[:space:]]*repin:/ {
      url = ""; dest = ""; d = ""
      for (i = 1; i <= NF; i++) {
        if ($i ~ /^dep=/)  { d = substr($i, 5) }
        if ($i ~ /^url=/)  { url = substr($i, 5) }
        if ($i ~ /^dest=/) { dest = substr($i, 6) }
      }
      if (d == dep && url != "") {
        pending_url = url; pending_dest = dest
      }
      next
    }
    pending_url != "" {
      if ($0 ~ /^ARG [A-Za-z_][A-Za-z0-9_]*=/) {
        name = $0; sub(/^ARG /, "", name); sub(/=.*/, "", name)
        if (pending_dest == "") { pending_dest = pending_url; sub(/^.*\//, "", pending_dest) }
        printf "%s %s %s\n", pending_dest, pending_url, name
      }
      pending_url = ""
      next
    }
  ' Dockerfile)
  [ -n "$rows" ] || {
    echo "error: no '# repin: dep=$dep' marker in Dockerfile" >&2
    exit 1
  }

  # Cache key: the version AND a digest of every pin, so a repin at an
  # unchanged version still misses the cache. The `.complete` marker is written
  # only after every asset downloaded AND verified, so an interrupted fetch
  # self-heals with a full retry instead of embedding a partial asset.
  local dests=() urls=() shas=() combined=""
  local dest url_tmpl sha_arg sha
  while read -r dest url_tmpl sha_arg; do
    [ -n "$dest" ] || continue
    sha=$(dockerfileArg "$sha_arg")
    [ -n "$sha" ] || {
      echo "error: could not read ARG $sha_arg from Dockerfile" >&2
      exit 1
    }
    dests+=("$dest")
    urls+=("${url_tmpl//\{version\}/$version}")
    shas+=("$sha")
    combined="${combined}${sha}"
  done <<<"$rows"

  local key cache
  key=$(printf '%s\n%s' "$version" "$combined" | sha256sum | cut -c1-16)
  cache="${HOME}/.cache/${cache_name}/${version}-${key}"

  # Re-verify the whole cache before reusing it. `.complete` records that a
  # download verified ONCE, which is a different claim from "these bytes are
  # still the pinned ones": a cache entry can change after the marker is written
  # (interrupted external tooling, disk corruption, another process under the
  # same uid), and non-empty is no evidence at all — the WRONG font is non-empty.
  # A mismatch discards the whole keyed directory rather than repairing one
  # entry, because whatever changed one is not known to have stopped at one. The
  # image build verifies every byte it fetches, so a dev build trusting a warm
  # cache would be the single path that embeds an unverified font in the
  # //go:embed static tree.
  local need=0 i
  [ -f "$cache/.complete" ] || need=1
  if [ "$need" = 0 ]; then
    for i in "${!dests[@]}"; do
      [ -s "$cache/${dests[$i]}" ] || {
        need=1
        break
      }
      printf '%s  %s\n' "${shas[$i]}" "$cache/${dests[$i]}" | sha256sum -c --status - || {
        printf '  cached %s %s no longer matches its pin (%s); discarding the cache\n' \
          "$dep" "$version" "${dests[$i]}" >&2
        need=1
        break
      }
    done
  fi
  if [ "$need" = 1 ]; then
    echo "  downloading $dep $version (${#dests[@]} assets)..."
    rm -rf "$cache"
    mkdir -p "$cache"
    for i in "${!dests[@]}"; do
      curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL \
        --connect-timeout 20 --max-time 300 --retry 3 --retry-delay 5 \
        -o "$cache/${dests[$i]}" "${urls[$i]}"
      printf '%s  %s\n' "${shas[$i]}" "$cache/${dests[$i]}" | sha256sum -c -
    done
    : >"$cache/.complete"
  fi

  # Copy rather than link, and let the caller own the destination's lifetime: a
  # dev build reuses the working tree, so an asset dropped from the Dockerfile
  # must not survive there and keep landing in the //go:embed static tree.
  mkdir -p "$dest_dir"
  for i in "${!dests[@]}"; do
    cp "$cache/${dests[$i]}" "$dest_dir/${dests[$i]}"
    # Verify what was actually COPIED, not what passed a check a moment ago: the
    # reuse check above and this copy are two operations on a path anything
    # sharing the uid can rewrite in between, and only the destination's bytes
    # reach the embedded tree.
    printf '%s  %s\n' "${shas[$i]}" "$dest_dir/${dests[$i]}" | sha256sum -c --quiet -
  done
}

# Validate every required checkout input BEFORE go.work is written or the
# destructive node_modules overlay below starts, so a missing sibling checkout
# or a typo'd ENGINE_DIR/UI_DIR override fails cleanly instead of half-deleting
# the installed packages (repaired only by a fresh npm install).
for required in \
  "$ENGINE_DIR/web/package.json" "$ENGINE_DIR/web/wire-compatibility.json" \
  "$UI_DIR/package.json" "$UI_DIR/css/MANIFEST"; do
  [ -f "$required" ] || {
    printf 'error: required local checkout input is not a regular file: %s\n' "$required" >&2
    exit 1
  }
done
for required in "$ENGINE_DIR/web/src" "$UI_DIR/src"; do
  [ -d "$required" ] || {
    printf 'error: required local checkout source directory not found: %s\n' "$required" >&2
    exit 1
  }
done
# A src directory that EXISTS but holds no file the overlay would copy is the
# same failure the image build gates on (engine-src-empty / ui-src-empty), and
# it has to be caught here too: step [2/7] deletes both installed package src
# trees before copying, so an empty (or test-only) source tree otherwise slips
# past preflight and leaves node_modules broken — cp exits on a literal
# unmatched *.ts, or tsc exits 1 with no input files, in both cases only after
# the destructive overlay. Match the basename, not the full path (so a checkout
# path containing 'fuzz' is not itself excluded); src/test-helpers/ is pruned as a
# DIRECTORY because recursion would otherwise pull test-support modules (which
# carry no *.test.ts basename) into the vendor emit and make the dev build depend
# on test-only code typechecking under --strict — the published tarball excludes
# them, so this keeps the local overlay matching what the image gets. The list
# captured here IS the list step [2/7] copies, so preflight and execution cannot
# drift.
mapfile -d '' -t engine_src < <(cd "$ENGINE_DIR/web/src" && find . \
  -type d -name 'test-helpers' -prune -o \
  -type f -name '*.ts' ! -name '*.test.ts' ! -name '*fuzz*' ! -name '*fc-strict-setup*' -print0)
[ "${#engine_src[@]}" -gt 0 ] || {
  printf 'error: engine-src-empty: no eligible *.ts under %s (wrong ENGINE_DIR or src layout change?)\n' \
    "$ENGINE_DIR/web/src" >&2
  exit 1
}
mapfile -d '' -t ui_src < <(cd "$UI_DIR/src" && find . \
  -type f -name '*.ts' ! -name '*.test.ts' ! -name '*fuzz*' ! -name '*fc-strict-setup*' -print0)
[ "${#ui_src[@]}" -gt 0 ] || {
  printf 'error: ui-src-empty: no eligible *.ts under %s (wrong UI_DIR or src layout change?)\n' \
    "$UI_DIR/src" >&2
  exit 1
}

printf '[1/7] go.work -> local engine (replace published module with %s)\n' "$ENGINE_DIR"
# Mirror go.mod's go directive and engine module path so neither can drift (a
# hardcoded version here broke the build when go.mod moved to a newer patch; a
# hardcoded /v2 module path silently no-opped the replace after the v3 bump).
GO_DIRECTIVE="$(sed -n 's/^go /go /p' go.mod | head -n1)"
[ -n "$GO_DIRECTIVE" ] || {
  printf 'error: go directive not found in go.mod\n' >&2
  exit 1
}
ENGINE_MOD="$(sed -n 's|.*\(github.com/cplieger/web-terminal-engine/v[0-9]*\) .*|\1|p' go.mod | head -n1)"
[ -n "$ENGINE_MOD" ] || {
  printf 'error: engine module path not found in go.mod\n' >&2
  exit 1
}
cat >go.work <<EOF
${GO_DIRECTIVE}

use .

replace ${ENGINE_MOD} => ${ENGINE_DIR}
EOF

printf '[2/7] overlay local engine + UI TS into the bundler-resolved packages\n'
rm -rf "$ENGINE_PKG/src" "$UI_PKG/src"
mkdir -p "$ENGINE_PKG/src" "$UI_PKG/src"
cp "$ENGINE_DIR/web/package.json" "$ENGINE_PKG/package.json"
# The wire-compatibility manifest is a PACKAGE-ROOT file, so the src overlay
# below does not carry it: without this copy the gate would read whatever the
# installed tarball shipped — stale for an unpublished local engine, which is
# exactly the case the gate exists for. The engine generates it from
# web/src/wire-manifest.ts, so a local checkout has it (preflight asserts so
# before the destructive overlay starts).
cp "$ENGINE_DIR/web/wire-compatibility.json" "$ENGINE_PKG/wire-compatibility.json"
# Copy the list captured in preflight (recursive, matching the UI loop below: the
# engine's src tree is flat today, but a future nested module must not be silently
# dropped — the emit assertions only check index.js, so the miss would surface as a
# runtime 404, not a build failure).
for f in "${engine_src[@]}"; do
  mkdir -p "$ENGINE_PKG/src/$(dirname "$f")"
  cp "$ENGINE_DIR/web/src/$f" "$ENGINE_PKG/src/$f"
done
cp "$UI_DIR/package.json" "$UI_PKG/package.json"
# The UI ships a nested src tree (src/kernel/, src/features/) since v3, so the
# captured list is recursive and preserves subdirectories.
for f in "${ui_src[@]}"; do
  mkdir -p "$UI_PKG/src/$(dirname "$f")"
  cp "$UI_DIR/src/$f" "$UI_PKG/src/$f"
done

# Wire-floor gate, mirroring the Dockerfile step after its vendor fetch: the Go
# half comes from go.work (the LOCAL engine) and the client half from the overlay
# above, so a dev build of an unpublished engine is exactly where the two floors
# can disagree. Without this the pairing fails at first connect (close 4002) with
# /api/health green and no build-time diagnostic. Built and then invoked, never
# `go run`, which collapses the gate's exit 2 ("the gate cannot read the client's
# declaration, do NOT bump a pin") into a plain 1. The client half is read from
# the engine's own manifest by the gate itself (encoding/json), so this script
# parses nothing.
wirecheck_bin="$(mktemp)"
# The gate's whole purpose is to EXIT non-zero (1 on a floor violation, 2 on a broken
# extraction), and under set -euo pipefail either exit -- like a failing go build --
# skips the rm below, leaking a multi-megabyte binary per failing run. Trap it, then
# disarm once the binary is removed: EXIT traps do not stack (a later step arming its
# own would silently replace this one), so no step may leave one armed past its use.
trap 'rm -f "$wirecheck_bin"' EXIT
go build -o "$wirecheck_bin" ./scripts/wirecheck
"$wirecheck_bin" -manifest "$ENGINE_PKG/wire-compatibility.json"
rm -f "$wirecheck_bin"
trap - EXIT

printf '[3/7] tsc: app -> static/app.js (resolves @cplieger/web-terminal-ui)\n'
# Drop the previous emit first so the assertion after step [4/7] observes THIS
# run's output: static/app.js is gitignored but persistent, so an outDir/rootDir
# change would otherwise leave a stale file that satisfies the check. The vendor
# dirs below already get the same treatment via rm -rf. (The image build is
# immune: .dockerignore keeps static/*.js out of the build context.)
rm -f static/app.js
"$TSC" --project static-src/tsconfig.json

printf '[4/7] tsc: engine + UI libs -> static/vendor/\n'
rm -rf static/vendor/cplieger-web-terminal-engine static/vendor/cplieger-web-terminal-ui
# Canonical recipe: scripts/vendor-tsc.sh, shared with the Dockerfile builder, so
# the dev binary and the image cannot end up compiled with different flags. It
# collects the whole tree recursively (the engine's is flat today; a future nested
# module must not be silently dropped) and carries the <label>-src-empty gate.
bash scripts/vendor-tsc.sh "$TSC" engine "$ENGINE_PKG/src" \
  static/vendor/cplieger-web-terminal-engine
bash scripts/vendor-tsc.sh "$TSC" ui "$UI_PKG/src" \
  static/vendor/cplieger-web-terminal-ui
# Assert the emit produced every module static/index.html loads: a tsconfig
# outDir/rootDir change or a lib src-layout move otherwise yields a clean
# build whose page 404s at runtime. Canonical recipe: scripts/assert-emit.sh,
# shared with the Dockerfile builder, and it DERIVES the target list from the
# page's own importmap rather than restating it here.
sh scripts/assert-emit.sh

printf '[5/7] fonts (Monaspace Neon NF + the Web Terminal Glyphs overlay, cached) + CSS bundle (from UI package)\n'
# Same sources, filenames and digests as the Dockerfile, derived from its own
# `# repin:` markers (fetch_pinned above), so neither the asset list nor the URL
# nor a pin can drift from the image build.
#
# Recreate the generated destination first, so an asset dropped from the marker
# list does not survive in the dev tree: the image build starts from a clean
# builder and fetches only the current members, but a dev build reuses the
# working tree and `cp` cannot delete what the list no longer names — the stale
# file would keep landing in the //go:embed static tree. The per-dep caches
# stay persistent.
rm -rf static/vendor/fonts
fetch_pinned githubnext/monaspace MONASPACE_VERSION \
  web-terminal-kiro-fonts static/vendor/fonts
fetch_pinned cplieger/web-terminal-glyphs WEB_TERMINAL_GLYPHS_VERSION \
  web-terminal-kiro-glyphs static/vendor/fonts

sh scripts/css-bundle.sh "$UI_DIR/css" static/style.css

printf '[6/7] cell-contract gate (the released cell contract vs the CSS this build serves)\n'
# Mirrors the Dockerfile step after its own CSS bundle: the overlay's glyphs are
# drawn for one cell, and here the CSS comes from a LOCAL UI checkout, which is
# exactly where it can move ahead of the released contract. Built and then
# invoked, never `go run`, which collapses the gate's exit 2 ("the gate is
# broken, do NOT bump a pin") into a plain 1.
fontcheck_bin="$(mktemp)"
trap 'rm -f "$fontcheck_bin"' EXIT
go build -o "$fontcheck_bin" ./scripts/fontcheck
"$fontcheck_bin" \
  -cell static/vendor/fonts/WebTerminalGlyphs-cell.json \
  -css static/style.css \
  -fonts static/vendor/fonts
rm -f "$fontcheck_bin"
trap - EXIT

printf '[7/7] go build (CGO off, host arch = image arch)\n'
CGO_ENABLED=0 go build -trimpath -o web-terminal-kiro-dev-bin .
printf 'OK -> %s/web-terminal-kiro-dev-bin (%s)\n' "$(pwd)" "$(du -h web-terminal-kiro-dev-bin | cut -f1)"
