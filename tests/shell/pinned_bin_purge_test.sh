#!/usr/bin/env bash
# Lint directives for this whole file, each against a stated guarantee:
#   SC2015 - `[ cond ] && ok "..." || no "..."` cannot mis-fire, because lib.sh's
#     ok/no return 0 unconditionally by design.
#   SC2016 - the `bash -c` scripts expand "$1" in the CHILD, so they must stay
#     single-quoted.
#   SC2034 - the variables set below are INPUTS to entrypoint.sh code that is
#     extracted and sourced at RUNTIME, so shellcheck cannot see the reads.
# shellcheck disable=SC2015,SC2016,SC2034
set -u

# shellcheck source-path=SCRIPTDIR
. "$(dirname -- "$0")/lib.sh"
new_workdir >/dev/null

load_function logfmt_value
load_function kiro_cli_data_dir
load_function purge_pinned_chat_cli_copies

setup() {
  ROOT=$(mktemp -d "$WORK/vol.XXXXXX")
  export XDG_DATA_HOME="$ROOT/share"
  RUN="$XDG_DATA_HOME/kiro-cli/run"
  mkdir -p "$RUN"
}

plant_pins() {
  : >"$RUN/chat-cli-2.22.0"
  : >"$RUN/.chat-cli-2.22.0.heartbeat"
  : >"$RUN/chat-cli-2.27.0"
  : >"$RUN/.chat-cli-2.27.0.heartbeat"
}

pins_left() {
  local n=0 f
  for f in chat-cli-2.22.0 .chat-cli-2.22.0.heartbeat chat-cli-2.27.0 .chat-cli-2.27.0.heartbeat; do
    [ -e "$RUN/$f" ] && n=$((n + 1))
  done
  printf '%s\n' "$n"
}

purge_logged() {
  purge_pinned_chat_cli_copies 2>"$WORK/purge.log" >/dev/null
}

setup
plant_pins
KIRO_SKIP_BINARY_PINNING=1 purge_logged
[ "$(pins_left)" -eq 0 ] && ok "copies and heartbeats removed when pinning is off" \
  || no "purge with pinning off" "$(pins_left) of 4 planted entries survived"
removed=$(grep -c 'msg="removed unused kiro-cli pinned binary"' "$WORK/purge.log")
[ "$removed" -eq 4 ] && grep -q 'entry=".chat-cli-2.22.0.heartbeat"' "$WORK/purge.log" \
  && ok "one info line per removed entry, naming it" \
  || no "removal log" "expected 4 named info lines, got $removed"

for gate in unset 0 true; do
  setup
  plant_pins
  if [ "$gate" = unset ]; then
    (
      unset KIRO_SKIP_BINARY_PINNING
      purge_logged
    )
  else
    KIRO_SKIP_BINARY_PINNING=$gate purge_logged
  fi
  [ "$(pins_left)" -eq 4 ] && [ ! -s "$WORK/purge.log" ] \
    && ok "gate $gate: run/ untouched and nothing logged" \
    || no "gate $gate" "$(pins_left) of 4 survived, or the purge logged"
done

setup
mkdir -p "$RUN/turn-markers" "$RUN/chat-cli-9.9.9"
: >"$RUN/turn-markers/m"
: >"$RUN/run-receipts"
: >"$RUN/chat-cli-notes"
: >"$RUN/.chat-cli-notes.heartbeat"
: >"$ROOT/bait"
ln -s "$ROOT/bait" "$RUN/chat-cli-8.8.8"
ln -s "$ROOT/bait" "$RUN/.chat-cli-8.8.8.heartbeat"
KIRO_SKIP_BINARY_PINNING=1 purge_logged
if [ -f "$RUN/turn-markers/m" ] && [ -e "$RUN/run-receipts" ] && [ -e "$RUN/chat-cli-notes" ] \
  && [ -e "$RUN/.chat-cli-notes.heartbeat" ]; then
  ok "kiro-cli's other run/ state and non-version names left alone"
else
  no "unrelated run/ entries" "the purge deleted state it does not own"
fi
[ -d "$RUN/chat-cli-9.9.9" ] && ok "a directory matching the copy name is left alone" \
  || no "directory entry" "chat-cli-9.9.9/ was removed"
[ -L "$RUN/chat-cli-8.8.8" ] && [ -L "$RUN/.chat-cli-8.8.8.heartbeat" ] && [ -f "$ROOT/bait" ] \
  && ok "symlinked entries and their target left alone" \
  || no "symlinked entry" "a symlink or its target was removed"
# rm -f on a directory fails rather than deleting it, so only the log shows
# whether a non-file was attempted at all.
[ ! -s "$WORK/purge.log" ] && ok "no removal was even attempted on a non-copy" \
  || no "skipped entries" "the purge logged: $(head -n 1 "$WORK/purge.log")"

