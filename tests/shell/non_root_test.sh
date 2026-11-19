#!/usr/bin/env bash
# A compose `user:` uid: the passwd entry ssh needs, and the boot paths that must
# degrade rather than stop. tests/image-smoke.sh runs as root, so none of these
# branches is reachable there.
# Lint directives, each against a stated guarantee:
#   SC2015 - `[ cond ] && ok || no` cannot mis-fire: lib.sh's ok/no return 0.
#   SC2016 - the call-site patterns are literal entrypoint text.
#   SC2329 - fatal() is invoked indirectly by the extracted functions.
# shellcheck disable=SC2015,SC2016,SC2329
set -u

# shellcheck source-path=SCRIPTDIR
. "$(dirname -- "$0")/lib.sh"
new_workdir >/dev/null

load_function logfmt_value
load_function provide_passwd_entry

fixture() {
  rm -rf "${WORK:?}/etc" "${WORK:?}/tmp"
  mkdir -p "$WORK/etc" "$WORK/tmp"
  printf '%s\n' 'root:x:0:0:root:/config/home:/bin/bash' \
    'daemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin' \
    'nobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin' >"$WORK/etc/passwd"
  printf '%s\n' 'root:x:0:' 'users:x:100:' 'nogroup:x:65534:' >"$WORK/etc/group"
  unset LD_PRELOAD NSS_WRAPPER_PASSWD NSS_WRAPPER_GROUP
}

run_provide() {
  out=$(provide_passwd_entry "$@" "$WORK/tmp" "$WORK/etc/passwd" "$WORK/etc/group" 2>&1)
  rc=$?
  # The exports are the contract, so read them from a second, uncaptured call.
  provide_passwd_entry "$@" "$WORK/tmp" "$WORK/etc/passwd" "$WORK/etc/group" 2>/dev/null
  take_preload
}

# Moves LD_PRELOAD into $preload: left exported, it would preload into every
# later command of this test, and the library is not installed outside the image.
take_preload() {
  preload=${LD_PRELOAD:-}
  unset LD_PRELOAD
}

lines_for_uid() { awk -F: -v u="$1" '$3 == u' "$2"; }

# --- 1. an entry already homed at $HOME needs nothing ---------------------------
fixture
run_provide 0 0 /config/home
[ "$rc" -eq 0 ] && [ -z "$out" ] && ok "root, already homed at /config/home, is silent" \
  || no "root is silent" "rc=$rc out=$out"
[ -z "$preload${NSS_WRAPPER_PASSWD:-}${NSS_WRAPPER_GROUP:-}" ] \
  && ok "root gets no nss_wrapper preload" \
  || no "root gets no preload" "LD_PRELOAD=$preload NSS_WRAPPER_PASSWD=${NSS_WRAPPER_PASSWD:-}"
[ -z "$(ls -A "$WORK/tmp")" ] && ok "root writes no overlay files" \
  || no "root writes no overlay files" "$(ls -A "$WORK/tmp")"

# --- 2. a uid with no entry gets one homed at $HOME ----------------------------
fixture
run_provide 1000 1000 /config/home
[ "$rc" -eq 0 ] && ok "a uid with no entry returns 0" || no "a uid with no entry returns 0" "rc=$rc"
case "$preload" in
  libnss_wrapper.so) ok "LD_PRELOAD names nss_wrapper by its bare soname" ;;
  *) no "LD_PRELOAD names nss_wrapper" "LD_PRELOAD=$preload" ;;
esac
pw=${NSS_WRAPPER_PASSWD:-}
gr=${NSS_WRAPPER_GROUP:-}
if [ -z "$pw" ] || [ ! -f "$pw" ] || [ -z "$gr" ] || [ ! -f "$gr" ]; then
  no "the overlay files exist" "NSS_WRAPPER_PASSWD=$pw NSS_WRAPPER_GROUP=$gr"
  report
  exit
