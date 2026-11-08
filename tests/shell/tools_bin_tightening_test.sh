#!/usr/bin/env bash
# The tightening loop over the two link dirs that lead PATH, $TOOLS/kiro-cli-bin
# and $TOOLS/bin: no entry is exempt by name. A toolbelt link at kiro-cli, or a
# link a 2.x install left at kiro-cli-chat or kiro-cli-term, is checked and
# tightened like any other entry. The manager's own link needs no exemption: its
# target sits under kiro-cli-versions/, which an earlier loop tightens.
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
mkdir -p "$tools/kiro-cli-bin" "$tools/kiro-cli-versions/3.0.0"
: >"$tools/kiro-cli-versions/3.0.0/kiro-cli"
chmod 0775 "$tools/kiro-cli-versions/3.0.0/kiro-cli"
ln -s "$tools/kiro-cli-versions/3.0.0/kiro-cli" "$tools/kiro-cli-bin/kiro-cli"
run_loop "$tools"
[ "$(mode_of "$tools/kiro-cli-bin/kiro-cli")" = 755 ] \
  && ok "a group-writable target behind the manager's link dir is tightened" \
  || no "kiro-cli-bin tightening" "mode is $(mode_of "$tools/kiro-cli-bin/kiro-cli"), expected 755"

tools=$(mktemp -d "$WORK/tools.XXXXXX")
mkdir -p "$tools/kiro-cli-bin" "$tools/kiro-cli-versions/3.0.0"
: >"$tools/kiro-cli-versions/3.0.0/kiro-cli"
chmod 0755 "$tools/kiro-cli-versions/3.0.0/kiro-cli"
ln -s "$tools/kiro-cli-versions/3.0.0/kiro-cli" "$tools/kiro-cli-bin/kiro-cli"
run_loop "$tools"
[ "$(mode_of "$tools/kiro-cli-bin/kiro-cli")" = 755 ] && [ ! -s "$WORK/loop.log" ] \
  && ok "a manager link to an already tight version-dir target passes silently" \
  || no "manager link" "mode $(mode_of "$tools/kiro-cli-bin/kiro-cli"), log: $(cat "$WORK/loop.log")"

report
