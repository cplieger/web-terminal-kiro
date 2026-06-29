#!/bin/sh
# Deterministic PTY output fixture for scroll testing (brick 4). vibecli runs
# its command as `<KIRO_CLI_PATH> chat`, so this ignores its args. It bursts
# 120 numbered lines (enough to overflow the viewport and accrue scrollback),
# then emits one line every 0.4s forever so the "holding holds while new output
# arrives" behavior can be observed live. Not used in production.
i=1
while [ "$i" -le 120 ]; do
  printf 'emitter line %d -- the quick brown fox jumps over the lazy dog\r\n' "$i"
  i=$((i + 1))
done
while true; do
  printf 'emitter line %d -- the quick brown fox jumps over the lazy dog\r\n' "$i"
  i=$((i + 1))
  sleep 0.4
done