fi
[ "$(lines_for_uid 1000 "$pw")" = 'kiro:x:1000:1000::/config/home:/bin/bash' ] \
  && ok "the passwd overlay holds the uid's entry, homed at /config/home" \
  || no "the passwd overlay holds the uid's entry" "got: $(lines_for_uid 1000 "$pw")"
[ "$(lines_for_uid 0 "$pw")" = 'root:x:0:0:root:/config/home:/bin/bash' ] \
  && [ "$(wc -l <"$pw")" -eq 4 ] \
  && ok "the passwd overlay keeps every existing entry" \
  || no "the passwd overlay keeps every existing entry" "$(cat "$pw")"
[ "$(lines_for_uid 1000 "$gr")" = 'kiro:x:1000:' ] && [ "$(wc -l <"$gr")" -eq 4 ] \
  && ok "the group overlay adds the missing gid and keeps the rest" \
  || no "the group overlay adds the missing gid" "$(cat "$gr")"
case "$pw" in
  "$WORK/tmp"/*) ok "the overlay lives under the given temp dir" ;;
  *) no "the overlay lives under the given temp dir" "path=$pw" ;;
esac
[ "$(stat -c '%a' "$(dirname -- "$pw")")" = 700 ] && ok "the overlay directory is private" \
  || no "the overlay directory is private" "mode=$(stat -c '%a' "$(dirname -- "$pw")")"
cmp -s "$WORK/etc/passwd" <(printf '%s\n' 'root:x:0:0:root:/config/home:/bin/bash' \
  'daemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin' \
  'nobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin') \
  && ok "the source passwd file is never written" \
  || no "the source passwd file is never written" "$(cat "$WORK/etc/passwd")"
case "$out" in
  *level=info*uid=1000*gid=1000*) ok "the overlay is logged at info with the uid and gid" ;;
  *) no "the overlay is logged at info with the uid and gid" "got: $out" ;;
esac

# --- 3. a uid whose entry points elsewhere is re-homed, not duplicated ---------
fixture
run_provide 65534 65534 /config/home
pw=${NSS_WRAPPER_PASSWD:-/dev/null}
gr=${NSS_WRAPPER_GROUP:-/dev/null}
[ "$(lines_for_uid 65534 "$pw")" = 'nobody:x:65534:65534:nobody:/config/home:/usr/sbin/nologin' ] \
  && ok "an existing entry keeps its name and fields, with home moved to /config/home" \
  || no "an existing entry is re-homed" "got: $(lines_for_uid 65534 "$pw")"
[ "$(wc -l <"$pw")" -eq 3 ] && ok "a re-homed entry is not duplicated" \
  || no "a re-homed entry is not duplicated" "$(cat "$pw")"
cmp -s "$gr" "$WORK/etc/group" && ok "an existing gid leaves the group overlay unchanged" \
  || no "an existing gid leaves the group overlay unchanged" "$(cat "$gr")"

# --- 4. an existing preload is kept --------------------------------------------
fixture
export LD_PRELOAD=/opt/other.so
provide_passwd_entry 1000 1000 /config/home "$WORK/tmp" "$WORK/etc/passwd" "$WORK/etc/group" 2>/dev/null
take_preload
[ "$preload" = 'libnss_wrapper.so:/opt/other.so' ] && ok "an operator's LD_PRELOAD is kept after nss_wrapper" \
  || no "an operator's LD_PRELOAD is kept" "LD_PRELOAD=$preload"

# --- 5. a HOME that cannot be a passwd field warns and exports nothing ----------
for bad in '/config/ho:me' $'/config/ho\nme'; do
  fixture
  run_provide 1000 1000 "$bad"
  [ "$rc" -eq 0 ] && [ -z "$preload${NSS_WRAPPER_PASSWD:-}" ] && [ -z "$(ls -A "$WORK/tmp")" ] \
    && ok "HOME $(printf '%q' "$bad") exports and writes nothing" \
    || no "HOME $(printf '%q' "$bad") exports and writes nothing" "rc=$rc LD_PRELOAD=$preload"
  case "$out" in
    *level=warn*'No user exists for uid'*) ok "HOME $(printf '%q' "$bad") warns with the ssh consequence" ;;
    *) no "HOME $(printf '%q' "$bad") warns with the ssh consequence" "got: $out" ;;
  esac
done

# --- 6. an unwritable temp dir degrades to a warning ---------------------------
fixture
out=$(provide_passwd_entry 1000 1000 /config/home "$WORK/absent" "$WORK/etc/passwd" "$WORK/etc/group" 2>&1)
rc=$?
[ "$rc" -eq 0 ] && ok "an unwritable temp dir still returns 0" || no "an unwritable temp dir returns 0" "rc=$rc"
case "$out" in
  *level=warn*'No user exists for uid'*) ok "an unwritable temp dir warns with the ssh consequence" ;;
  *) no "an unwritable temp dir warns with the ssh consequence" "got: $out" ;;
esac
provide_passwd_entry 1000 1000 /config/home "$WORK/absent" "$WORK/etc/passwd" "$WORK/etc/group" 2>/dev/null
take_preload
[ -z "$preload${NSS_WRAPPER_PASSWD:-}" ] && ok "an unwritable temp dir exports no preload" \
  || no "an unwritable temp dir exports no preload" "LD_PRELOAD=$preload"

# --- 7. call site: once HOME is proven, with the running identity --------------
call_line=$(grep -nE '^provide_passwd_entry "\$EUID" "\$\(id -g\)" "\$HOME"' "$ENTRYPOINT" | head -1 | cut -d: -f1)
home_line=$(grep -n "HOME does not resolve beneath the /config mount" "$ENTRYPOINT" | head -1 | cut -d: -f1)
def_line=$(grep -n '^provide_passwd_entry() {' "$ENTRYPOINT" | head -1 | cut -d: -f1)
[ -n "$call_line" ] && [ -n "$home_line" ] && [ "$call_line" -gt "$home_line" ] \
  && ok "the entry is written with the running uid and gid after HOME is proven beneath /config" \
  || no "the entry is written after HOME is proven" "call at ${call_line:-none}, HOME check at ${home_line:-none}"
[ -n "$def_line" ] && [ -n "$call_line" ] && [ "$def_line" -lt "$call_line" ] \
  && ok "provide_passwd_entry is defined before it is called" \
  || no "provide_passwd_entry is defined before it is called" "def at ${def_line:-none}, call at ${call_line:-none}"

# --- 8. an unwritable /config names the uid that could not write it -------------
fatal() {
  printf 'FATAL %s %s\n' "$1" "${2:-}"
  exit 1
}
load_function make_config_dir
: >"$WORK/file"
out=$( (make_config_dir "$WORK/file/sub") 2>&1)
case "$out" in
  *FATAL*"uid=$EUID"*) ok "a failed config mkdir names the running uid" ;;
  *) no "a failed config mkdir names the running uid" "got: $out" ;;
esac
probe_src=$(extract_range '^if ! probe=\$(mktemp "\$TOOLS' '^fi$' "$WORK/probe.sh") || exit 1
# The sourced range reads TOOLS, and its path is generated.
# shellcheck disable=SC1090,SC2034
out=$( (TOOLS="$WORK/absent/tools" && . "$probe_src") 2>&1)
case "$out" in
  *FATAL*"not writable"*"uid=$EUID"*) ok "a failed write probe names the running uid" ;;
  *) no "a failed write probe names the running uid" "got: $out" ;;
esac

# --- 9. a non-root boot degrades containment and never stops ------------------
# Runs the real re-exec block as a non-root uid: no stubbed mount or setpriv,
# because their refusals are what this path must survive.
nonroot=()
if [ "$EUID" -eq 0 ]; then
  nonroot=(setpriv --reuid=1000 --regid=1000 --clear-groups --)
fi
block_src=$(extract_range '^if \[ "\${CONTAINMENT_CAPS_DROPPED:-}" != "1" \]; then' '^fi$' "$WORK/block.sh") || exit 1
{
  printf '%s\n' 'set -u'
  cat "$WORK/logfmt_value.sh" "$(extract_function enable_session_containment "$WORK/esc.sh")"
  printf '%s\n' 'fatal() { printf "FATAL %s\n" "$1"; exit 1; }'
  cat "$block_src"
  printf '%s\n' 'printf "REACHED\n"'
} >"$WORK/boot.sh"
chmod 755 "$WORK"
chmod 644 "$WORK/boot.sh"
out=$(cd / && env -u CONTAINMENT_CAPS_DROPPED "${nonroot[@]}" bash "$WORK/boot.sh" 2>&1)
rc=$?
[ "$rc" -eq 0 ] && [ "${out##*$'\n'}" = REACHED ] && ok "a non-root boot passes the containment block" \
  || no "a non-root boot passes the containment block" "rc=$rc out=$out"
case "$out" in
  *'level=warn msg="cannot remount /sys/fs/cgroup rw'*) ok "a non-root boot warns that containment cannot engage" ;;
  *) no "a non-root boot warns that containment cannot engage" "got: $out" ;;
esac
case "$out" in
  *'CAP_SYS_ADMIN not held and containment unavailable'*) ok "a non-root boot warns it continues without containment" ;;
  *) no "a non-root boot warns it continues without containment" "got: $out" ;;
esac

# --- 10. the tools-tree checks hold for a non-root owner ----------------------
load_function secure_tools_dir
load_function harden_config_dir
mkdir -p "$WORK/own/tools" "$WORK/own/home"
chmod 775 "$WORK/own/tools"
chmod 755 "$WORK/own/home"
{
  printf '%s\n' 'set -u' 'fatal() { printf "FATAL %s\n" "$1"; exit 1; }' 'tools_tree_was_writable=0'
  cat "$WORK/secure_tools_dir.sh" "$WORK/harden_config_dir.sh"
  printf '%s\n' 'secure_tools_dir "$1/tools" && harden_config_dir "$1/home" && printf "REACHED\n"'
} >"$WORK/own.sh"
chmod 644 "$WORK/own.sh"
if [ "$EUID" -eq 0 ]; then
  chown -R 1000:1000 "$WORK/own"
fi
out=$("${nonroot[@]}" bash "$WORK/own.sh" "$WORK/own" 2>&1)
rc=$?
[ "$rc" -eq 0 ] && [ "${out##*$'\n'}" = REACHED ] && ok "a non-root owner passes the tools and config checks" \
  || no "a non-root owner passes the tools and config checks" "rc=$rc out=$out"
[ "$(stat -c '%a' "$WORK/own/tools")" = 755 ] && [ "$(stat -c '%a' "$WORK/own/home")" = 700 ] \
  && ok "a non-root owner's chmod tightens both trees" \
  || no "a non-root owner's chmod tightens both trees" "tools=$(stat -c '%a' "$WORK/own/tools") home=$(stat -c '%a' "$WORK/own/home")"

# --- 11. a restart replaces the earlier start's copies -------------------------
# The running uid owns what mktemp creates, so it is the uid passed in; a HOME
# other than the fixture's makes even root need copies.
fixture
mkdir "$WORK/tmp/keep.d"
run_provide "$EUID" "$(id -g)" /config/elsewhere
run_provide "$EUID" "$(id -g)" /config/elsewhere
copies=$(find "$WORK/tmp" -maxdepth 1 -name 'nss_wrapper.*' -type d | wc -l)
[ "$copies" -eq 1 ] && [ -d "${NSS_WRAPPER_PASSWD%/*}" ] \
  && ok "repeated starts leave one nss_wrapper directory, the current one" \
  || no "repeated starts leave one nss_wrapper directory" "count=$copies $(ls -A "$WORK/tmp")"
[ -d "$WORK/tmp/keep.d" ] && ok "the cleanup leaves other temp entries alone" \
  || no "the cleanup leaves other temp entries alone" "$(ls -A "$WORK/tmp")"

report
