#!/usr/bin/env bash
# The command sandbox on a built image: "available" with real sandboxed
# commands under the shipped profiles; "unavailable" under Docker's defaults
# (tabs find only the refusing bwrap), a renamed AppArmor profile, AppArmor off
# and Docker's default seccomp; "off" without KIRO_CLI_SANDBOX; and each added
# allowance red-checked. Needs Docker on an AppArmor host with passwordless sudo
# that has not loaded the shipped profile (the CI runner); elsewhere it prints
# SKIP, and under GITHUB_ACTIONS a missing prerequisite fails.
set -euo pipefail

image=${1:-ci-smoke:latest}
repo=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
seccomp="$repo/sandbox/seccomp.json"
aa_src="$repo/sandbox/web-terminal-kiro-sandbox"
aa_public=web-terminal-kiro-sandbox
# The startup check accepts only the shipped profile name, which is host-wide,
# so the suite refuses a host that already loads it rather than replace or
# unload a deployment's. Its copy under another name serves the rest.
aa_name="$aa_public-test-$$-$RANDOM"
shim=/opt/web-terminal-kiro/sandbox/bin/bwrap
refuser=/opt/web-terminal-kiro/sandbox/unavailable/bwrap

# loaded_profile_blocker prints why the suite must not load the shipped
# profile, or nothing. The list is captured, not piped into grep -q: under
# pipefail a match closes the pipe, cat dies of SIGPIPE (status 141) and a
# loaded profile reads as absent.
loaded_profile_blocker() {
  local loaded
  if ! loaded=$(sudo -n cat /sys/kernel/security/apparmor/profiles 2>/dev/null); then
    printf 'passwordless sudo to read the loaded AppArmor profiles\n'
  elif grep -q -- "^$aa_public (" <<<"$loaded"; then
    printf 'a host that has not loaded AppArmor profile %s\n' "$aa_public"
  fi
}

missing=""
docker info >/dev/null 2>&1 || missing="a Docker daemon"
if [ -z "$missing" ] && [ "$(cat /sys/module/apparmor/parameters/enabled 2>/dev/null || true)" != Y ]; then
  missing="an AppArmor-enabled kernel"
fi
if [ -z "$missing" ] && ! sudo -n apparmor_parser --version >/dev/null 2>&1; then
  missing="passwordless sudo apparmor_parser"
fi
if [ -z "$missing" ] && ! command -v jq >/dev/null 2>&1; then
  missing="jq"
fi
if [ -z "$missing" ]; then
  missing=$(loaded_profile_blocker)
fi
if [ -n "$missing" ]; then
  if [ "${GITHUB_ACTIONS:-}" = true ]; then
    printf 'FAIL: the sandbox image test needs %s\n' "$missing" >&2
    exit 1
  fi
  printf 'SKIP: the sandbox image test needs %s; it runs in CI (unprivileged bwrap cannot run in this environment)\n' "$missing"
  exit 0
fi

tmp=$(mktemp -d)
containers=()
profiles=()
ok=0

# shellcheck disable=SC2317,SC2329 # invoked by the EXIT trap
cleanup() {
  local c p
  if [ "$ok" -ne 1 ]; then
    for c in "${containers[@]}"; do
      printf -- '--- docker logs %s (tail) ---\n' "$c" >&2
      docker logs "$c" 2>&1 | tail -40 >&2 || true
    done
    printf -- '--- AppArmor denials (tail) ---\n' >&2
    sudo dmesg 2>/dev/null | grep 'apparmor="DENIED"' | tail -20 >&2 || true
  fi
  for c in "${containers[@]}"; do
    docker rm -f "$c" >/dev/null 2>&1 || true
  done
  for p in "${profiles[@]}"; do
    sudo apparmor_parser -R "$p" >/dev/null 2>&1 || true
  done
  rm -rf "$tmp"
}
trap cleanup EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

load_profile() {
  sudo apparmor_parser -r "$1" || fail "apparmor_parser could not load $1"
  profiles+=("$1")
}

