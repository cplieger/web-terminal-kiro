#!/usr/bin/env bash
# Local dev build of web-terminal-kiro against the LOCAL (working-tree) engine + UI, for
# the rebuild/restructure effort (docs in web-terminal-engine/docs/REBUILD.md +
# RESTRUCTURE.md). Produces ./web-terminal-kiro-dev-bin with static assets embedded,
# built from the sibling ../web-terminal-engine (engine) and ../web-terminal-ui (UI) checkouts
# instead of the published Go module / npm packages. Deploy with
# scripts/dev-deploy.sh.
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
  echo "error: $TSC not found — run 'cd static-src && npm install' first" >&2
  exit 1
}

echo "[1/6] go.work -> local engine (replace published module with ${ENGINE_DIR})"
# Mirror go.mod's go directive and engine module path so neither can drift (a
# hardcoded version here broke the build when go.mod moved to a newer patch; a
# hardcoded /v2 module path silently no-opped the replace after the v3 bump).
GO_DIRECTIVE="$(sed -n 's/^go /go /p' go.mod | head -n1)"
ENGINE_MOD="$(sed -n 's|.*\(github.com/cplieger/web-terminal-engine/v[0-9]*\) .*|\1|p' go.mod | head -n1)"
[ -n "$ENGINE_MOD" ] || {
  echo "error: engine module path not found in go.mod" >&2
  exit 1
}
cat >go.work <<EOF
${GO_DIRECTIVE}

use .

replace ${ENGINE_MOD} => ${ENGINE_DIR}
EOF

echo "[2/6] overlay local engine + UI TS into the bundler-resolved packages"
rm -rf "$ENGINE_PKG/src" "$UI_PKG/src"
mkdir -p "$ENGINE_PKG/src" "$UI_PKG/src"
cp "$ENGINE_DIR/web/package.json" "$ENGINE_PKG/package.json"
for f in "$ENGINE_DIR"/web/src/*.ts; do
  case "$f" in *.test.ts | *fuzz* | *fc-strict-setup*) continue ;; esac
  cp "$f" "$ENGINE_PKG/src/"
done
cp "$UI_DIR/package.json" "$UI_PKG/package.json"
# The UI ships a nested src tree (src/kernel/, src/features/) since v3, so copy
# recursively, preserving subdirectories and excluding tests.
(cd "$UI_DIR/src" && find . -name '*.ts' ! -name '*.test.ts' ! -name 'fc-strict-setup.ts' -print0) \
  | while IFS= read -r -d '' f; do
    mkdir -p "$UI_PKG/src/$(dirname "$f")"
    cp "$UI_DIR/src/$f" "$UI_PKG/src/$f"
  done

echo "[3/6] tsc: app -> static/app.js (resolves @cplieger/web-terminal-ui)"
"$TSC" --project static-src/tsconfig.json

echo "[4/6] tsc: engine + UI libs -> static/vendor/"
rm -rf static/vendor/cplieger-web-terminal-engine static/vendor/cplieger-web-terminal-ui
"$TSC" --module ESNext --target ESNext --moduleResolution bundler \
  --outDir static/vendor/cplieger-web-terminal-engine \
  --rootDir "$ENGINE_PKG/src" --skipLibCheck --strict "$ENGINE_PKG/src"/*.ts
# Compile the whole nested UI src tree (index.ts + presets.ts + kernel/ +
# features/); find collects every .ts (the overlay already excluded tests).
mapfile -t ui_ts < <(find "$UI_PKG/src" -name '*.ts')
"$TSC" --module ESNext --target ESNext --moduleResolution bundler \
  --outDir static/vendor/cplieger-web-terminal-ui \
  --rootDir "$UI_PKG/src" --skipLibCheck --strict "${ui_ts[@]}"

echo "[5/6] fonts (Monaspace Nerd Font, cached) + CSS bundle (from UI package)"
FONT_CACHE="${HOME}/.cache/web-terminal-kiro-fonts"
# Single source of truth: the Dockerfile's Renovate-managed NERDFONT_* ARGs.
FONT_VER="$(sed -n 's/^ARG NERDFONT_VERSION=//p' Dockerfile)"
FONT_SHA256="$(sed -n 's/^ARG NERDFONT_SHA256=//p' Dockerfile)"
: "${FONT_VER:?failed to parse NERDFONT_VERSION from Dockerfile}"
: "${FONT_SHA256:?failed to parse NERDFONT_SHA256 from Dockerfile}"
mkdir -p "$FONT_CACHE" static/vendor/fonts
if [ ! -f "$FONT_CACHE/MonaspiceNeNerdFontMono-Regular.otf" ]; then
  echo "  downloading Monaspace ${FONT_VER}..."
  mona_tmp="$(mktemp)"
  curl --proto '=https' --tlsv1.2 -fsSL \
    "https://github.com/ryanoasis/nerd-fonts/releases/download/${FONT_VER}/Monaspace.tar.xz" \
    -o "$mona_tmp"
  printf '%s  %s\n' "$FONT_SHA256" "$mona_tmp" | sha256sum -c -
  tar -xJ -C "$FONT_CACHE" -f "$mona_tmp" \
    MonaspiceNeNerdFontMono-Regular.otf \
    MonaspiceNeNerdFontMono-Bold.otf \
    MonaspiceNeNerdFontMono-Italic.otf \
    MonaspiceNeNerdFontMono-BoldItalic.otf
  rm -f "$mona_tmp"
fi
cp "$FONT_CACHE"/MonaspiceNeNerdFontMono-*.otf static/vendor/fonts/

: >static/style.css
while IFS= read -r line || [ -n "$line" ]; do
  case "$line" in '' | \#*) continue ;; esac
  cat "$UI_DIR/css/${line}" >>static/style.css
done <"$UI_DIR/css/MANIFEST"

echo "[6/6] go build (CGO off, linux/amd64 host = container arch)"
CGO_ENABLED=0 go build -trimpath -o web-terminal-kiro-dev-bin .
echo "OK -> $(pwd)/web-terminal-kiro-dev-bin ($(du -h web-terminal-kiro-dev-bin | cut -f1))"
