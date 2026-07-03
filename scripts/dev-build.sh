#!/usr/bin/env bash
# Local dev build of vibecli against the LOCAL (working-tree) engine + UI, for
# the rebuild/restructure effort (docs in web-terminal-engine/docs/REBUILD.md +
# RESTRUCTURE.md). Produces ./vibecli-dev-bin with static assets embedded,
# built from the sibling ../web-terminal-engine (engine) and ../web-terminal-ui (UI) checkouts
# instead of the published Go module / npm packages. Deploy with
# scripts/dev-deploy.sh.
#
# Not for CI or release. go.work and vibecli-dev-bin are gitignored.
# Override the sibling checkouts with ENGINE_DIR=... / UI_DIR=...
set -euo pipefail
cd "$(dirname "$0")/.."
ENGINE_DIR="${ENGINE_DIR:-../web-terminal-engine}"
UI_DIR="${UI_DIR:-../web-terminal-ui}"
ENGINE_PKG="static-src/node_modules/@cplieger/web-terminal-engine"
UI_PKG="static-src/node_modules/@cplieger/web-terminal-ui"

echo "[1/6] go.work -> local engine (replace; engine module is unpublished)"
cat >go.work <<'EOF'
go 1.26.4

use .

replace github.com/cplieger/web-terminal-engine/v2 => ../web-terminal-engine
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
(cd "$UI_DIR/src" && find . -name '*.ts' ! -name '*.test.ts' ! -name 'fc-strict-setup.ts' -print0) |
  while IFS= read -r -d '' f; do
    mkdir -p "$UI_PKG/src/$(dirname "$f")"
    cp "$UI_DIR/src/$f" "$UI_PKG/src/$f"
  done

echo "[3/6] tsgo: app -> static/app.js (resolves @cplieger/web-terminal-ui)"
tsgo --project static-src/tsconfig.json

echo "[4/6] tsgo: engine + UI libs -> static/vendor/"
rm -rf static/vendor/cplieger-web-terminal-engine static/vendor/cplieger-web-terminal-ui
tsgo --module ESNext --target ESNext --moduleResolution bundler \
  --outDir static/vendor/cplieger-web-terminal-engine \
  --rootDir "$ENGINE_PKG/src" --skipLibCheck --strict "$ENGINE_PKG/src"/*.ts
# Compile the whole nested UI src tree (index.ts + presets.ts + kernel/ +
# features/); find collects every .ts (the overlay already excluded tests).
mapfile -t ui_ts < <(find "$UI_PKG/src" -name '*.ts')
tsgo --module ESNext --target ESNext --moduleResolution bundler \
  --outDir static/vendor/cplieger-web-terminal-ui \
  --rootDir "$UI_PKG/src" --skipLibCheck --strict "${ui_ts[@]}"

echo "[5/6] fonts (Monaspace Nerd Font, cached) + CSS bundle (from UI package)"
FONT_CACHE="${HOME}/.cache/vibecli-fonts"
FONT_VER="v3.4.0"
# Keep in lockstep with NERDFONT_SHA256 in the Dockerfile (Monaspace.tar.xz, this tag).
FONT_SHA256="5fdb97828e1a23fd28ea5ed0e7d15cdebb77ef079aaa48b93f1526764b40ef8c"
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
CGO_ENABLED=0 go build -trimpath -o vibecli-dev-bin .
echo "OK -> $(pwd)/vibecli-dev-bin ($(du -h vibecli-dev-bin | cut -f1))"