# shape_args <block|allow> sets argv to the bwrap arguments kiro-cli builds for
# one command in its "block all" or "allow all" network mode (the server's
# startup check uses the block-all shape), with a PATH for the command.
argv=()
shape_args() {
  argv=(--ro-bind / / --tmpfs /tmp --bind /workspace /workspace --dev /dev
    --tmpfs /proc --unshare-pid --die-with-parent --new-session --tmpfs /run
    --clearenv --setenv PATH /usr/bin:/bin)
  if [ "$1" = block ]; then
    argv+=(--unshare-net)
  fi
}

profile_args=(--security-opt "seccomp=$seccomp" --security-opt "apparmor=$aa_public"
  --security-opt no-new-privileges)

# start_server <name> <docker run args...> starts the server.
start_server() {
  local name=$1
  shift
  containers+=("$name")
  docker run -d --name "$name" "$@" "$image" >/dev/null
}

# sandbox_state <name> prints the sandbox state /api/health reports. The 503
# body carries it too, so this does not wait for the kiro-cli download.
sandbox_state() {
  local state="" i
  for i in $(seq 1 60); do
    state=$(docker exec "$1" curl -s --max-time 2 http://127.0.0.1:9848/api/health 2>/dev/null \
      | jq -r '.sandbox // empty' 2>/dev/null || true)
    [ -n "$state" ] && break
    [ "$i" -lt 60 ] && sleep 1
  done
  printf '%s\n' "$state"
}

# log_has <container> <text>... succeeds when one line of the container's log
# holds every text. The log is captured for the same SIGPIPE reason as the
# profile list, which would fail a present line and pass a forbidden one.
log_has() {
  local lines needle
  lines=$(docker logs "$1" 2>&1) || fail "could not read the log of $1"
  shift
  for needle in "$@"; do
    lines=$(grep -F -- "$needle" <<<"$lines") || return 1
  done
}

wait_for_log() {
  local i
  for i in $(seq 1 20); do
    log_has "$1" "$2" && return 0
    [ "$i" -lt 20 ] && sleep 0.5
  done
  return 1
}

