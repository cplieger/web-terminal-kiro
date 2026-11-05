#!/usr/bin/env bash
# wt_reports_pin() from tests/image-smoke.conf: the image smoke's proof that the
# kiro-cli in the pinned version directory runs and reports the pin. The suite
# runs only in CI against a built image, so this pins the matching alone: the
# exact line passes, and a wrong version, a near-miss version, another binary's
# name or empty output fails.

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
  wt_reports_pin "$1" "$2" && ok "$1 accepts \"$2\"" \
    || no "$1 accepts \"$2\"" "refused"
}
refuses() {
  wt_reports_pin "$1" "$2" && no "$1 refuses \"$2\"" "accepted" \
    || ok "$1 refuses \"$2\""
}

# The line the real 3.0.0 x86_64 archive prints.
accepts 3.0.0 'kiro-cli 3.0.0'

refuses 3.0.0 'kiro-cli 2.37.1'
refuses 3.0.0 'kiro-cli 3.0.01'
refuses 3.0.0 'kiro-cli 13.0.0'
refuses 3.0.0 'kiro-cli 3.0.0-rc1'
refuses 3.0.0 ''
refuses 3.0.0 'figterm 3.0.0'
# The retired sidecar's name does not stand in for kiro-cli.
refuses 3.0.0 'kiro-cli-chat 3.0.0'
# A dot in the pin is literal, not a wildcard.
refuses 3.0.0 'kiro-cli 3x0y0'

report
