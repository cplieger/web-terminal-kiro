#!/usr/bin/env bash
# log_has(), wait_for_log() and loaded_profile_blocker() from tests/image-test.sh,
# under its pipefail. Piped into grep -q, a match closes the pipe while the
# writer still writes and the pipeline reports SIGPIPE's 141: a present log line
# reads as missing, a forbidden one as absent, a loaded profile as not loaded.
# Every case puts the match first and over a megabyte after it.

# SC2015: lib.sh's ok/no return 0, so `cond && ok || no` cannot mis-fire.
# SC2034, SC2329: aa_public and the stubs are used by the extracted functions.
# shellcheck disable=SC2015,SC2034,SC2329
set -u
set -o pipefail

# shellcheck source-path=SCRIPTDIR
. "$(dirname -- "$0")/lib.sh"
new_workdir >/dev/null

# Overridable so a red-check can point this file at a mutated /tmp copy.
ENTRYPOINT="${IMAGE_TEST:-$REPO_ROOT/tests/image-test.sh}"
load_function log_has
load_function wait_for_log
load_function loaded_profile_blocker

aa_public=web-terminal-kiro-sandbox

# 1 MiB of filler, far past a pipe buffer, so a writer feeding an early-exiting
# reader is still writing when the reader closes the pipe.
filler="$WORK/filler"
awk 'BEGIN { for (i = 0; i < 16384; i++) printf "%063d\n", i }' >"$filler"
{
  printf 'level=WARN msg="kiro-cli sandbox unavailable" error="AppArmor confines the container as \\"docker-default (enforce)\\""\n'
  printf 'level=INFO msg="kiro-cli sandbox available" proc=tmpfs\n'
  cat "$filler"
} >"$WORK/big.log"
{
  printf '%s\n' 'web-terminal-kiro-sandbox (enforce)'
  cat "$filler"
} >"$WORK/loaded.profiles"
{
  printf '%s\n' 'web-terminal-kiro-sandbox-test-1 (enforce)' 'docker-default (enforce)'
  cat "$filler"
} >"$WORK/other.profiles"
for f in big.log loaded.profiles other.profiles; do
  [ "$(wc -c <"$WORK/$f")" -gt 1000000 ] || {
    printf 'harness error: fixture %s is not past 1 MB\n' "$f" >&2
    exit 1
  }
done

# fail is the suite's fatal exit; status 3 tells it apart from a plain miss.
fail() { exit 3; }
sleeps=0
# Self-limited, so a wait_for_log that never stops polling fails, not hangs.
sleep() {
  sleeps=$((sleeps + 1))
  [ "$sleeps" -le 40 ] || exit 4
}
docker() {
  case "$1 $2" in
    "logs big") cat "$WORK/big.log" ;;
    *) return 1 ;;
  esac
}

# --- 1. log_has: a match before a large log ----------------------------------
log_has big 'kiro-cli sandbox unavailable' && ok "a log line before 1 MiB of log is found" \
  || no "a log line before 1 MiB of log is found" "log_has returned $?"
log_has big 'kiro-cli sandbox unavailable' 'AppArmor confines the container as' 'docker-default (enforce)' \
  && ok "every text on one line is found" || no "every text on one line is found" "log_has returned $?"
if log_has big 'kiro-cli sandbox'; then
  ok "a forbidden line before 1 MiB of log is not taken for absent"
else
  no "a forbidden line before 1 MiB of log is not taken for absent" "log_has returned $?"
fi

# --- 2. log_has: misses and an unreadable log -------------------------------
log_has big 'kiro-cli sandbox unavailable' 'proc=tmpfs'
rc=$?
[ "$rc" -eq 1 ] && ok "texts on different lines do not match" \
  || no "texts on different lines do not match" "log_has returned $rc, want 1"
log_has big 'never logged'
rc=$?
[ "$rc" -eq 1 ] && ok "an absent text does not match" || no "an absent text does not match" "log_has returned $rc, want 1"
(log_has gone 'kiro-cli sandbox')
rc=$?
[ "$rc" -eq 3 ] && ok "an unreadable log fails the suite rather than reading as absent" \
  || no "an unreadable log fails the suite rather than reading as absent" "status $rc, want 3 from fail"

# --- 3. wait_for_log: a present line returns at once --------------------------
wait_for_log big 'kiro-cli sandbox available'
rc=$?
[ "$rc" -eq 0 ] && [ "$sleeps" -eq 0 ] && ok "wait_for_log finds a present line on its first read" \
  || no "wait_for_log finds a present line on its first read" "status $rc after $sleeps sleeps"

# --- 4. loaded_profile_blocker -------------------------------------------------
# sudo_list <file> makes sudo print file for the one command the suite runs.
sudo_list() {
  list=$1
  sudo() {
    [ "$*" = "-n cat /sys/kernel/security/apparmor/profiles" ] || return 99
    if [ -z "$list" ]; then
      return 1
    fi
    cat "$list"
  }
}
sudo_list "$WORK/loaded.profiles"
out=$(loaded_profile_blocker)
[ "$out" = "a host that has not loaded AppArmor profile web-terminal-kiro-sandbox" ] \
  && ok "a loaded profile listed first, before 1 MiB of list, blocks the suite" \
  || no "a loaded profile listed first, before 1 MiB of list, blocks the suite" "printed \"$out\""
sudo_list ""
out=$(loaded_profile_blocker)
[ "$out" = "passwordless sudo to read the loaded AppArmor profiles" ] \
  && ok "an unreadable profile list blocks the suite" || no "an unreadable profile list blocks the suite" "printed \"$out\""
sudo_list "$WORK/other.profiles"
out=$(loaded_profile_blocker)
[ -z "$out" ] && ok "other profiles, one named with the shipped name as a prefix, do not block" \
  || no "other profiles, one named with the shipped name as a prefix, do not block" "printed \"$out\""

report
