#!/usr/bin/env bash
# The $TOOLS/bin tightening loop: no entry is exempt by name.
#
# The install manager yields to a foreign entry at kiro-cli and kiro-cli-chat
# instead of replacing it, so a toolbelt link at either name can survive a boot
# and lead PATH. Its target must be checked and tightened like any other bin
# entry. The manager's own links need no exemption: their targets sit under
# kiro-cli-versions/, which an earlier loop tightens.
#
# Lint directives for this whole file:
#   SC2015 - `[ cond ] && ok "..." || no "..."` cannot mis-fire, because lib.sh's
#     ok/no return 0 unconditionally by design.
#   SC2034 - TOOLS is an INPUT to the extracted loop, which is sourced at runtime.
# shellcheck disable=SC2015,SC2034
set -u

# shellcheck source-path=SCRIPTDIR
. "$(dirname -- "$0")/lib.sh"
new_workdir >/dev/null

load_function logfmt_value
loop=$(extract_range '^tightened_tool_bins=0$' '^done$' "$WORK/loop.sh") || exit 1

mode_of() { stat -Lc '%a' "$1"; }

run_loop() {
  TOOLS=$1
  # shellcheck disable=SC1090
  . "$loop" 2>"$WORK/loop.log"
}

for name in kiro-cli kiro-cli-chat kiro-cli-term other-tool; do
  tools=$(mktemp -d "$WORK/tools.XXXXXX")
  mkdir -p "$tools/bin" "$tools/opt/foreign/1.0"
  : >"$tools/opt/foreign/1.0/$name"
  chmod 0775 "$tools/opt/foreign/1.0/$name"
  ln -s "$tools/opt/foreign/1.0/$name" "$tools/bin/$name"
  [ "$(mode_of "$tools/bin/$name")" = 775 ] || {
    printf 'harness error: precondition, bait for %s is not group-writable\n' "$name" >&2
    exit 1
  }
  run_loop "$tools"
  [ "$(mode_of "$tools/bin/$name")" = 755 ] \
    && ok "a group-writable foreign bin/$name target is tightened" \
    || no "bin/$name tightening" "mode is $(mode_of "$tools/bin/$name"), expected 755"
done

tools=$(mktemp -d "$WORK/tools.XXXXXX")
mkdir -p "$tools/bin" "$tools/kiro-cli-versions/2.0.0"
: >"$tools/kiro-cli-versions/2.0.0/kiro-cli-chat"
chmod 0755 "$tools/kiro-cli-versions/2.0.0/kiro-cli-chat"
ln -s "$tools/kiro-cli-versions/2.0.0/kiro-cli-chat" "$tools/bin/kiro-cli-chat"
run_loop "$tools"
[ "$(mode_of "$tools/bin/kiro-cli-chat")" = 755 ] && [ ! -s "$WORK/loop.log" ] \
  && ok "a manager link to an already tight version-dir target passes silently" \
  || no "manager link" "mode $(mode_of "$tools/bin/kiro-cli-chat"), log: $(cat "$WORK/loop.log")"

report
