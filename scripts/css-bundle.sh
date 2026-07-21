#!/bin/sh
# Concatenate the UI package's per-feature CSS splits (listed in
# css/MANIFEST; blank lines and #-comments skipped, unterminated
# final line handled) into the served bundle.
# Usage: css-bundle.sh <ui-css-dir> <out-file>
set -eu
css_dir="${1:?usage: css-bundle.sh <ui-css-dir> <out-file>}"
out="${2:?usage: css-bundle.sh <ui-css-dir> <out-file>}"
: >"$out"
while IFS= read -r line || [ -n "$line" ]; do
  case "$line" in '' | \#*) continue ;; esac
  cat "${css_dir}/${line}" >>"$out"
done <"${css_dir}/MANIFEST"
