#!/usr/bin/env bash
# Local dev build of vibecli against the LOCAL (working-tree) vterm, for the
# rebuild effort (docs in vterm/docs/REBUILD.md). Produces ./vibecli-dev-bin
# with static assets embedded, built from the sibling ../vterm checkout instead
# of the published Go module / npm package. Deploy with scripts/dev-deploy.sh.
#
# Not for CI or release. go.work and vibecli-dev-bin are gitignored.
set -euo pipefail
cd "$(dirname "$0")/.."
VTERM="../vterm"
PKG="static-src/node_modules/@cplieger/vterm/src"

echo "[1/5] go.work -> local ../vterm"
cat > go.work <<'EOF'
go 1.26.4

use .
use ../vterm
EOF

echo "[2/5] overlay local vterm TS into the bundler-resolved package"
mkdir -p "$PKG"
find "$PKG" -maxdepth 1 -name '*.ts' -delete
for f in "$VTERM"/web/src/*.ts; do
  case "$f" in
    *.test.ts | *fuzz* | *fc-strict-setup*) continue ;;
  esac
  cp "$f" "$PKG/"
done

echo "[3/5] tsgo: app + vterm lib -> static/"
tsgo --project static-src/tsconfig.json
tsgo --module ESNext --target ESNext --moduleResolution bundler \
  --outDir static/vendor/cplieger-vterm \
  --rootDir "$PKG" --skipLibCheck --strict "$PKG"/*.ts

echo "[4/6] fonts (Monaspace Nerd Font, cached)"
FONT_CACHE="${HOME}/.cache/vibecli-fonts"
FONT_VER="v3.4.0"
mkdir -p "$FONT_CACHE" static/vendor/fonts
if [ ! -f "$FONT_CACHE/MonaspiceNeNerdFontMono-Regular.otf" ]; then
  echo "  downloading Monaspace ${FONT_VER}..."
  curl -fsSL "https://github.com/ryanoasis/nerd-fonts/releases/download/${FONT_VER}/Monaspace.tar.xz" \
    | tar -xJ -C "$FONT_CACHE" \
        MonaspiceNeNerdFontMono-Regular.otf \
        MonaspiceNeNerdFontMono-Bold.otf \
        MonaspiceNeNerdFontMono-Italic.otf \
        MonaspiceNeNerdFontMono-BoldItalic.otf
fi
cp "$FONT_CACHE"/MonaspiceNeNerdFontMono-*.otf static/vendor/fonts/

echo "[5/6] css bundle"
: > static/style.css
while IFS= read -r line || [ -n "$line" ]; do
  case "$line" in '' | \#*) continue ;; esac
  cat "static-src/css/${line}" >> static/style.css
done < static-src/css/MANIFEST

echo "[6/6] go build (CGO off, linux/amd64 host = container arch)"
CGO_ENABLED=0 go build -trimpath -o vibecli-dev-bin .
echo "OK -> $(pwd)/vibecli-dev-bin ($(du -h vibecli-dev-bin | cut -f1))"