# tab_path <name> waits for kiro-cli, opens a tab and prints the PATH its
# process received. The engine marks every tab process TERM_PROGRAM=iTerm.app.
tab_path() {
  local i path="" opened=0
  # Session creation waits for the kiro-cli download and the tools sync.
  for i in $(seq 1 300); do
    if docker exec "$1" curl -sf --max-time 5 -X POST -o /dev/null http://127.0.0.1:9848/api/sessions; then
      opened=1
      break
    fi
    [ "$i" -lt 300 ] && sleep 1
  done
  [ "$opened" -eq 1 ] || fail "$1: POST /api/sessions was refused for 300s"
  for i in $(seq 1 20); do
    path=$(docker exec "$1" sh -c 'for e in /proc/[0-9]*/environ; do
      tr "\0" "\n" 2>/dev/null <"$e" | grep -qx TERM_PROGRAM=iTerm.app || continue
      tr "\0" "\n" 2>/dev/null <"$e" | sed -n "s/^PATH=//p"; exit 0
    done' || true)
    [ -n "$path" ] && break
    [ "$i" -lt 20 ] && sleep 0.5
  done
  [ -n "$path" ] || fail "$1: found no tab process to read its PATH from"
  printf '%s\n' "$path"
}

# in_container <container> <command...> runs a command under a shell that
# stays its parent, as kiro-cli stays bwrap's. docker exec starts a process as
# the child of `runc exec --detach`, which exits once it runs; bwrap's
# --die-with-parent SIGKILL (prctl(2) PR_SET_PDEATHSIG) then fires if bwrap set
# it first, a silent exit 137.
in_container() {
  local c=$1
  shift
  docker exec "$c" sh -c '"$@"; exit "$?"' sh "$@"
}

# sandboxed <container> <block|allow> <script> runs one command through
# the shim, the way kiro-cli would.
sandboxed() {
  shape_args "$2"
  in_container "$1" "$shim" "${argv[@]}" -- /bin/sh -c "$3"
}

aa_file="$tmp/$aa_name"
sed "s/\"$aa_public\"/\"$aa_name\"/g" "$aa_src" >"$aa_file"
grep -qF "\"$aa_public\"" "$aa_src" || fail "sandbox/$aa_public does not name profile \"$aa_public\""
if grep -qF "\"$aa_public\"" "$aa_file"; then
  fail "the test copy of the AppArmor profile still names \"$aa_public\""
fi
load_profile "$aa_src"
load_profile "$aa_file"

# ---- P: the shipped profiles and the switch ----------------------------------
p=smoke-web-terminal-kiro-sbx-P-$$
start_server "$p" "${profile_args[@]}" -e KIRO_CLI_SANDBOX=true
state=$(sandbox_state "$p")
[ "$state" = available ] || fail "under the shipped profiles /api/health reports sandbox \"$state\", want available"
wait_for_log "$p" 'kiro-cli sandbox available' || fail "no 'kiro-cli sandbox available' log line under the shipped profiles"
log=$(docker logs "$p" 2>&1) || fail "could not read the log of $p"
proc=$(sed -n '/kiro-cli sandbox available/{s/.*proc=\([a-z]*\).*/\1/p;q;}' <<<"$log")
# The profile refuses a fresh /proc, so kiro-cli's commands get an empty one.
[ "$proc" = tmpfs ] || fail "the available log line carries proc mode \"$proc\", want tmpfs"
[ "$(docker exec "$p" stat -c %a /run/user/0)" = 700 ] || fail "/run/user/0 is missing or not mode 700"
if docker exec "$p" sh -c 'command -v bwrap' >/dev/null 2>&1; then
  fail "bwrap resolves on the image PATH; kiro-cli must only find it through the checked shim"
fi
docker exec "$p" test ! -e /usr/bin/bwrap || fail "/usr/bin/bwrap exists; the package was not diverted"
version=$(docker exec "$p" /opt/web-terminal-kiro/libexec/bwrap --version) || fail "the diverted bwrap does not run"
case "$version" in
  "bubblewrap "*) : ;;
  *) fail "the diverted bwrap printed \"$version\", want its version" ;;
esac
docker exec "$p" socat -V >/dev/null || fail "socat does not run"

out=$(sandboxed "$p" block 'echo sandbox-ok; id -u') || fail "a sandboxed command failed under the shipped profiles (exit $?)"
[ "$out" = "sandbox-ok"$'\n'"0" ] || fail "sandboxed command printed \"$out\", want sandbox-ok and uid 0"
out=$(sandboxed "$p" block 'touch /etc/sbx-probe 2>/dev/null && echo etc-writable; touch /workspace/sbx-probe && echo workspace-ok') \
  || fail "the sandboxed write probe failed (exit $?)"
[ "$out" = workspace-ok ] || fail "sandboxed write probe printed \"$out\", want only workspace-ok (the system must stay read-only)"
docker exec "$p" test -f /workspace/sbx-probe || fail "a file the sandbox wrote to /workspace is not visible outside it"
docker exec "$p" test ! -e /etc/sbx-probe || fail "the sandbox wrote to /etc"
# kiro-cli hides credential folders under an empty tmpfs and credential files
# under /dev/null.
docker exec "$p" sh -c 'mkdir -p /config/home/.ssh && echo key >/config/home/.ssh/id && echo token >/config/home/.netrc'
shape_args block
argv+=(--tmpfs /config/home/.ssh --ro-bind /dev/null /config/home/.netrc)
out=$(in_container "$p" "$shim" "${argv[@]}" -- /bin/sh -c \
  'test -e /config/home/.ssh/id || echo ssh-hidden; test -s /config/home/.netrc || echo netrc-hidden') \
  || fail "a sandboxed command hiding credentials failed under the shipped profiles (exit $?)"
