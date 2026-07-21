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
while IFS= read -r line || [ -n "$line" ]; do
  case "$line" in '' | \#*) continue ;; esac
  case "$line" in
    /* | ../* | */../* | */.. | ..)
      printf 'css-bundle: MANIFEST entry escapes css dir, refusing: %s\n' "$line" >&2
      exit 1
      ;;
  esac
  cat "${css_dir}/${line}" >>"$tmp"
done <"${css_dir}/MANIFEST"
mv "$tmp" "$out"
trap - EXIT HUP INT TERM
