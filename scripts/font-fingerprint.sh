#!/bin/sh
# Content-address the web fonts the CSS bundle names: hash each referenced file, rename it
# to <stem>.<8 hex><ext>, and rewrite every /vendor/fonts/ reference in the bundle and in
# each extra file. The bundle is served no-cache, so a new bundle points at the new font
# URL on the next load. The page is an extra file because its font preload gates the first
# frame, so a stale href there costs a round trip on a 404.
#
# Usage: font-fingerprint.sh <fonts-dir> <css-file> [extra-file...]
set -eu

if [ $# -lt 2 ]; then
  printf 'font-fingerprint: usage: font-fingerprint.sh <fonts-dir> <css-file> [extra-file...]\n' >&2
  exit 2
fi
fonts_dir="$1"
css="$2"
# Shift only the fonts dir, so "$@" is the CSS plus every extra file: each one carries
# references the rename has to follow.
shift 1

# The loop below renames inside this directory, so refuse a symlinked or non-directory
# root before writing anything.
if [ -L "$fonts_dir" ] || [ ! -d "$fonts_dir" ]; then
  printf 'font-fingerprint: fonts dir is a symlink or not a directory, refusing: %s\n' "$fonts_dir" >&2
  exit 1
fi
fonts_root=$(realpath "$fonts_dir")
for target in "$@"; do
  if [ ! -f "$target" ]; then
    printf 'font-fingerprint: target is not a regular file, refusing: %s\n' "$target" >&2
    exit 1
  fi
done

# Anchored on the url( or attribute quote that OPENS the reference, because a prose
# mention of the path in a CSS comment is not a face; tolerant of the quoting and
# whitespace inside it, which belong to the published UI's own CSS; and grep -o rather
# than a sed substitution because two url()s can share a line. A reference this misses
# keeps naming a file the rename already moved, which is the one silent failure mode.
font_refs() {
  {
    grep -oE "url\([[:space:]]*[\"']?/vendor/fonts/[^\"')[:space:]]+" "$1" || true
    grep -oE "=[\"']/vendor/fonts/[^\"']+" "$1" || true
  } | sed 's|.*/vendor/fonts/||' | sort -u
}

# Escape the regex metacharacters a filename can legitimately carry, so the rewrite below
# matches the name rather than a pattern.
escape_re() {
  printf '%s' "$1" | sed 's|[.[\*^$/]|\\&|g'
}

names=$(font_refs "$css")
if [ -z "$names" ]; then
  printf 'font-fingerprint: the bundle names no /vendor/fonts asset, refusing\n' >&2
  exit 1
fi

stamped=0
for name in $names; do
  # A URL component reaches a filesystem path here, so refuse anything but a plain
  # filename rather than resolving it.
  case "$name" in
    */* | '' | . | ..)
      printf 'font-fingerprint: url names a path rather than a file, refusing: %s\n' "$name" >&2
      exit 1
      ;;
  esac
  # A name already carrying a hash is left alone, so a second run over the same tree is a
  # no-op rather than stamping a hash onto a hash.
  if printf '%s' "$name" | grep -Eq '\.[0-9a-f]{8}\.[^.]+$'; then
    continue
  fi
  src="${fonts_dir}/${name}"
  # An asset the fetch did not produce would reach the reader as the SPA fallback under a
  # font URL, so this fails the build rather than skipping.
  if [ ! -f "$src" ]; then
    printf 'font-fingerprint: the bundle names an asset the build did not produce: %s\n' "$src" >&2
    exit 1
  fi
  resolved=$(realpath "$src")
  case "$resolved" in
    "${fonts_root}"/*) ;;
    *)
      printf 'font-fingerprint: asset resolves outside the fonts dir, refusing: %s\n' "$name" >&2
      exit 1
      ;;
  esac
  hash=$(sha256sum "$src" | cut -c1-8)
  case "$hash" in
    [0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]) ;;
    *)
      printf 'font-fingerprint: sha256sum did not yield a hash for %s\n' "$name" >&2
      exit 1
      ;;
  esac
  ext=""
  stem="$name"
  case "$name" in
    *.*)
      ext=".${name##*.}"
      stem="${name%.*}"
      ;;
  esac
  hashed="${stem}.${hash}${ext}"
  mv "$src" "${fonts_dir}/${hashed}"
  name_re=$(escape_re "$name")
  # The trailing delimiter is what stops a name being rewritten inside a longer one, and
  # it admits whitespace because `url( x )` is legal CSS.
  for target in "$@"; do
    tmp=$(mktemp "${target}.XXXXXX")
    sed "s|/vendor/fonts/${name_re}\([\"')[:space:]]\)|/vendor/fonts/${hashed}\1|g" "$target" >"$tmp"
    mv "$tmp" "$target"
  done
  stamped=$((stamped + 1))
done

# An unstamped reference left here would 404 at runtime, so the rewrite must have reached
# every one -- including a page that preloads a face the bundle no longer declares.
for target in "$@"; do
  missed=$(font_refs "$target" | grep -Ev '\.[0-9a-f]{8}\.[^.]+$' || true)
  if [ -n "$missed" ]; then
    printf 'font-fingerprint: %s still names an unstamped asset: %s\n' \
      "$target" "$(printf '%s' "$missed" | tr '\n' ' ')" >&2
    exit 1
  fi
done

printf 'font-fingerprint: %s assets content-addressed in %s\n' "$stamped" "$fonts_dir"