[ "$out" = "ssh-hidden"$'\n'"netrc-hidden" ] || fail "credential-hiding probe printed \"$out\", want ssh-hidden and netrc-hidden"
docker exec "$p" curl -s --max-time 2 -o /dev/null http://127.0.0.1:9848/api/health || fail "the server is unreachable outside the sandbox"
if sandboxed "$p" block 'curl -s --max-time 2 -o /dev/null http://127.0.0.1:9848/api/health'; then
  fail "a block-all sandboxed command reached the server; it must have its own network"
fi
sandboxed "$p" allow 'curl -s --max-time 2 -o /dev/null http://127.0.0.1:9848/api/health' \
  || fail "an allow-all sandboxed command could not reach the server (exit $?)"
tab=$(tab_path "$p")
case "$tab" in
  /opt/web-terminal-kiro/sandbox/bin:*) : ;;
  *) fail "a tab's PATH is \"$tab\", want the shim directory first" ;;
esac
# Each arm's server downloads kiro-cli in the background; stop it once checked.
docker rm -f "$p" >/dev/null

# ---- C: the shipped profiles with CAP_SYS_ADMIN granted ----------------------
# The entrypoint drops the capability before the server starts; the sandbox
# must still come up.
c=smoke-web-terminal-kiro-sbx-C-$$
start_server "$c" "${profile_args[@]}" --cap-add SYS_ADMIN -e KIRO_CLI_SANDBOX=true
state=$(sandbox_state "$c")
[ "$state" = available ] || fail "with cap_add SYS_ADMIN /api/health reports sandbox \"$state\", want available"
wait_for_log "$c" 'kiro-cli sandbox available' || fail "no 'kiro-cli sandbox available' log line with cap_add SYS_ADMIN"
# Without the shim's --unshare-pid, kiro-cli's probe would bind the
# container's /proc here and pick a fresh /proc no command can mount.
log_has "$c" 'kiro-cli sandbox available' 'proc=tmpfs' \
  || fail "with cap_add SYS_ADMIN the available log line does not carry proc=tmpfs"
docker rm -f "$c" >/dev/null

# ---- R, U and S: one of the shipped profiles missing ---------------------------
# A renamed copy of the profile runs the sandboxed command (the red-check
# control below), and with AppArmor off, as on a host without it, the seccomp
# file's mount rules are limited by nothing: both must stay unavailable. Under
# the shipped AppArmor profile with Docker's default seccomp profile, the check
# runs bwrap, whose own error must reach the warning.
r=smoke-web-terminal-kiro-sbx-R-$$
start_server "$r" --security-opt "seccomp=$seccomp" --security-opt "apparmor=$aa_name" \
  --security-opt no-new-privileges -e KIRO_CLI_SANDBOX=true
u=smoke-web-terminal-kiro-sbx-U-$$
start_server "$u" --security-opt "seccomp=$seccomp" --security-opt apparmor=unconfined \
  --security-opt no-new-privileges -e KIRO_CLI_SANDBOX=true