# The symlink and containment guards are redundant against a symlink (realpath
# also defeats one), so each case asserts its own refusal line.
guard_said() {
  grep -q "$1" "$WORK/purge.log"
}

setup
VICTIM="$ROOT/victim"
mkdir -p "$VICTIM"
: >"$VICTIM/chat-cli-1.0.0"
rm -rf "$RUN"
ln -s "$VICTIM" "$RUN"
KIRO_SKIP_BINARY_PINNING=1 purge_logged
[ -f "$VICTIM/chat-cli-1.0.0" ] && guard_said 'is a symlink; refusing to purge' \
  && ok "symlinked run dir refused BY the symlink guard; the bait survived" \
  || no "symlinked run dir" "deleted through the symlink, or a different guard caught it"

setup
VICTIM="$ROOT/victim2"
mkdir -p "$VICTIM/run"
: >"$VICTIM/run/chat-cli-1.0.0"
rm -rf "$XDG_DATA_HOME/kiro-cli"
ln -s "$VICTIM" "$XDG_DATA_HOME/kiro-cli"
KIRO_SKIP_BINARY_PINNING=1 purge_logged
[ -f "$VICTIM/run/chat-cli-1.0.0" ] && guard_said 'is a symlink; refusing to purge' \
  && ok "symlinked data dir refused BY the symlink guard; the bait survived" \
  || no "symlinked data dir" "deleted through the symlink, or a different guard caught it"

# A non-canonical path with no symlink reaches only the containment guard.
setup
mkdir -p "$ROOT/real-share"
XDG_DATA_HOME="$ROOT/real-share/../real-share"
RUN="$XDG_DATA_HOME/kiro-cli/run"
mkdir -p "$RUN"
: >"$RUN/chat-cli-1.0.0"
KIRO_SKIP_BINARY_PINNING=1 purge_logged
[ -f "$RUN/chat-cli-1.0.0" ] && guard_said 'does not resolve inside the data dir' \
  && ok "non-canonical data-dir path refused BY the containment guard" \
  || no "containment guard" "purged a path realpath could not confirm, or a different guard caught it"

FUNCS="$WORK/funcs.sh"
cat "$WORK/logfmt_value.sh" "$WORK/kiro_cli_data_dir.sh" "$WORK/purge_pinned_chat_cli_copies.sh" >"$FUNCS"

# Empty stderr too: an unguarded $HOME trips set -u inside the substitution,
# which the caller's `|| return 0` would otherwise hide.
env -u HOME -u XDG_DATA_HOME KIRO_SKIP_BINARY_PINNING=1 \
  bash -euf -c '. "$1"; purge_pinned_chat_cli_copies' _ "$FUNCS" >/dev/null 2>"$WORK/purge.log"
rc=$?
[ "$rc" -eq 0 ] && [ ! -s "$WORK/purge.log" ] \
  && ok "unset HOME and XDG_DATA_HOME return 0 quietly under set -euf" \
  || no "unset HOME" "returned $rc, stderr: $(head -n 1 "$WORK/purge.log")"

setup
plant_pins
# -eu, not -euf: noglob would keep the loop from ever reaching rm.
XDG_DATA_HOME="$XDG_DATA_HOME" KIRO_SKIP_BINARY_PINNING=1 \
  bash -eu -c '. "$1"; rm() { return 1; }; purge_pinned_chat_cli_copies' _ "$FUNCS" \
  >/dev/null 2>"$WORK/purge.log"
rc=$?
[ "$rc" -eq 0 ] && grep -q 'msg="failed to remove unused kiro-cli pinned binary"' "$WORK/purge.log" \
  && ok "a failing rm warns and returns 0 under set -eu" \
  || no "failing rm" "returned $rc or logged no warn"

# A call above the definition is "command not found" at boot, and a call past
# the server exec never runs; either leaves the copies on the volume silently.
def_line=$(grep -n '^purge_pinned_chat_cli_copies() {' "$ENTRYPOINT" | head -1 | cut -d: -f1)
prune_line=$(grep -n '^prune_superseded_kas_runtimes ' "$ENTRYPOINT" | head -1 | cut -d: -f1)
call_line=$(grep -n '^purge_pinned_chat_cli_copies$' "$ENTRYPOINT" | head -1 | cut -d: -f1)
server_line=$(grep -nE '^[^#]*exec .*/(web-terminal-kiro|app)' "$ENTRYPOINT" | tail -1 | cut -d: -f1)
[ -n "$def_line" ] && [ -n "$prune_line" ] && [ -n "$call_line" ] && [ -n "$server_line" ] \
  && [ "$def_line" -lt "$prune_line" ] && [ "$prune_line" -lt "$call_line" ] \
  && [ "$call_line" -lt "$server_line" ] \
  && ok "the boot calls the purge after its definition and the KAS prune, before the server" \
  || no "purge wiring" \
    "def ${def_line:-none}, kas prune ${prune_line:-none}, call ${call_line:-none}, server ${server_line:-none}"

report
