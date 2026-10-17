#!/usr/bin/env bash
# Runs each *_test.sh as its own process, so stubs, traps and shell options
# cannot leak, and runs all of them after a failure. This filename opts into
# cplieger/ci's shell-ci.yaml and its sync of lib.sh and harness_test.sh. Scope:
# entrypoint.sh branches a healthy boot never takes, and tests/image-test.sh's
# log and profile-list matching, since that suite runs only in CI.
set -u

cd -- "$(dirname -- "$0")" || exit 1

failed=0
ran=0
for t in ./*_test.sh; do
  # An unmatched glob expands to itself: an empty suite must fail, not pass.
  if [ ! -f "$t" ]; then
    printf 'harness error: no *_test.sh found in %s\n' "$PWD" >&2
    exit 1
  fi
  printf '=== %s\n' "$(basename "$t")"
  ran=$((ran + 1))
  bash "$t" || failed=$((failed + 1))
  printf '\n'
done

if [ "$failed" -ne 0 ]; then
  printf 'FAILED: %d of %d shell test files failed\n' "$failed" "$ran" >&2
  exit 1
fi
printf 'all %d shell test files passed\n' "$ran"