s=smoke-web-terminal-kiro-sbx-S-$$
start_server "$s" --security-opt "apparmor=$aa_public" --security-opt no-new-privileges -e KIRO_CLI_SANDBOX=true
for pair in "$r:$aa_name (enforce)" "$u:unconfined"; do
  arm=${pair%%:*}
  label=${pair#*:}
  state=$(sandbox_state "$arm")
  [ "$state" = unavailable ] || fail "$arm: under AppArmor \"$label\" /api/health reports sandbox \"$state\", want unavailable"
  wait_for_log "$arm" 'kiro-cli sandbox unavailable' || fail "$arm: no 'kiro-cli sandbox unavailable' warning under AppArmor \"$label\""
  log_has "$arm" 'kiro-cli sandbox unavailable' 'AppArmor confines the container as' "$label" \
    || fail "$arm: the unavailable warning does not name the AppArmor confinement \"$label\""
  docker rm -f "$arm" >/dev/null
done
state=$(sandbox_state "$s")
[ "$state" = unavailable ] || fail "under Docker's default seccomp profile /api/health reports sandbox \"$state\", want unavailable"
wait_for_log "$s" 'kiro-cli sandbox unavailable' || fail "no 'kiro-cli sandbox unavailable' warning under Docker's default seccomp profile"
log_has "$s" 'kiro-cli sandbox unavailable' 'output="bwrap: ' \
  || fail "under Docker's default seccomp profile the unavailable warning does not carry bwrap's own error"
docker rm -f "$s" >/dev/null

# ---- D: Docker's defaults and the switch -------------------------------------
# A bwrap already on PATH at start, and one installed after it, must both stay
# hidden behind the refusing one.
image_path=$(docker image inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$image" | sed -n 's/^PATH=//p')
[ -n "$image_path" ] || fail "could not read the image's PATH"
mkdir -p "$tmp/stray"
printf '#!/bin/sh\nexit 0\n' >"$tmp/stray/bwrap"
chmod 755 "$tmp/stray" "$tmp/stray/bwrap"
d=smoke-web-terminal-kiro-sbx-D-$$
start_server "$d" -e KIRO_CLI_SANDBOX=true -v "$tmp/stray:/opt/stray:ro" -e PATH="/opt/stray:$image_path"
state=$(sandbox_state "$d")
[ "$state" = unavailable ] || fail "under Docker's default profiles /api/health reports sandbox \"$state\", want unavailable"
wait_for_log "$d" 'kiro-cli sandbox unavailable' || fail "no 'kiro-cli sandbox unavailable' warning under Docker's defaults"
log_has "$d" 'kiro-cli sandbox unavailable' 'AppArmor confines the container as' 'docker-default (enforce)' \
  || fail "under Docker's defaults the unavailable warning does not name the AppArmor confinement \"docker-default (enforce)\""
if log_has "$d" 'a bwrap is on PATH while'; then
  fail "the server warned about a bwrap it already hides from tabs"
fi
[ "$(docker exec "$d" sh -c 'command -v bwrap')" = /opt/stray/bwrap ] \
  || fail "the stray bwrap is not on the container's PATH, so the next checks prove nothing"
tab=$(tab_path "$d")
case "$tab" in
  /opt/web-terminal-kiro/sandbox/unavailable:*) : ;;
  *) fail "a tab's PATH is \"$tab\", want the refusing bwrap's directory first" ;;
esac
docker exec "$d" sh -c 'mkdir -p /config/tools/bin && printf "#!/bin/sh\nexit 0\n" >/config/tools/bin/bwrap && chmod 755 /config/tools/bin/bwrap' \
  || fail "could not install a bwrap after start"
found=$(docker exec -e PATH="$tab" "$d" sh -c 'command -v bwrap' || true)
[ "$found" = "$refuser" ] || fail "a tab resolves bwrap to \"$found\", want $refuser although the startup check failed"
# kiro-cli reads exit 127 without "bwrap: execvp " as an unavailable sandbox.
rc=0
err=$(docker exec "$d" "$refuser" --ro-bind / / --proc /proc -- /bin/true 2>&1 >/dev/null) || rc=$?
[ "$rc" -eq 127 ] || fail "kiro-cli's probe through the refusing bwrap exited $rc, want 127"
case "$err" in
  *"bwrap: execvp "*) fail "the refusing bwrap printed \"$err\", which makes kiro-cli report its sandbox as on" ;;
  *"server log"*) : ;;
  *) fail "the refusing bwrap printed \"$err\", want its pointer to the server log" ;;
esac
docker rm -f "$d" >/dev/null

# ---- O: the profiles without the switch --------------------------------------
o=smoke-web-terminal-kiro-sbx-O-$$
start_server "$o" "${profile_args[@]}"
state=$(sandbox_state "$o")
[ "$state" = off ] || fail "without KIRO_CLI_SANDBOX /api/health reports sandbox \"$state\", want off"
wait_for_log "$o" 'web-terminal-kiro listening' || fail "the server never logged that it is listening"
if log_has "$o" 'kiro-cli sandbox'; then
  fail "the server logged a sandbox verdict with the sandbox off"
fi
docker rm -f "$o" >/dev/null

