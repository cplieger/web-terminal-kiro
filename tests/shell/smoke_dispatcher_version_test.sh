#!/usr/bin/env bash
# wt_reports_pin() from tests/image-smoke.conf: the image smoke's proof that each
# required dispatcher in the pinned version directory runs and reports the pin.
# The suite runs only in CI against a built image, so this pins the matching
# alone: both names the chat sidecar has printed pass, and a wrong version, a
# near-miss version, the other dispatcher's name or empty output fails.

# SC2015: lib.sh's ok/no return 0, so `cond && ok || no` cannot mis-fire.
# shellcheck disable=SC2015
set -u

# shellcheck source-path=SCRIPTDIR
. "$(dirname -- "$0")/lib.sh"
new_workdir >/dev/null

# Overridable so a red-check can point this file at a mutated /tmp copy.
ENTRYPOINT="${SMOKE_CONF:-$REPO_ROOT/tests/image-smoke.conf}"
load_function wt_reports_pin

accepts() {
  wt_reports_pin "$1" "$2" "$3" && ok "$1 at $2 accepts \"$3\"" \
    || no "$1 at $2 accepts \"$3\"" "refused"
}
refuses() {
  wt_reports_pin "$1" "$2" "$3" && no "$1 at $2 refuses \"$3\"" "accepted" \
    || ok "$1 at $2 refuses \"$3\""
}

# The lines the real archives print (2.32.0 and 2.37.0, x86_64).
accepts kiro-cli 2.37.0 'kiro-cli 2.37.0'
accepts kiro-cli-chat 2.37.0 'kiro-cli 2.37.0'
accepts kiro-cli-chat 2.32.0 'kiro-cli-chat 2.32.0'

refuses kiro-cli-chat 2.37.0 'kiro-cli 2.36.0'
refuses kiro-cli-chat 2.37.0 'kiro-cli-chat 2.36.0'
refuses kiro-cli-chat 2.37.0 'kiro-cli 2.37.01'
refuses kiro-cli-chat 2.37.0 'kiro-cli 12.37.0'
refuses kiro-cli-chat 2.37.0 'kiro-cli 2.37.0-rc1'
refuses kiro-cli-chat 2.37.0 ''
refuses kiro-cli-chat 2.37.0 'figterm 2.37.0'
# A dot in the pin is literal, not a wildcard.
refuses kiro-cli-chat 2.37.0 'kiro-cli 2x37y0'
# Only the sidecar may answer under the main dispatcher's name, not the reverse.
refuses kiro-cli 2.37.0 'kiro-cli-chat 2.37.0'
refuses kiro-cli 2.37.0 'kiro-cli 2.36.0'
refuses kiro-cli 2.37.0 ''

report
