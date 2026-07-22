#!/bin/sh
# Concatenate the UI package's per-feature CSS splits (listed in
# css/MANIFEST; blank lines and #-comments skipped, unterminated
# final line handled) into the served bundle.
# Usage: css-bundle.sh <ui-css-dir> <out-file>
set -eu
css_dir="${1:?usage: css-bundle.sh <ui-css-dir> <out-file>}"
out="${2:?usage: css-bundle.sh <ui-css-dir> <out-file>}"
# Assemble beside the destination and rename only after every manifest member
# was read, so a missing/unreadable CSS split fails the build without
# replacing the previously usable bundle with a partial file. mktemp in the
# output directory keeps the rename atomic.
tmp=$(mktemp "${out}.XXXXXX")
trap 'rm -f "$tmp"' EXIT HUP INT TERM
css_root=$(realpath "$css_dir")
# The per-entry regular-file guard below cannot protect the MANIFEST
# itself: opening a FIFO for the loop redirect blocks the build forever,
# the same crafted-tarball class the entry guard closes. [ -f ] follows
# symlinks, matching the entry semantics (in-tree symlink to a regular
# file passes; symlink to a FIFO refuses).
if [ ! -f "${css_dir}/MANIFEST" ]; then
  printf 'css-bundle: MANIFEST is missing or not a regular file, refusing: %s\n' "${css_dir}/MANIFEST" >&2
  exit 1
fi
while IFS= read -r line || [ -n "$line" ]; do
  case "$line" in '' | \#*) continue ;; esac
  case "$line" in
    /* | ../* | */../* | */.. | ..)
      printf 'css-bundle: MANIFEST entry escapes css dir, refusing: %s\n' "$line" >&2
      exit 1
      ;;
  esac
  # Resolve symlinks and re-assert containment: the literal guard above
  # cannot see a symlink shipped inside a crafted UI tarball.
  entry=$(realpath -e "${css_dir}/${line}") || {
    printf 'css-bundle: MANIFEST entry does not resolve: %s\n' "$line" >&2
    exit 1
  }
  case "$entry" in
    "${css_root}"/*) ;;
    *)
      printf 'css-bundle: MANIFEST entry resolves outside css dir, refusing: %s\n' "$line" >&2
      exit 1
      ;;
  esac
  # Containment alone still admits a FIFO/device/directory shipped in a
  # crafted UI tarball; cat on a FIFO blocks the build forever. Only regular
  # files (including resolved in-tree symlinks) are readable bundle members.
  if [ ! -f "$entry" ]; then
    printf 'css-bundle: MANIFEST entry is not a regular file, refusing: %s\n' "$line" >&2
    exit 1
  fi
  cat "$entry" >>"$tmp"
done <"${css_dir}/MANIFEST"
# An empty or fully-commented MANIFEST (a truncated/mis-published UI tarball)
# would otherwise install an empty bundle that nothing downstream catches.
[ -s "$tmp" ] || {
  printf 'css-bundle: assembled bundle is empty (empty or fully-commented MANIFEST?): %s\n' "${css_dir}/MANIFEST" >&2
  exit 1
}
mv "$tmp" "$out"
trap - EXIT HUP INT TERM