# ---- red-checks: every added allowance is needed -----------------------------
# Each runs the block-all /bin/true (allow-all for its own clone rule) with one
# allowance removed and must fail. The control is the same run unmodified.
oneshot() {
  shape_args "$1"
  shift
  timeout 60 docker run --rm --entrypoint "$shim" "$@" "$image" "${argv[@]}" -- /bin/true >/dev/null 2>&1
}
seccomp_without() {
  jq "$1" "$seccomp" >"$tmp/$2.json"
  printf '%s\n' "$tmp/$2.json"
}

oneshot block "${profile_args[@]}" || fail "control: a sandboxed /bin/true failed under the shipped profiles"
oneshot allow "${profile_args[@]}" || fail "control: an allow-all sandboxed /bin/true failed under the shipped profiles"
# The AppArmor red-checks below each remove a rule from this renamed copy.
oneshot block --security-opt "seccomp=$seccomp" --security-opt "apparmor=$aa_name" --security-opt no-new-privileges \
  || fail "control: a sandboxed /bin/true failed under the renamed copy of the AppArmor profile"
checked=0

redcheck_seccomp() {
  local label=$1 filter=$2 shape=$3 file
  file=$(seccomp_without "$filter" "$label")
  cmp -s "$file" "$seccomp" && fail "red-check $label: the jq filter changed nothing"
  if oneshot "$shape" --security-opt "seccomp=$file" --security-opt "apparmor=$aa_public" --security-opt no-new-privileges; then
    fail "allowance $label is not needed: remove it from sandbox/seccomp.json"
  fi
  checked=$((checked + 1))
}
redcheck_seccomp clone-block-all 'del(.syscalls[] | select((.args // [])[0].valueTwo == 1879179264))' block
redcheck_seccomp clone-allow-all 'del(.syscalls[] | select((.args // [])[0].valueTwo == 805437440))' allow
for call in mount umount2 pivot_root; do
  redcheck_seccomp "$call" "(.syscalls[] | select(.names == [\"mount\",\"pivot_root\",\"umount2\"]) | .names) -= [\"$call\"]" block
done

if oneshot block --security-opt "apparmor=$aa_public" --security-opt no-new-privileges; then
  fail "a sandboxed command ran under Docker's default seccomp profile; the seccomp file is not needed"
fi
if oneshot block --security-opt "seccomp=$seccomp" --security-opt no-new-privileges; then
  fail "a sandboxed command ran under Docker's default AppArmor profile; the AppArmor profile is not needed"
fi
checked=$((checked + 2))

n=0
while IFS= read -r rule; do
  n=$((n + 1))
  variant="$tmp/redcheck-$n"
  name="$aa_name-redcheck-$n"
  grep -vxF -- "  $rule" "$aa_file" | sed "s/\"$aa_name\"/\"$name\"/g" >"$variant"
  cmp -s <(grep -vF -- "$rule" "$variant") "$variant" || fail "red-check: could not remove AppArmor rule \"$rule\""
  load_profile "$variant"
  if oneshot block --security-opt "seccomp=$seccomp" --security-opt "apparmor=$name" --security-opt no-new-privileges; then
    fail "allowance \"$rule\" is not needed: remove it from sandbox/$aa_public"
  fi
  checked=$((checked + 1))
done < <(grep -E '^  (mount |remount |pivot_root,)' "$aa_src" | sed 's/^  //')
[ "$n" -eq 7 ] || fail "red-checked $n AppArmor allowances, want the 7 the profile adds"

ok=1
printf 'web-terminal-kiro payload: sandbox available under the shipped profiles (proc=%s, credential folders and files hidden) with and without CAP_SYS_ADMIN and a tab finds the shim first, unavailable under Docker defaults with tabs resolving only the refusing bwrap (exit 127) over strays from before and after start, unavailable under a renamed AppArmor profile, with AppArmor off, and with the bwrap error logged under the default seccomp profile, off without KIRO_CLI_SANDBOX; %s allowances red-checked\n' "$proc" "$checked"
printf 'web-terminal-kiro image test: ok\n'
